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
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/drakkan/sftpgo/v2/internal/logger"
	"github.com/drakkan/sftpgo/v2/internal/photoindex"
)

// searchRelPath returns the path of dir/name relative to the search start
// directory. It is used as the row "meta" name in search results, so the
// WebClient actions that build paths as "current dir + name" (delete, rename,
// move, copy, download, share) target the right file even when it is in a
// subfolder of the current directory.
func searchRelPath(startDir, dir, name string) string {
	rel := strings.TrimPrefix(path.Join(dir, name), startDir)
	return strings.TrimPrefix(rel, "/")
}

// searchResultRow returns a search result in the same JSON row shape as the
// /dirs listing, plus a "path" field naming the parent directory.
func searchResultRow(id int, startDir, dir string, info os.FileInfo) map[string]any {
	name := info.Name()
	res := make(map[string]any)
	res["id"] = id
	res["url"] = getFileObjectURL(dir, name, webClientFilesPath)
	if info.IsDir() {
		res["type"] = "1"
		res["size"] = ""
		res["dir_path"] = url.QueryEscape(path.Join(dir, name))
	} else {
		res["type"] = "2"
		if info.Mode()&os.ModeSymlink != 0 {
			res["size"] = ""
		} else {
			res["size"] = info.Size()
			if info.Size() < httpdMaxEditFileSize {
				res["edit_url"] = strings.Replace(res["url"].(string), webClientFilesPath, webClientEditFilePath, 1)
			}
		}
	}
	res["meta"] = fmt.Sprintf("%v_%v", res["type"], searchRelPath(startDir, dir, name))
	res["name"] = name
	res["last_modified"] = getFileObjectModTime(info.ModTime())
	// Extra field (search only): the hit's parent directory, so the UI can
	// show where each result lives and navigate to it.
	res["path"] = dir
	return res
}

type searchHit struct {
	dir  string
	info os.FileInfo
}

// walkSearch recursively searches startDir, breadth first, for the entries
// matching the query name and size filters. Each directory is listed through
// the user's connection, so the usual permissions apply.
//
// A name search stops at maxSearchResults. A size search (larger:, smaller:,
// sort:size) only returns files and keeps walking to return the
// maxSearchResults largest ones. Both are bounded by maxSearchDirs.
func walkSearch(connection *Connection, startDir string, q photoindex.Query) []map[string]any {
	w := &searchWalker{
		connection: connection,
		q:          &q,
		text:       strings.TrimSpace(q.Text),
		sizeMode:   q.HasSizeFilter(),
	}
	w.textLower = strings.ToLower(w.text)
	if w.text == "" && !w.sizeMode {
		return []map[string]any{}
	}
	queue := []string{startDir}
	dirsWalked := 0
	for len(queue) > 0 && !w.truncated {
		if dirsWalked >= maxSearchDirs {
			w.truncated = true
			break
		}
		dir := queue[0]
		queue = queue[1:]
		dirsWalked++
		queue = append(queue, w.searchDir(dir)...)
	}
	if w.sizeMode {
		w.keepLargest()
	}
	if w.truncated {
		connection.Log(logger.LevelInfo, "search for %q under %q hit a safety cap (dirs walked: %d, results: %d); returning partial results",
			q.Text, startDir, dirsWalked, len(w.hits))
	}
	results := make([]map[string]any, 0, len(w.hits))
	for i, h := range w.hits {
		results = append(results, searchResultRow(i+1, startDir, h.dir, h.info))
	}
	return results
}

type searchWalker struct {
	connection *Connection
	q          *photoindex.Query
	text       string
	textLower  string
	sizeMode   bool
	hits       []searchHit
	truncated  bool
}

// searchDir lists dir, records its matching entries and returns its
// subdirectories.
func (w *searchWalker) searchDir(dir string) []string {
	lister, err := w.connection.ReadDir(dir)
	if err != nil {
		// Skip directories the user cannot list or that no longer exist.
		w.connection.Log(logger.LevelDebug, "search: skipping dir %q: %v", dir, err)
		return nil
	}
	defer lister.Close()
	var subdirs []string
	for {
		entries, err := lister.Next(defaultQueryLimit)
		for _, info := range entries {
			if info.IsDir() {
				// Always recurse, whether or not the folder name itself matches.
				subdirs = append(subdirs, path.Join(dir, info.Name()))
			}
			if !searchMatches(info, w.text, w.textLower, w.q, w.sizeMode) {
				continue
			}
			w.hits = append(w.hits, searchHit{dir: dir, info: info})
			if w.sizeMode {
				if len(w.hits) >= 4*maxSearchResults {
					w.keepLargest()
				}
			} else if len(w.hits) >= maxSearchResults {
				w.truncated = true
				return subdirs
			}
		}
		if errors.Is(err, io.EOF) || err != nil {
			return subdirs
		}
	}
}

// keepLargest trims the hits to the largest maxSearchResults files.
func (w *searchWalker) keepLargest() {
	slices.SortStableFunc(w.hits, func(a, b searchHit) int {
		switch {
		case a.info.Size() > b.info.Size():
			return -1
		case a.info.Size() < b.info.Size():
			return 1
		}
		return 0
	})
	if len(w.hits) > maxSearchResults {
		w.hits = w.hits[:maxSearchResults]
	}
}

func searchMatches(info os.FileInfo, text, textLower string, q *photoindex.Query, sizeMode bool) bool {
	if text != "" && !matchesSearchQuery(text, textLower, info.Name()) {
		return false
	}
	if !sizeMode {
		return true
	}
	// Size searches are about files: skip directories and symlinks.
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	return q.SizeMatches(info.Size())
}
