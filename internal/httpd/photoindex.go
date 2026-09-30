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

package httpd

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/go-chi/render"

	"github.com/drakkan/sftpgo/v2/internal/common"
	"github.com/drakkan/sftpgo/v2/internal/jwt"
	"github.com/drakkan/sftpgo/v2/internal/logger"
	"github.com/drakkan/sftpgo/v2/internal/photoindex"
	"github.com/drakkan/sftpgo/v2/internal/vfs"
)

const (
	i18nPhotoQueryInvalid = "fs.search.photo_query_invalid"
	i18nPhotoIndexOff     = "fs.search.photo_index_disabled"
	// thumbnailSizePreview is the value of the "size" query parameter asking
	// the thumbnail endpoint for the large preview, used by the viewer for
	// formats browsers cannot display (HEIC, TIFF, RAW).
	thumbnailSizePreview = "preview"
)

// initPhotoIndex starts the photo index (fork feature, see FORK_FEATURES.md)
// and connects it to the filesystem events. Failures are logged and leave the
// index disabled: the rest of SFTPGo never depends on it.
func initPhotoIndex(cfg photoindex.Config, configDir string) {
	if err := photoindex.Initialize(cfg, configDir, nil); err != nil {
		return
	}
	common.SetFsEventHook(func(operation, fsPath, fsTarget string) {
		if m := photoindex.Get(); m != nil {
			m.OnFsEvent(operation, fsPath, fsTarget)
		}
	})
}

// photoStatusURLForUser returns the photo index status endpoint if the index
// is enabled, or an empty string so the WebClient hides the photo features.
func photoStatusURLForUser() string {
	if photoindex.Get() == nil {
		return ""
	}
	return webClientPhotoStatusPath
}

// searchPhotoIndex answers a WebClient search containing photo filters, such as
// "taken:2023", from the photo index instead of walking the directories. Every
// hit is mapped back to the virtual path seen by the user and checked against
// the same permissions and file patterns as a directory listing.
func searchPhotoIndex(w http.ResponseWriter, r *http.Request, connection *Connection, m *photoindex.Manager,
	startDir string, q photoindex.Query,
) {
	scope := photoindex.NewUserScope(&connection.User, startDir)
	text := strings.TrimSpace(q.Text)
	textLower := strings.ToLower(text)
	results := make([]map[string]any, 0)
	truncated := false

	err := m.Search(scope.Dirs, q, func(rec photoindex.Media) bool {
		vPath, ok := scope.VirtualPath(rec.Path)
		if !ok || !scope.Visible(vPath) {
			return true
		}
		name := path.Base(vPath)
		if text != "" && !matchesSearchQuery(text, textLower, name) {
			return true
		}
		dir := path.Dir(vPath)
		res := make(map[string]any)
		res["id"] = len(results) + 1
		res["url"] = getFileObjectURL(dir, name, webClientFilesPath)
		res["type"] = "2"
		res["size"] = rec.Size
		res["meta"] = fmt.Sprintf("2_%v", name)
		res["name"] = name
		res["last_modified"] = getFileObjectModTime(time.Unix(0, rec.ModTime))
		res["path"] = dir
		// Extra fields (photo search only). "taken" is the local wall clock
		// time at which the photo was taken, deliberately without a zone.
		res["taken"] = rec.TakenTime().Format("2006-01-02T15:04:05")
		res["taken_src"] = rec.TakenSrc
		results = append(results, res)
		if len(results) >= maxSearchResults {
			truncated = true
			return false
		}
		return true
	})
	if err != nil {
		connection.Log(logger.LevelError, "photo search for %q under %q failed: %v", q.Text, startDir, err)
		sendAPIResponse(w, r, err, "fs.dir_list.err_generic", http.StatusInternalServerError)
		return
	}
	if truncated {
		connection.Log(logger.LevelInfo, "photo search under %q hit the results cap (%d)", startDir, maxSearchResults)
	}
	streamSearchResults(w, results)
}

// handleClientPhotoIndexStatus returns the photo index progress, shown in the
// WebClient while the first indexing pass is running.
func (s *httpdServer) handleClientPhotoIndexStatus(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestSize)
	claims, err := jwt.FromContext(r.Context())
	if err != nil || claims.Username == "" {
		sendAPIResponse(w, r, nil, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	m := photoindex.Get()
	if m == nil {
		render.JSON(w, r, map[string]any{"enabled": false})
		return
	}
	render.JSON(w, r, struct {
		Enabled bool `json:"enabled"`
		photoindex.Status
	}{Enabled: true, Status: m.Status()})
}

// canThumbnailWithIndex reports whether the photo index tools can render a
// thumbnail for a file the built-in decoders cannot read (HEIC, TIFF, RAW).
func canThumbnailWithIndex(name string) bool {
	m := photoindex.Get()
	return m != nil && m.CanRender(name)
}

// errNoIndexThumbnail means the photo index cannot help with a thumbnail and
// the built-in generation should be used.
var errNoIndexThumbnail = errors.New("no photo index thumbnail")

// thumbnailFromIndex returns a thumbnail (or, if large is true, the full size
// preview) for the file at virtual path name, using the preview generated by
// the photo index or, for formats the built-in decoders cannot read, rendering
// it with the index tools. It only works for local filesystems.
func thumbnailFromIndex(connection *Connection, name string, info os.FileInfo, large bool) ([]byte, error) {
	m := photoindex.Get()
	if m == nil {
		return nil, errNoIndexThumbnail
	}
	fs, fsPath, err := connection.GetFsAndResolvedPath(name)
	if err != nil || !vfs.IsLocalOsFs(fs) {
		return nil, errNoIndexThumbnail
	}
	if p, ok := m.PreviewPath(fsPath, info.ModTime(), info.Size()); ok {
		data, err := os.ReadFile(p)
		if err == nil {
			if large {
				return data, nil
			}
			src, _, err := image.Decode(bytes.NewReader(data))
			if err == nil {
				return encodeThumbnail(src)
			}
		}
	}
	// Not indexed yet: render now if the built-in decoders cannot do it.
	if !large && thumbs.canThumbnail(name) {
		return nil, errNoIndexThumbnail
	}
	if !m.CanRender(name) {
		return nil, errNoIndexThumbnail
	}
	maxEdge := thumbnailMaxEdge
	if large {
		maxEdge = 1024
	}
	return m.RenderJPEG(fsPath, maxEdge)
}
