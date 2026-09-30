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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// BenchmarkOptions configures Benchmark.
type BenchmarkOptions struct {
	Dir          string
	Samples      int
	PreviewSize  int
	ExiftoolPath string
	VipsPath     string
}

type benchStats struct {
	files   int
	bytes   int64
	meta    []time.Duration
	preview []time.Duration
	errors  int
}

func (s *benchStats) total() time.Duration {
	var t time.Duration
	for _, d := range s.meta {
		t += d
	}
	for _, d := range s.preview {
		t += d
	}
	return t
}

func median(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	v := slices.Clone(values)
	slices.Sort(v)
	return v[len(v)/2]
}

// Benchmark runs the indexing pipeline (metadata + preview) on a sample of the
// media files found under opts.Dir, without touching any index, and prints the
// time per file by format and an estimate for the whole directory. Previews
// are written to a temporary directory that is removed afterwards.
func Benchmark(opts BenchmarkOptions, out io.Writer) error {
	if opts.Samples <= 0 {
		opts.Samples = 50
	}
	if opts.PreviewSize <= 0 {
		opts.PreviewSize = 1024
	}
	b := &bench{
		exiftoolPath: resolveTool(opts.ExiftoolPath, "exiftool"),
		vipsPath:     resolveTool(opts.VipsPath, "vipsthumbnail"),
		previewSize:  opts.PreviewSize,
		stats:        make(map[string]*benchStats),
	}
	fmt.Fprintf(out, "exiftool: %s\nvipsthumbnail: %s\n", toolOrMissing(b.exiftoolPath), toolOrMissing(b.vipsPath))

	start := time.Now()
	all := collectMedia(opts.Dir)
	fmt.Fprintf(out, "found %d photos/videos under %q in %s\n", len(all), opts.Dir, time.Since(start).Round(time.Millisecond))
	if len(all) == 0 {
		return errors.New("no media files found")
	}
	samples := sampleEvenly(all, opts.Samples)

	var err error
	b.tmpDir, err = os.MkdirTemp("", "sftpgo-photoindex-bench-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(b.tmpDir)
	if b.exiftoolPath != "" {
		b.exif = newExiftool(b.exiftoolPath)
		defer b.exif.close()
	}

	fmt.Fprintf(out, "processing %d sample files, cold disk caches make the first files slower...\n", len(samples))
	for i, p := range samples {
		b.run(i, p)
	}
	return b.report(out, len(all))
}

type bench struct {
	exiftoolPath string
	vipsPath     string
	previewSize  int
	tmpDir       string
	exif         *exiftool
	stats        map[string]*benchStats
}

// collectMedia returns the media files under dir, skipping the same
// directories as the indexer.
func collectMedia(dir string) []string {
	var all []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error { //nolint:errcheck
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && KindForName(p) != "" {
			all = append(all, p)
		}
		return nil
	})
	return all
}

// sampleEvenly picks n files spread across the tree so every folder and
// format is represented.
func sampleEvenly(all []string, n int) []string {
	if len(all) <= n {
		return all
	}
	samples := make([]string, 0, n)
	step := float64(len(all)) / float64(n)
	for i := range n {
		samples = append(samples, all[int(float64(i)*step)])
	}
	return samples
}

func (b *bench) statsFor(p string) *benchStats {
	ext := strings.ToLower(filepath.Ext(p))
	if b.stats[ext] == nil {
		b.stats[ext] = &benchStats{}
	}
	return b.stats[ext]
}

func (b *bench) run(i int, p string) {
	st := b.statsFor(p)
	info, err := os.Stat(p)
	if err != nil {
		st.errors++
		return
	}
	st.files++
	st.bytes += info.Size()
	kind := KindForName(p)
	if b.exif != nil {
		t := time.Now()
		if _, err := b.exif.read(p, kind); err != nil {
			st.errors++
		}
		st.meta = append(st.meta, time.Since(t))
	}
	if b.vipsPath == "" || kind == KindVideo {
		return
	}
	t := time.Now()
	if err := b.preview(i, p, kind); err != nil {
		st.errors++
	}
	st.preview = append(st.preview, time.Since(t))
}

func (b *bench) preview(i int, p, kind string) error {
	src := p
	if kind == KindRaw {
		if b.exiftoolPath == "" {
			return errors.New("exiftool not available")
		}
		src = filepath.Join(b.tmpDir, "raw.jpg")
		if err := extractRawPreview(b.exiftoolPath, p, src); err != nil {
			return err
		}
	}
	dst := filepath.Join(b.tmpDir, fmt.Sprintf("%d.jpg", i))
	defer os.Remove(dst)
	return makePreview(b.vipsPath, src, dst, b.previewSize)
}

func (b *bench) report(out io.Writer, totalFiles int) error {
	exts := make([]string, 0, len(b.stats))
	for ext := range b.stats {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "format\tfiles\tavg size\tmetadata (median)\tpreview (median)\terrors\t")
	var total time.Duration
	var processed int
	for _, ext := range exts {
		st := b.stats[ext]
		avgSize := "-"
		if st.files > 0 {
			avgSize = fmt.Sprintf("%.1f MB", float64(st.bytes)/float64(st.files)/1024/1024)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%d\t\n", ext, st.files, avgSize,
			median(st.meta).Round(time.Millisecond), median(st.preview).Round(time.Millisecond), st.errors)
		total += st.total()
		processed += st.files
	}
	tw.Flush()
	if processed == 0 {
		return errors.New("no sample could be processed")
	}
	perFile := total / time.Duration(processed)
	estimate := perFile * time.Duration(totalFiles)
	fmt.Fprintf(out, "\naverage %s per file: a first full pass over %d files would take about %s with 1 worker\n",
		perFile.Round(time.Millisecond), totalFiles, estimate.Round(time.Minute))
	return nil
}

func toolOrMissing(p string) string {
	if p == "" {
		return "not found"
	}
	return p
}
