// Copyright (C) 2019 Nicola Murino
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package photoindex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/drakkan/sftpgo/v2/internal/logger"
)

const (
	exiftoolTimeout = 60 * time.Second
	vipsTimeout     = 3 * time.Minute
	previewQuality  = 82
)

// resolveTool returns the configured tool path if usable, or looks up name in
// the PATH. It returns an empty string if the tool is not available.
func resolveTool(configured, name string) string {
	if configured != "" {
		if _, err := os.Stat(configured); err != nil {
			logger.Warn(logSender, "", "configured %s path %q is not usable: %v", name, configured, err)
			return ""
		}
		return configured
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// exiftool wraps a long-running "exiftool -stay_open" process, so the (slow on
// a Raspberry Pi) Perl startup cost is paid once instead of once per photo.
// It is safe for concurrent use; requests are serialized.
type exiftool struct {
	path string

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	seq    int
}

func newExiftool(path string) *exiftool {
	return &exiftool{path: path}
}

func (e *exiftool) start() error {
	cmd := exec.Command(e.path, "-stay_open", "True", "-@", "-")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	lowerPriority(cmd.Process.Pid)
	e.cmd = cmd
	e.stdin = stdin
	e.stdout = bufio.NewReaderSize(stdout, 64*1024)
	return nil
}

// stopLocked terminates the exiftool process. e.mu must be held.
func (e *exiftool) stopLocked() {
	if e.cmd == nil {
		return
	}
	io.WriteString(e.stdin, "-stay_open\nFalse\n") //nolint:errcheck
	e.stdin.Close()
	done := make(chan struct{})
	go func() {
		e.cmd.Wait() //nolint:errcheck
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		e.cmd.Process.Kill() //nolint:errcheck
		<-done
	}
	e.cmd = nil
}

func (e *exiftool) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopLocked()
}

// read returns the metadata fields exiftool reports for the file at fsPath.
func (e *exiftool) read(fsPath string, kind string) (exifFields, error) {
	if strings.ContainsAny(fsPath, "\r\n") {
		// Arguments are newline separated, such names cannot be passed safely.
		return nil, errors.New("unsupported file name")
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.cmd == nil {
		if err := e.start(); err != nil {
			return nil, fmt.Errorf("unable to start exiftool: %w", err)
		}
	}
	e.seq++
	marker := "{ready" + strconv.Itoa(e.seq) + "}"
	if _, err := io.WriteString(e.stdin, exiftoolArgs(fsPath, kind, e.seq)); err != nil {
		e.stopLocked()
		return nil, fmt.Errorf("unable to write to exiftool: %w", err)
	}
	output, err := e.readUntil(marker)
	if err != nil {
		return nil, err
	}
	return parseExiftoolOutput(output)
}

// exiftoolArgs returns the "-stay_open" arguments, one per line, to read the
// metadata of fsPath. The request is terminated by "-execute<seq>".
func exiftoolArgs(fsPath, kind string, seq int) string {
	var args strings.Builder
	args.WriteString("-j\n-n\n-G0\n-a\n-api\nLargeFileSupport=1\n-charset\nfilename=utf8\n")
	if kind != KindVideo {
		// Don't scan JPEGs to the end looking for trailers. Not used for videos
		// since camera MP4s often store their metadata after the media data.
		args.WriteString("-fast\n")
	}
	for _, tag := range exiftoolTags {
		args.WriteString(tag + "\n")
	}
	args.WriteString(fsPath + "\n")
	args.WriteString("-execute" + strconv.Itoa(seq) + "\n")
	return args.String()
}

// readUntil reads the exiftool output up to the marker line. On error or
// timeout the process is stopped and restarted on the next request. e.mu must
// be held.
func (e *exiftool) readUntil(marker string) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var buf bytes.Buffer
		for {
			line, err := e.stdout.ReadString('\n')
			if strings.TrimSpace(line) == marker {
				ch <- result{data: buf.Bytes()}
				return
			}
			buf.WriteString(line)
			if err != nil {
				ch <- result{err: err}
				return
			}
		}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			e.stopLocked()
			return nil, fmt.Errorf("exiftool failed: %w", res.err)
		}
		return res.data, nil
	case <-time.After(exiftoolTimeout):
		// Killing the process unblocks the reader goroutine.
		e.cmd.Process.Kill() //nolint:errcheck
		<-ch
		e.stopLocked()
		return nil, errors.New("exiftool timeout")
	}
}

