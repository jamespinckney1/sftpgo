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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"

	"github.com/go-chi/render"
	"github.com/rs/xid"

	"github.com/drakkan/sftpgo/v2/internal/dataprovider"
	"github.com/drakkan/sftpgo/v2/internal/jwt"
	"github.com/drakkan/sftpgo/v2/internal/logger"
	"github.com/drakkan/sftpgo/v2/internal/photoindex"
	"github.com/drakkan/sftpgo/v2/internal/util"
)

// People (fork feature, see FORK_FEATURES.md): the faces found by the photo
// index are grouped into people that any user can name, merge, hide and
// correct. Names are shared by the whole family, but every user only sees the
// people, faces and photos in the files they can access.

const faceCropSize = 160

type clientPeoplePage struct {
	baseClientPage
	PeopleListURL   string
	PersonFacesURL  string
	FaceURL         string
	PeopleActionURL string
}

// peopleURLForUser returns the People page URL if face recognition is
// enabled, or an empty string to hide the menu entry.
func peopleURLForUser() string {
	if m := photoindex.Get(); m != nil && m.FacesEnabled() {
		return webClientPeoplePath
	}
	return ""
}

type peopleContext struct {
	user        dataprovider.User
	m           *photoindex.Manager
	visible     photoindex.Visible
	virtualPath func(fsPath string) (string, bool)
}

// getPeopleContext checks that face recognition is enabled and the user may
// use the WebClient, and returns a function reporting whether the user can
// see a photo, applying the same rules as a directory listing.
func getPeopleContext(w http.ResponseWriter, r *http.Request) (*peopleContext, bool) {
	m := photoindex.Get()
	if m == nil || !m.FacesEnabled() {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return nil, false
	}
	return getPhotoUserContext(w, r, m)
}

// getPhotoUserContext checks that the user may use the WebClient and returns
// the functions mapping indexed files to what the user can see.
func getPhotoUserContext(w http.ResponseWriter, r *http.Request, m *photoindex.Manager) (*peopleContext, bool) {
	claims, err := jwt.FromContext(r.Context())
	if err != nil || claims.Username == "" {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return nil, false
	}
	user, err := dataprovider.GetUserWithGroupSettings(claims.Username, "")
	if err != nil {
		status := getRespStatus(err)
		http.Error(w, http.StatusText(status), status)
		return nil, false
	}
	connectionID := fmt.Sprintf("%s_%s", getProtocolFromRequest(r), xid.New().String())
	if err := checkHTTPClientUser(&user, r, connectionID, false, false); err != nil {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return nil, false
	}
	ctx := &peopleContext{user: user, m: m}
	scope := photoindex.NewUserScope(&ctx.user, "/")
	ctx.visible = func(fsPath string) bool {
		vPath, ok := scope.VirtualPath(fsPath)
		return ok && scope.Visible(vPath)
	}
	ctx.virtualPath = func(fsPath string) (string, bool) {
		vPath, ok := scope.VirtualPath(fsPath)
		return vPath, ok && scope.Visible(vPath)
	}
	return ctx, true
}

func (s *httpdServer) handleClientPeoplePage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestSize)
	if _, ok := getPeopleContext(w, r); !ok {
		return
	}
	data := clientPeoplePage{
		baseClientPage:  s.getBaseClientPageData(util.I18nPeopleTitle, webClientPeoplePath, w, r),
		PeopleListURL:   webClientPeoplePath + "/list",
		PersonFacesURL:  webClientPeoplePath + "/faces",
		FaceURL:         webClientPeoplePath + "/face",
		PeopleActionURL: webClientPeoplePath + "/action",
	}
	renderClientTemplate(w, templateClientPeople, data)
}

// handleClientPeopleList returns the people the user can see. Unnamed groups
// with a single face are left out unless all=1.
func (s *httpdServer) handleClientPeopleList(w http.ResponseWriter, r *http.Request) {
	ctx, ok := getPeopleContext(w, r)
	if !ok {
		return
	}
	minFaces := 2
	if r.URL.Query().Get("all") == "1" {
		minFaces = 1
	}
	people, err := ctx.m.People(ctx.visible, minFaces)
	if err != nil {
		sendAPIResponse(w, r, err, "", http.StatusInternalServerError)
		return
	}
	// Only offer the names of people the user can see.
	names := make([]string, 0)
	for _, p := range people {
		if p.Name != "" && !p.Hidden {
			names = append(names, p.Name)
		}
	}
	render.JSON(w, r, map[string]any{
		"people": people,
		"names":  names,
		"status": ctx.m.Status().Faces,
	})
}

// visiblePerson returns the person if the user can see at least one of their
// faces, with those faces.
func (ctx *peopleContext) visiblePerson(id int64) (photoindex.Person, []photoindex.Face, bool, error) {
	p, found, err := ctx.m.GetPerson(id)
	if err != nil || !found {
		return p, nil, false, err
	}
	faces, err := ctx.m.PersonFaces(id, ctx.visible)
	if err != nil || len(faces) == 0 {
		return p, nil, false, err
	}
	return p, faces, true, nil
}

