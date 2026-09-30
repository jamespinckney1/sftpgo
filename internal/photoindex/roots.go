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
	"path"
	"path/filepath"
	"strings"

	"github.com/sftpgo/sdk"

	"github.com/drakkan/sftpgo/v2/internal/dataprovider"
)

// localUserRoots returns the home directories and virtual folders, on the
// local filesystem, of every user. Encrypted and cloud filesystems are not
// indexed.
func localUserRoots() ([]string, error) {
	var roots []string
	const pageSize = 100
	for offset := 0; ; offset += pageSize {
		users, err := dataprovider.GetUsers(pageSize, offset, dataprovider.OrderASC, "")
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			// Group settings can change the home dir and add virtual folders.
			user, err := dataprovider.GetUserWithGroupSettings(u.Username, "")
			if err != nil {
				continue
			}
			for _, mp := range userMappings(&user) {
				roots = append(roots, mp.fsDir)
			}
		}
		if len(users) < pageSize {
			return roots, nil
		}
	}
}

// mapping links a local filesystem directory to the virtual path at which a
// user sees it.
type mapping struct {
	fsDir       string
	virtualPath string
	isFolder    bool
}

func userMappings(user *dataprovider.User) []mapping {
	var res []mapping
	if user.FsConfig.Provider == sdk.LocalFilesystemProvider && filepath.IsAbs(user.GetHomeDir()) {
		res = append(res, mapping{fsDir: filepath.Clean(user.GetHomeDir()), virtualPath: "/"})
	}
	for _, vf := range user.VirtualFolders {
		if vf.FsConfig.Provider == sdk.LocalFilesystemProvider && filepath.IsAbs(vf.MappedPath) {
			res = append(res, mapping{
				fsDir:       filepath.Clean(vf.MappedPath),
				virtualPath: path.Clean(vf.VirtualPath),
				isFolder:    true,
			})
		}
	}
	return res
}

func isInsideVirtual(p, dir string) bool {
	if dir == "/" {
		return true
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// UserScope translates between the filesystem paths stored in the index and
// the virtual paths seen by a user, for a search started in a given virtual
// directory.
type UserScope struct {
	user     *dataprovider.User
	startDir string
	mappings []mapping
	// Dirs are the filesystem directories to search.
	Dirs []string
}

// NewUserScope returns the scope for a search, by user, of the virtual
// directory startDir and everything below it.
func NewUserScope(user *dataprovider.User, startDir string) *UserScope {
	s := &UserScope{user: user, startDir: path.Clean("/" + startDir)}
	// The mapping that contains startDir itself: the innermost virtual folder,
	// or the home dir.
	containing := "/"
	if vf, err := user.GetVirtualFolderForPath(s.startDir); err == nil {
		containing = path.Clean(vf.VirtualPath)
	}
	for _, mp := range userMappings(user) {
		switch {
		case isInsideVirtual(s.startDir, mp.virtualPath):
			if mp.virtualPath != containing {
				// Shadowed by a virtual folder mounted inside this mapping.
				continue
			}
			rel := strings.TrimPrefix(s.startDir, mp.virtualPath)
			s.Dirs = append(s.Dirs, filepath.Join(mp.fsDir, filepath.FromSlash(rel)))
		case isInsideVirtual(mp.virtualPath, s.startDir):
			s.Dirs = append(s.Dirs, mp.fsDir)
		default:
			continue
		}
		s.mappings = append(s.mappings, mp)
	}
	s.Dirs = normalizeRoots(s.Dirs)
	return s
}

// VirtualPath returns the virtual path at which the user sees the file stored
// at fsPath, or false if the file is not visible inside the search scope, for
// example because a virtual folder shadows that part of the home directory.
// Permissions are not checked, see Visible.
func (s *UserScope) VirtualPath(fsPath string) (string, bool) {
	for _, mp := range s.mappings {
		if !isInside(fsPath, mp.fsDir) {
			continue
		}
		rel := filepath.ToSlash(strings.TrimPrefix(fsPath, mp.fsDir))
		vPath := path.Join(mp.virtualPath, rel)
		if !isInsideVirtual(vPath, s.startDir) {
			continue
		}
		// The path must resolve back to this same mapping.
		vf, err := s.user.GetVirtualFolderForPath(vPath)
		if mp.isFolder {
			if err != nil || path.Clean(vf.VirtualPath) != mp.virtualPath {
				continue
			}
		} else if err == nil {
			continue
		}
		return vPath, true
	}
	return "", false
}

// Visible reports whether the user can see the file at virtual path vPath in a
// directory listing.
func (s *UserScope) Visible(vPath string) bool {
	if !s.user.HasPerm(dataprovider.PermListItems, path.Dir(vPath)) {
		return false
	}
	if ok, policy := s.user.IsFileAllowed(vPath); !ok && policy == sdk.DenyPolicyHide {
		return false
	}
	return true
}