func parseExiftoolOutput(output []byte) (exifFields, error) {
	data := bytes.TrimSpace(output)
	if len(data) == 0 {
		return nil, errors.New("no metadata")
	}
	var out []exifFields
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("unable to parse exiftool output: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("no metadata")
	}
	return out[0], nil
}

// extractRawPreview writes the largest JPEG embedded in a RAW file to dst.
// RAW sensor data cannot be decoded by libvips, but every RAW format embeds a
// camera-rendered JPEG, which is what we want for a preview anyway.
func extractRawPreview(exiftoolPath, fsPath, dst string) error {
	for _, tag := range []string{"-JpgFromRaw", "-PreviewImage", "-OtherImage"} {
		ctx, cancel := context.WithTimeout(context.Background(), exiftoolTimeout)
		cmd := exec.CommandContext(ctx, exiftoolPath, "-b", tag, "--", fsPath)
		out, err := cmd.Output()
		cancel()
		if err == nil && len(out) > 1024 && out[0] == 0xFF && out[1] == 0xD8 {
			return os.WriteFile(dst, out, 0600)
		}
	}
	return errors.New("no embedded preview found")
}

// makePreview uses vipsthumbnail to write a JPEG to dst whose longest edge is
// at most maxEdge pixels. vipsthumbnail decodes only what it needs (JPEG
// shrink-on-load), honors the EXIF orientation and reads HEIC, PNG, WebP,
// TIFF and more, depending on how libvips was built.
func makePreview(vipsPath, src, dst string, maxEdge int) error {
	if vipsPath == "" {
		return errors.New("vipsthumbnail not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), vipsTimeout)
	defer cancel()

	size := fmt.Sprintf("%dx%d>", maxEdge, maxEdge) // ">" means only shrink
	output := fmt.Sprintf("%s[Q=%d,strip,optimize_coding]", dst, previewQuality)
	cmd := exec.CommandContext(ctx, vipsPath, src, "--size", size, "--export-profile", "srgb", "-o", output)
	cmd.Env = append(os.Environ(), "VIPS_CONCURRENCY=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	lowerPriority(cmd.Process.Pid)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("vipsthumbnail failed: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	if info, err := os.Stat(dst); err != nil || info.Size() == 0 {
		return errors.New("vipsthumbnail produced no output")
	}
	return nil
}

// RenderJPEG renders the image at fsPath (any format supported by the
// available tools, including HEIC and RAW) as a JPEG whose longest edge is at
// most maxEdge pixels and returns its bytes. It is used by the WebClient for
// thumbnails of files not yet indexed.
func (m *Manager) RenderJPEG(fsPath string, maxEdge int) ([]byte, error) {
	if m == nil || m.vipsPath == "" {
		return nil, errors.New("vipsthumbnail not available")
	}
	tmpDir, err := os.MkdirTemp(m.dataDir, "render-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	src := fsPath
	if KindForName(fsPath) == KindRaw {
		if m.exiftoolPath == "" {
			return nil, errors.New("exiftool not available")
		}
		src = filepath.Join(tmpDir, "raw.jpg")
		if err := extractRawPreview(m.exiftoolPath, fsPath, src); err != nil {
			return nil, err
		}
	}
	dst := filepath.Join(tmpDir, "out.jpg")
	if err := makePreview(m.vipsPath, src, dst, maxEdge); err != nil {
		return nil, err
	}
	return os.ReadFile(dst)
}