func (s *httpdServer) handleClientPersonFaces(w http.ResponseWriter, r *http.Request) {
	ctx, ok := getPeopleContext(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		sendAPIResponse(w, r, err, "", http.StatusBadRequest)
		return
	}
	p, faces, found, err := ctx.visiblePerson(id)
	if err != nil {
		sendAPIResponse(w, r, err, "", http.StatusInternalServerError)
		return
	}
	if !found {
		sendAPIResponse(w, r, nil, "", http.StatusNotFound)
		return
	}
	items := make([]map[string]any, 0, len(faces))
	for _, f := range faces {
		vPath, ok := ctx.virtualPath(f.Path)
		if !ok {
			continue
		}
		dir, name := path.Split(vPath)
		dir = path.Clean(dir)
		items = append(items, map[string]any{
			"id":       f.ID,
			"locked":   f.Locked,
			"name":     name,
			"path":     dir,
			"file_url": getFileObjectURL(dir, name, webClientFilesPath),
			"dir_url":  webClientFilesPath + "?path=" + url.QueryEscape(dir),
		})
	}
	render.JSON(w, r, map[string]any{
		"person": p,
		"faces":  items,
	})
}

func (s *httpdServer) handleClientFaceCrop(w http.ResponseWriter, r *http.Request) {
	ctx, ok := getPeopleContext(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	f, found, err := ctx.m.GetFace(id)
	if err != nil || !found || !ctx.visible(f.Path) {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	etag := fmt.Sprintf(`"face-%d"`, f.ID)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	data, err := ctx.m.FaceCrop(f, faceCropSize)
	if err != nil {
		logger.Debug(logSender, "", "unable to crop face %d: %v", f.ID, err)
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	writeThumbnail(w, data)
}

type peopleAction struct {
	// Action is one of rename, merge, hide, unhide, move_faces.
	Action   string  `json:"action"`
	PersonID int64   `json:"person_id"`
	TargetID int64   `json:"target_id"`
	Name     string  `json:"name"`
	FaceIDs  []int64 `json:"face_ids"`
}

func (s *httpdServer) handleClientPeopleAction(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestSize)
	ctx, ok := getPeopleContext(w, r)
	if !ok {
		return
	}
	var req peopleAction
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendAPIResponse(w, r, err, "", http.StatusBadRequest)
		return
	}
	resultID, status, err := ctx.applyAction(&req)
	if err != nil {
		if status == http.StatusInternalServerError {
			logger.Warn(logSender, "", "people action %q by %q failed: %v", req.Action, ctx.user.Username, err)
		}
		sendAPIResponse(w, r, err, "", status)
		return
	}
	logger.Info(logSender, "", "people action %q by %q: person %d, target %d, name %q, faces %d",
		req.Action, ctx.user.Username, req.PersonID, req.TargetID, req.Name, len(req.FaceIDs))
	render.JSON(w, r, map[string]any{"person_id": resultID})
}

var errNotVisible = errors.New("not found")

// applyAction runs a people action, after checking that the user can see the
// people and faces involved.
func (ctx *peopleContext) applyAction(req *peopleAction) (int64, int, error) {
	checkPerson := func(id int64) (int, error) {
		_, _, found, err := ctx.visiblePerson(id)
		if err != nil {
			return http.StatusInternalServerError, err
		}
		if !found {
			return http.StatusNotFound, errNotVisible
		}
		return 0, nil
	}
	switch req.Action {
	case "rename":
		if status, err := checkPerson(req.PersonID); err != nil {
			return 0, status, err
		}
		id, err := ctx.m.RenamePerson(req.PersonID, req.Name)
		if errors.Is(err, photoindex.ErrNameRequired) {
			return 0, http.StatusBadRequest, err
		}
		return id, http.StatusInternalServerError, err
	case "move_faces":
		return ctx.moveFaces(req)
	case "merge":
		for _, id := range []int64{req.PersonID, req.TargetID} {
			if status, err := checkPerson(id); err != nil {
				return 0, status, err
			}
		}
		return req.TargetID, http.StatusInternalServerError, ctx.m.MergePeople(req.PersonID, req.TargetID)
	case "hide", "unhide":
		if status, err := checkPerson(req.PersonID); err != nil {
			return 0, status, err
		}
		return req.PersonID, http.StatusInternalServerError, ctx.m.SetPersonHidden(req.PersonID, req.Action == "hide")
	}
	return 0, http.StatusBadRequest, fmt.Errorf("unknown action %q", req.Action)
}

// moveFaces moves faces the user can see to the named person, or out of
// their person with an empty name.
func (ctx *peopleContext) moveFaces(req *peopleAction) (int64, int, error) {
	if len(req.FaceIDs) == 0 {
		return 0, http.StatusBadRequest, errors.New("no faces selected")
	}
	for _, id := range req.FaceIDs {
		f, found, err := ctx.m.GetFace(id)
		if err != nil {
			return 0, http.StatusInternalServerError, err
		}
		if !found || !ctx.visible(f.Path) {
			return 0, http.StatusNotFound, errNotVisible
		}
	}
	id, err := ctx.m.MoveFaces(req.FaceIDs, req.Name)
	return id, http.StatusInternalServerError, err
}
