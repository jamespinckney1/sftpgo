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
	"slices"
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
	i18nShowUnavailable   = "fs.search.show_unavailable"
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
		if text != "" && !matchesSearchQuery(text, textLower, path.Base(vPath)) {
			return true
		}
		results = append(results, photoResultRow(len(results)+1, startDir, vPath, &rec))
		if len(results) >= maxSearchResults {
			truncated = true
			return false
		}
		return true
	})
	if errors.Is(err, photoindex.ErrShowUnavailable) {
		connection.Log(logger.LevelInfo, "show search under %q not available: %v", startDir, err)
		sendAPIResponse(w, r, err, i18nShowUnavailable, http.StatusBadRequest)
		return
	}
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

// photoResultRow returns an indexed file as a search result row, in the same
// shape as the /dirs listing plus the "path" of the parent directory and the
// photo fields.
func photoResultRow(id int, startDir, vPath string, rec *photoindex.Media) map[string]any {
	dir, name := path.Split(vPath)
	dir = path.Clean(dir)
	res := make(map[string]any)
	res["id"] = id
	res["url"] = getFileObjectURL(dir, name, webClientFilesPath)
	res["type"] = "2"
	res["size"] = rec.Size
	res["meta"] = fmt.Sprintf("2_%v", searchRelPath(startDir, dir, name))
	res["name"] = name
	res["last_modified"] = getFileObjectModTime(time.Unix(0, rec.ModTime))
	res["path"] = dir
	// "taken" is the local wall clock time at which the photo was taken,
	// deliberately without a zone.
	res["taken"] = rec.TakenTime().Format("2006-01-02T15:04:05")
	res["taken_src"] = rec.TakenSrc
	if rec.Width > 0 && rec.Height > 0 {
		res["width"] = rec.Width
		res["height"] = rec.Height
	}
	if place := (photoindex.Place{City: rec.City, State: rec.State, Country: rec.Country}).String(); place != "" {
		res["place"] = place
	}
	return res
}

// searchDuplicates answers "is:duplicate" and "is:similar": groups of copies
// among the files the user can see, the groups wasting the most space first
// and, in each group, the copy suggested to keep first. Rows carry "group",
// "group_size", "dup_exact", "keep" and "reclaimable" so the WebClient can
// show the groups and preselect the extra copies.
func searchDuplicates(w http.ResponseWriter, r *http.Request, connection *Connection, m *photoindex.Manager,
	startDir string, q photoindex.Query,
) {
	scope := photoindex.NewUserScope(&connection.User, startDir)
	visible := func(fsPath string) (string, bool) {
		vPath, ok := scope.VirtualPath(fsPath)
		if !ok || !scope.Visible(vPath) {
			return "", false
		}
		return vPath, true
	}
	groups, err := m.FindDuplicates(scope.Dirs, q, visible)
	if err != nil {
		connection.Log(logger.LevelError, "duplicate search under %q failed: %v", startDir, err)
		sendAPIResponse(w, r, err, "fs.dir_list.err_generic", http.StatusInternalServerError)
		return
	}
	text := strings.TrimSpace(q.Text)
	textLower := strings.ToLower(text)
	results := make([]map[string]any, 0)
	groupNum := 0
	for _, g := range groups {
		if text != "" && !slices.ContainsFunc(g.Files, func(f photoindex.DupFile) bool {
			return matchesSearchQuery(text, textLower, path.Base(f.VirtualPath))
		}) {
			continue
		}
		if len(results)+len(g.Files) > maxSearchResults {
			connection.Log(logger.LevelInfo, "duplicate search under %q hit the results cap (%d)", startDir, maxSearchResults)
			break
		}
		groupNum++
		for i := range g.Files {
			res := photoResultRow(len(results)+1, startDir, g.Files[i].VirtualPath, &g.Files[i].Media)
			res["group"] = groupNum
			res["group_size"] = len(g.Files)
			res["dup_exact"] = g.Exact
			res["keep"] = g.Files[i].Keep
			res["reclaimable"] = g.Reclaimable
			results = append(results, res)
		}
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
