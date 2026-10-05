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

package httpd_test

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drakkan/sftpgo/v2/internal/dataprovider"
	"github.com/drakkan/sftpgo/v2/internal/httpdtest"
	"github.com/drakkan/sftpgo/v2/internal/photoindex"
	"github.com/drakkan/sftpgo/v2/internal/photoindex/mltest"
)

const (
	webClientPhotoStatusPath = "/web/client/photoindex/status"
	webClientPeoplePath      = "/web/client/people"
	webClientPlacesPath      = "/web/client/places"
)

func testJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 320, 240))
	for y := range 240 {
		for x := range 320 {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, nil))
	return buf.Bytes()
}

// startTestPhotoIndex starts the photo index on the given roots and waits for
// the first scan to complete with the expected number of files.
func startTestPhotoIndex(t *testing.T, cfg photoindex.Config, roots []string, expected int64) {
	t.Helper()
	cfg.Enabled = true
	cfg.DataDir = filepath.Join(t.TempDir(), "photoindex")
	cfg.StartupDelay = 0
	cfg.Workers = 1
	err := photoindex.Initialize(cfg, "", func() ([]string, error) { return roots, nil })
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		st := photoindex.Get().Status()
		return st.Total == expected && !st.Scanning && st.Pending == 0
	}, 20*time.Second, 50*time.Millisecond)
}

func TestWebClientPhotoSearch(t *testing.T) {
	u := getTestUser()
	u.Permissions["/private"] = []string{dataprovider.PermUpload}
	user, _, err := httpdtest.AddUser(u, http.StatusCreated)
	require.NoError(t, err)
	defer func() {
		photoindex.Initialize(photoindex.Config{}, "", nil) //nolint:errcheck
		_, err = httpdtest.RemoveUser(user, http.StatusOK)
		assert.NoError(t, err)
		assert.NoError(t, os.RemoveAll(user.GetHomeDir()))
	}()

	home := user.GetHomeDir()
	// Another indexed directory, not visible to this user.
	otherRoot := filepath.Join(t.TempDir(), "other")
	data := testJPEG(t)
	for _, p := range []string{
		filepath.Join(home, "2019", "IMG_20190501_101010.jpg"),
		filepath.Join(home, "2023", "IMG_20230614_120000.jpg"),
		filepath.Join(home, "private", "IMG_20230701_000000.jpg"),
		filepath.Join(home, "misc", "nodate.jpg"),
		filepath.Join(otherRoot, "IMG_20230615_000000.jpg"),
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), os.ModePerm))
		require.NoError(t, os.WriteFile(p, data, os.ModePerm))
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, "notes.txt"), []byte("x"), os.ModePerm))

	// No external tools: dates come from the file names.
	startTestPhotoIndex(t, photoindex.Config{ExiftoolPath: "/nonexistent", VipsPath: "/nonexistent"},
		[]string{home, otherRoot}, 5)

	webToken, err := getJWTWebClientTokenFromTestServer(defaultUsername, defaultPassword)
	require.NoError(t, err)

	search := func(startPath, q string, expectedStatus int) []map[string]any {
		reqURL := webClientSearchPath + "?path=" + url.QueryEscape(startPath) + "&q=" + url.QueryEscape(q)
		req, _ := http.NewRequest(http.MethodGet, reqURL, nil)
		setJWTCookieForReq(req, webToken)
		rr := executeRequest(req)
		checkResponseCode(t, expectedStatus, rr)
		if expectedStatus != http.StatusOK {
			var resp map[string]any
			assert.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
			return []map[string]any{resp}
		}
		var results []map[string]any
		assert.NoError(t, json.Unmarshal(rr.Body.Bytes(), &results))
		return results
	}
	names := func(results []map[string]any) []string {
		out := make([]string, 0, len(results))
		for _, r := range results {
			out = append(out, r["name"].(string))
		}
		return out
	}

	// The photo in the unlistable dir and the one outside the user's home are
	// not returned.
	res := search("/", "taken:2023", http.StatusOK)
	require.Equal(t, []string{"IMG_20230614_120000.jpg"}, names(res))
	assert.Equal(t, "/2023", res[0]["path"])
	assert.Equal(t, "2023-06-14T12:00:00", res[0]["taken"])
	assert.Equal(t, "filename", res[0]["taken_src"])
	assert.Equal(t, "2", res[0]["type"])

	// Newest first; the undated file uses its (recent) modification time.
	res = search("/", "taken:any", http.StatusOK)
	assert.Equal(t, []string{"nodate.jpg", "IMG_20230614_120000.jpg", "IMG_20190501_101010.jpg"}, names(res))
	res = search("/", "taken:unknown", http.StatusOK)
	assert.Equal(t, []string{"nodate.jpg"}, names(res))
	res = search("/", "taken:2019..2023-06 IMG_*", http.StatusOK)
	assert.Equal(t, []string{"IMG_20230614_120000.jpg", "IMG_20190501_101010.jpg"}, names(res))
	res = search("/2019", "taken:any", http.StatusOK)
	assert.Equal(t, []string{"IMG_20190501_101010.jpg"}, names(res))
	res = search("/", "taken:1990", http.StatusOK)
	assert.Empty(t, res)

	res = search("/", "taken:2023-13", http.StatusBadRequest)
	assert.Equal(t, "fs.search.photo_query_invalid", res[0]["message"])

	// Plain name searches still walk the directories.
	res = search("/", "notes", http.StatusOK)
	assert.Equal(t, []string{"notes.txt"}, names(res))

	// Uploads through SFTPGo are indexed right away.
	apiToken, err := getJWTAPIUserTokenFromTestServer(defaultUsername, defaultPassword)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, userUploadFilePath+"?path="+url.QueryEscape("IMG_20240101_080000.jpg"),
		bytes.NewReader(data))
	require.NoError(t, err)
	setBearerForReq(req, apiToken)
	rr := executeRequest(req)
	checkResponseCode(t, http.StatusCreated, rr)
	assert.Eventually(t, func() bool {
		return len(search("/", "taken:2024", http.StatusOK)) == 1
	}, 10*time.Second, 50*time.Millisecond)

	// Status endpoint.
	req, _ = http.NewRequest(http.MethodGet, webClientPhotoStatusPath, nil)
	setJWTCookieForReq(req, webToken)
	rr = executeRequest(req)
	checkResponseCode(t, http.StatusOK, rr)
	var status map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &status))
	assert.Equal(t, true, status["enabled"])
	assert.Equal(t, float64(6), status["total"])

	// Previews for formats browsers cannot display, when libvips is available.
	if vips, err := exec.LookPath("vipsthumbnail"); err == nil {
		heicAvailable := exec.Command("heif-enc", "-q", "50", filepath.Join(home, "2019", "IMG_20190501_101010.jpg"),
			"-o", filepath.Join(home, "2019", "IMG_20190502_101010.heic")).Run() == nil
		expected := int64(6)
		if heicAvailable {
			expected++
		}
		startTestPhotoIndex(t, photoindex.Config{ExiftoolPath: "/nonexistent", VipsPath: vips, PreviewSize: 512},
			[]string{home, otherRoot}, expected)

		req, _ = http.NewRequest(http.MethodGet, webClientThumbnailPath+"?size=preview&path="+
			url.QueryEscape("/2023/IMG_20230614_120000.jpg"), nil)
		setJWTCookieForReq(req, webToken)
		rr = executeRequest(req)
		checkResponseCode(t, http.StatusOK, rr)
		cfg, _, err := image.DecodeConfig(bytes.NewReader(rr.Body.Bytes()))
		require.NoError(t, err)
		assert.Equal(t, 320, cfg.Width, "previews are never upscaled")

		if heicAvailable {
			for _, size := range []string{"", "preview"} {
				req, _ = http.NewRequest(http.MethodGet, webClientThumbnailPath+"?size="+size+"&path="+
					url.QueryEscape("/2019/IMG_20190502_101010.heic"), nil)
				setJWTCookieForReq(req, webToken)
				rr = executeRequest(req)
				checkResponseCode(t, http.StatusOK, rr)
				assert.Equal(t, "image/jpeg", rr.Header().Get("Content-Type"))
				assert.Equal(t, []byte{0xFF, 0xD8}, rr.Body.Bytes()[:2])
			}
		}
		// Files the user cannot list are still protected.
		req, _ = http.NewRequest(http.MethodGet, webClientThumbnailPath+"?size=preview&path="+
			url.QueryEscape("/private/IMG_20230701_000000.jpg"), nil)
		setJWTCookieForReq(req, webToken)
		rr = executeRequest(req)
		checkResponseCode(t, http.StatusNotFound, rr)
	}

	// With the index disabled, photo filters are reported as unavailable.
	require.NoError(t, photoindex.Initialize(photoindex.Config{}, "", nil))
	res = search("/", "taken:2023", http.StatusBadRequest)
	assert.Equal(t, "fs.search.photo_index_disabled", res[0]["message"])
	req, _ = http.NewRequest(http.MethodGet, webClientThumbnailPath+"?size=preview&path="+
		url.QueryEscape("/2023/IMG_20230614_120000.jpg"), nil)
	setJWTCookieForReq(req, webToken)
	rr = executeRequest(req)
	checkResponseCode(t, http.StatusNotFound, rr)
}

func TestWebClientCleanupSearch(t *testing.T) {
	u := getTestUser()
	u.Permissions["/private"] = []string{dataprovider.PermUpload}
	user, _, err := httpdtest.AddUser(u, http.StatusCreated)
	require.NoError(t, err)
	defer func() {
		photoindex.Initialize(photoindex.Config{}, "", nil) //nolint:errcheck
		_, err = httpdtest.RemoveUser(user, http.StatusOK)
		assert.NoError(t, err)
		assert.NoError(t, os.RemoveAll(user.GetHomeDir()))
	}()

	home := user.GetHomeDir()
	data := testJPEG(t)
	write := func(rel string, content []byte) {
		p := filepath.Join(home, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), os.ModePerm))
		require.NoError(t, os.WriteFile(p, content, os.ModePerm))
	}
	write("2019/IMG_20190501_101010.jpg", data)
	write("backup/IMG_20190501_101010.jpg", data)  // exact copy
	write("private/IMG_20190501_101010.jpg", data) // copy the user cannot list
	write("big/video.bin", bytes.Repeat([]byte("x"), 3*1024*1024))
	write("big/archive.zip", bytes.Repeat([]byte("y"), 2*1024*1024))
	write("notes.txt", []byte("hello"))

	webToken, err := getJWTWebClientTokenFromTestServer(defaultUsername, defaultPassword)
	require.NoError(t, err)
	search := func(startPath, q string) []map[string]any {
		reqURL := webClientSearchPath + "?path=" + url.QueryEscape(startPath) + "&q=" + url.QueryEscape(q)
		req, _ := http.NewRequest(http.MethodGet, reqURL, nil)
		setJWTCookieForReq(req, webToken)
		rr := executeRequest(req)
		checkResponseCode(t, http.StatusOK, rr)
		var results []map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &results))
		return results
	}
	names := func(results []map[string]any) []string {
		out := make([]string, 0, len(results))
		for _, r := range results {
			out = append(out, r["name"].(string))
		}
		return out
	}

	// Size searches work on any file, without the photo index, largest first.
	res := search("/", "sort:size")
	require.GreaterOrEqual(t, len(res), 5)
	assert.Equal(t, []string{"video.bin", "archive.zip"}, names(res[:2]))
	res = search("/", "larger:1MB")
	assert.Equal(t, []string{"video.bin", "archive.zip"}, names(res))
	res = search("/", "larger:2.5MB")
	assert.Equal(t, []string{"video.bin"}, names(res))
	res = search("/", "*.zip larger:1MB")
	assert.Equal(t, []string{"archive.zip"}, names(res))
	res = search("/", "smaller:1KB")
	assert.Equal(t, []string{"notes.txt"}, names(res))
	// The row meta holds the path relative to the search start, so the
	// WebClient actions target the right file; directories are not listed.
	res = search("/", "larger:2.5MB")
	assert.Equal(t, "2_big/video.bin", res[0]["meta"])
	assert.Equal(t, "/big", res[0]["path"])
	res = search("/big", "larger:2.5MB")
	assert.Equal(t, "2_video.bin", res[0]["meta"])
	// Name searches also use relative metas.
	res = search("/", "notes")
	assert.Equal(t, "2_notes.txt", res[0]["meta"])
	res = search("/", "IMG_2019")
	for _, r := range res {
		assert.Contains(t, []string{"1_", "2_"}, r["meta"].(string)[:2])
		assert.Contains(t, r["meta"], "/", "hits in subfolders have a relative path")
	}

	// Duplicates need the photo index.
	startTestPhotoIndex(t, photoindex.Config{ExiftoolPath: "/nonexistent", VipsPath: "/nonexistent"},
		[]string{home}, 3)
	require.Eventually(t, func() bool {
		return photoindex.Get().Status().DupPending == 0
	}, 20*time.Second, 100*time.Millisecond)

	res = search("/", "is:duplicate")
	require.Len(t, res, 2, "the copy in the unlistable dir is not shown")
	assert.Equal(t, float64(1), res[0]["group"])
	assert.Equal(t, float64(2), res[0]["group_size"])
	assert.Equal(t, true, res[0]["dup_exact"])
	assert.Equal(t, true, res[0]["keep"])
	assert.Equal(t, false, res[1]["keep"])
	assert.Equal(t, float64(len(data)), res[1]["reclaimable"])
	assert.Equal(t, "2_2019/IMG_20190501_101010.jpg", res[0]["meta"], "shortest path kept")
	assert.Equal(t, "2_backup/IMG_20190501_101010.jpg", res[1]["meta"])

	res = search("/", "is:similar")
	assert.Len(t, res, 2)
	res = search("/2019", "is:duplicate")
	assert.Empty(t, res, "only one copy inside the scope")
	res = search("/", "is:duplicate nomatch")
	assert.Empty(t, res)

	// Deleting the extra copy, as the WebClient does (current dir + meta name).
	csrfToken, err := getCSRFTokenFromInternalPageMock(webClientProfilePath, webToken)
	require.NoError(t, err)
	req, _ := http.NewRequest(http.MethodDelete, webClientFilesPath+"?path="+url.QueryEscape("/backup/IMG_20190501_101010.jpg"), nil)
	req.RemoteAddr = defaultRemoteAddr
	req.Header.Set("X-CSRF-TOKEN", csrfToken)
	setJWTCookieForReq(req, webToken)
	rr := executeRequest(req)
	checkResponseCode(t, http.StatusOK, rr)
	assert.Empty(t, search("/", "is:duplicate"), "the group disappears right away")
	assert.FileExists(t, filepath.Join(home, "2019", "IMG_20190501_101010.jpg"))
}

func facePhotoJPEG(t *testing.T, identities ...int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{128, 128, 128, 255}}, image.Point{}, draw.Src)
	for i, id := range identities {
		mltest.DrawFace(img, id, 30+i*150, 100+i*20, 64)
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}))
	return buf.Bytes()
}

func TestWebClientPeople(t *testing.T) {
	ml := mltest.NewServer()
	defer ml.Close()

	// Two users: the second one cannot see the first one's home.
	u := getTestUser()
	u.Permissions["/private"] = []string{dataprovider.PermUpload}
	user, _, err := httpdtest.AddUser(u, http.StatusCreated)
	require.NoError(t, err)
	u2 := getTestUser()
	u2.Username = defaultUsername + "_2"
	u2.HomeDir = filepath.Join(filepath.Dir(user.GetHomeDir()), u2.Username)
	user2, _, err := httpdtest.AddUser(u2, http.StatusCreated)
	require.NoError(t, err)
	defer func() {
		photoindex.Initialize(photoindex.Config{}, "", nil) //nolint:errcheck
		for _, usr := range []dataprovider.User{user, user2} {
			_, err = httpdtest.RemoveUser(usr, http.StatusOK)
			assert.NoError(t, err)
			assert.NoError(t, os.RemoveAll(usr.GetHomeDir()))
		}
	}()
	const red, blue, green = 0, 1, 2
	write := func(p string, data []byte) {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), os.ModePerm))
		require.NoError(t, os.WriteFile(p, data, os.ModePerm))
	}
	home, home2 := user.GetHomeDir(), user2.GetHomeDir()
	write(filepath.Join(home, "a.jpg"), facePhotoJPEG(t, red, blue))
	write(filepath.Join(home, "b.jpg"), facePhotoJPEG(t, red))
	write(filepath.Join(home, "private", "c.jpg"), facePhotoJPEG(t, green)) // not listable by user
	write(filepath.Join(home2, "d.jpg"), facePhotoJPEG(t, red, green))

	startTestPhotoIndex(t, photoindex.Config{ExiftoolPath: "/nonexistent", VipsPath: "/nonexistent",
		MLURL: ml.URL, FaceModel: "buffalo_l", FaceMinScore: 0.7, FaceMatchThreshold: 0.5}, []string{home, home2}, 4)
	require.Eventually(t, func() bool {
		st := photoindex.Get().Status().Faces
		return st.Pending == 0 && st.Faces == 6
	}, 30*time.Second, 100*time.Millisecond)

	token1, err := getJWTWebClientTokenFromTestServer(defaultUsername, defaultPassword)
	require.NoError(t, err)
	token2, err := getJWTWebClientTokenFromTestServer(u2.Username, defaultPassword)
	require.NoError(t, err)
	csrf1, err := getCSRFTokenFromInternalPageMock(webClientProfilePath, token1)
	require.NoError(t, err)

	get := func(token, reqURL string, expected int) []byte {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, reqURL, nil)
		setJWTCookieForReq(req, token)
		rr := executeRequest(req)
		checkResponseCode(t, expected, rr)
		return rr.Body.Bytes()
	}
	type personJSON struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Faces  int    `json:"faces"`
		Photos int    `json:"photos"`
		Cover  int64  `json:"cover"`
		Hidden bool   `json:"hidden"`
	}
	list := func(token string) []personJSON {
		t.Helper()
		var resp struct {
			People []personJSON `json:"people"`
			Names  []string     `json:"names"`
		}
		require.NoError(t, json.Unmarshal(get(token, webClientPeoplePath+"/list?all=1", http.StatusOK), &resp))
		return resp.People
	}
	action := func(payload map[string]any, expected int) int64 {
		t.Helper()
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		req, _ := http.NewRequest(http.MethodPost, webClientPeoplePath+"/action", bytes.NewReader(body))
		req.RemoteAddr = defaultRemoteAddr
		req.Header.Set("X-CSRF-TOKEN", csrf1)
		setJWTCookieForReq(req, token1)
		rr := executeRequest(req)
		checkResponseCode(t, expected, rr)
		var resp struct {
			PersonID int64 `json:"person_id"`
		}
		json.Unmarshal(rr.Body.Bytes(), &resp) //nolint:errcheck
		return resp.PersonID
	}

	// The page and the menu entry.
	page := string(get(token1, webClientPeoplePath, http.StatusOK))
	assert.Contains(t, page, `id="people_list_view"`)
	assert.Contains(t, string(get(token1, webClientFilesPath, http.StatusOK)), `href="`+webClientPeoplePath+`"`)

	// User 1 sees red (2 photos) and blue, not green (only in the unlistable dir).
	people := list(token1)
	require.Len(t, people, 2)
	var redID, blueID int64
	for _, p := range people {
		switch p.Photos {
		case 2:
			redID = p.ID
		case 1:
			blueID = p.ID
		}
	}
	require.NotZero(t, redID)
	require.NotZero(t, blueID)
	// User 2 sees red (its own photo, same person) and green.
	people2 := list(token2)
	require.Len(t, people2, 2)
	var greenID int64
	for _, p := range people2 {
		assert.Equal(t, 1, p.Faces)
		if p.ID != redID {
			greenID = p.ID
		}
	}
	require.NotZero(t, greenID)

	// Faces and face thumbnails follow the photo permissions.
	var faces struct {
		Faces []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"faces"`
	}
	require.NoError(t, json.Unmarshal(get(token1, webClientPeoplePath+"/faces?id="+strconv.FormatInt(redID, 10), http.StatusOK), &faces))
	require.Len(t, faces.Faces, 2)
	crop := get(token1, webClientPeoplePath+"/face?id="+strconv.FormatInt(faces.Faces[0].ID, 10), http.StatusOK)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(crop))
	require.NoError(t, err)
	assert.Equal(t, 160, cfg.Width)
	get(token2, webClientPeoplePath+"/face?id="+strconv.FormatInt(faces.Faces[0].ID, 10), http.StatusNotFound)
	get(token1, webClientPeoplePath+"/faces?id="+strconv.FormatInt(greenID, 10), http.StatusNotFound)

	// Naming, searching, merging by name.
	assert.Equal(t, redID, action(map[string]any{"action": "rename", "person_id": redID, "name": "Grandma Rose"}, http.StatusOK))
	action(map[string]any{"action": "rename", "person_id": blueID, "name": " "}, http.StatusBadRequest)
	action(map[string]any{"action": "rename", "person_id": greenID, "name": "Hacker"}, http.StatusNotFound) // not visible
	action(map[string]any{"action": "explode", "person_id": redID}, http.StatusBadRequest)

	searchPeople := func(token, q string) []string {
		t.Helper()
		var res []map[string]any
		require.NoError(t, json.Unmarshal(get(token, webClientSearchPath+"?path=%2F&q="+url.QueryEscape(q), http.StatusOK), &res))
		var names []string
		for _, r := range res {
			names = append(names, r["name"].(string))
		}
		return names
	}
	assert.ElementsMatch(t, []string{"a.jpg", "b.jpg"}, searchPeople(token1, "person:rose"))
	assert.ElementsMatch(t, []string{"d.jpg"}, searchPeople(token2, `person:"grandma rose"`), "names are shared")
	assert.ElementsMatch(t, []string{"a.jpg"}, searchPeople(token1, "person:#"+strconv.FormatInt(blueID, 10)))

	assert.Equal(t, redID, action(map[string]any{"action": "rename", "person_id": blueID, "name": "grandma rose"}, http.StatusOK))
	people = list(token1)
	require.Len(t, people, 1)
	assert.Equal(t, 3, people[0].Faces)

	// Moving a face out, then to a new person.
	require.NoError(t, json.Unmarshal(get(token1, webClientPeoplePath+"/faces?id="+strconv.FormatInt(redID, 10), http.StatusOK), &faces))
	require.Len(t, faces.Faces, 3)
	moved := faces.Faces[2].ID
	bobID := action(map[string]any{"action": "move_faces", "face_ids": []int64{moved}, "name": "Bob"}, http.StatusOK)
	assert.NotZero(t, bobID)
	assert.Len(t, searchPeople(token1, "person:bob"), 1)
	action(map[string]any{"action": "move_faces", "face_ids": []int64{}, "name": "Bob"}, http.StatusBadRequest)

	// Hiding.
	action(map[string]any{"action": "hide", "person_id": bobID}, http.StatusOK)
	assert.Empty(t, searchPeople(token1, "person:bob"))
	for _, p := range list(token1) {
		if p.ID == bobID {
			assert.True(t, p.Hidden)
		}
	}
	action(map[string]any{"action": "unhide", "person_id": bobID}, http.StatusOK)
	assert.Len(t, searchPeople(token1, "person:bob"), 1)

	// Without CSRF token actions are refused.
	req, _ := http.NewRequest(http.MethodPost, webClientPeoplePath+"/action", bytes.NewReader([]byte(`{"action":"hide","person_id":1}`)))
	setJWTCookieForReq(req, token1)
	rr := executeRequest(req)
	checkResponseCode(t, http.StatusForbidden, rr)

	// Disabled face recognition: no page.
	startTestPhotoIndex(t, photoindex.Config{ExiftoolPath: "/nonexistent", VipsPath: "/nonexistent"}, []string{home, home2}, 4)
	get(token1, webClientPeoplePath, http.StatusNotFound)
	assert.NotContains(t, string(get(token1, webClientFilesPath, http.StatusOK)), `href="`+webClientPeoplePath+`"`)
}

func colorJPEG(t *testing.T, colors ...color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 400, 300))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{128, 128, 128, 255}}, image.Point{}, draw.Src)
	for i, c := range colors {
		draw.Draw(img, image.Rect(20+i*120, 80, 120+i*120, 180), &image.Uniform{c}, image.Point{}, draw.Src)
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}))
	return buf.Bytes()
}

func TestWebClientShowAndPlaces(t *testing.T) {
	ml := mltest.NewServer()
	defer ml.Close()
	u := getTestUser()
	u.Permissions["/private"] = []string{dataprovider.PermUpload}
	user, _, err := httpdtest.AddUser(u, http.StatusCreated)
	require.NoError(t, err)
	defer func() {
		photoindex.Initialize(photoindex.Config{}, "", nil) //nolint:errcheck
		_, err = httpdtest.RemoveUser(user, http.StatusOK)
		assert.NoError(t, err)
		assert.NoError(t, os.RemoveAll(user.GetHomeDir()))
	}()
	home := user.GetHomeDir()
	red, blue := mltest.Concepts["red"], mltest.Concepts["blue"]
	write := func(rel string, data []byte) {
		p := filepath.Join(home, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), os.ModePerm))
		require.NoError(t, os.WriteFile(p, data, os.ModePerm))
	}
	write("red.jpg", colorJPEG(t, red))
	write("trip/red-blue.jpg", colorJPEG(t, red, blue))
	write("blue.jpg", colorJPEG(t, blue))
	write("private/red.jpg", colorJPEG(t, red))

	// GPS positions, if exiftool is available to write them.
	exiftool, _ := exec.LookPath("exiftool")
	if exiftool != "" {
		for p, pos := range map[string][2]string{
			filepath.Join(home, "red.jpg"):              {"33.70", "-78.87"}, // Myrtle Beach
			filepath.Join(home, "trip", "red-blue.jpg"): {"40.75", "-73.99"}, // New York
			filepath.Join(home, "private", "red.jpg"):   {"33.70", "-78.87"},
		} {
			lonRef := "E"
			if strings.HasPrefix(pos[1], "-") {
				lonRef = "W"
			}
			out, err := exec.Command(exiftool, "-q", "-overwrite_original", "-GPSLatitude="+pos[0], "-GPSLatitudeRef=N",
				"-GPSLongitude="+strings.TrimPrefix(pos[1], "-"), "-GPSLongitudeRef="+lonRef, p).CombinedOutput()
			require.NoError(t, err, string(out))
		}
	}
	geoDir := filepath.Join(t.TempDir(), "geonames")
	require.NoError(t, os.MkdirAll(geoDir, os.ModePerm))
	require.NoError(t, os.WriteFile(filepath.Join(geoDir, "admin1CodesASCII.txt"),
		[]byte("US.SC\tSouth Carolina\tSouth Carolina\t1\nUS.NY\tNew York\tNew York\t2\n"), os.ModePerm))
	require.NoError(t, os.WriteFile(filepath.Join(geoDir, "countryInfo.txt"),
		[]byte("#ISO\tISO3\nUS\tUSA\t840\tUS\tUnited States\tWashington\n"), os.ModePerm))
	require.NoError(t, os.WriteFile(filepath.Join(geoDir, "cities500.txt"), []byte(
		"1\tMyrtle Beach\tMyrtle Beach\t\t33.68906\t-78.88669\tP\tPPL\tUS\t\tSC\t\t\t\t1\t\t9\tAmerica/New_York\t2011-05-14\n"+
			"2\tNew York City\tNew York City\t\t40.71427\t-74.00597\tP\tPPL\tUS\t\tNY\t\t\t\t1\t\t9\tAmerica/New_York\t2011-05-14\n"),
		os.ModePerm))

	cfg := photoindex.Config{ExiftoolPath: exiftool, VipsPath: "/nonexistent", GeonamesDir: geoDir,
		MLURL: ml.URL, ClipModel: "ViT-B-32__openai", OCRModel: "PP-OCRv5_mobile", FaceMinScore: 0.7,
		FaceMatchThreshold: 0.5}
	if exiftool == "" {
		cfg.ExiftoolPath = "/nonexistent"
	}
	startTestPhotoIndex(t, cfg, []string{home}, 4)
	require.Eventually(t, func() bool {
		return photoindex.Get().Status().Faces.Pending == 0
	}, 30*time.Second, 100*time.Millisecond)
	assert.False(t, photoindex.Get().FacesEnabled(), "no face model configured")

	token, err := getJWTWebClientTokenFromTestServer(defaultUsername, defaultPassword)
	require.NoError(t, err)
	get := func(reqURL string, expected int) []byte {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, reqURL, nil)
		setJWTCookieForReq(req, token)
		rr := executeRequest(req)
		checkResponseCode(t, expected, rr)
		return rr.Body.Bytes()
	}
	search := func(q string, expected int) []map[string]any {
		t.Helper()
		var res []map[string]any
		require.NoError(t, json.Unmarshal(get(webClientSearchPath+"?path=%2F&q="+url.QueryEscape(q), expected), &res))
		return res
	}
	names := func(res []map[string]any) []string {
		var out []string
		for _, r := range res {
			out = append(out, r["name"].(string))
		}
		return out
	}

	// Things pictured: best matches first, only visible files.
	res := search("show:red", http.StatusOK)
	assert.Equal(t, []string{"red.jpg", "red-blue.jpg"}, names(res))
	assert.Equal(t, "2_trip/red-blue.jpg", res[1]["meta"])
	res = search("show:red red-", http.StatusOK)
	assert.Equal(t, []string{"red-blue.jpg"}, names(res))
	ml.Down.Store(true)
	var errResp map[string]any
	require.NoError(t, json.Unmarshal(get(webClientSearchPath+"?path=%2F&q="+url.QueryEscape("show:green"),
		http.StatusBadRequest), &errResp))
	assert.Equal(t, "fs.search.show_unavailable", errResp["message"])
	ml.Down.Store(false)

	// Visible text, with the line that matched.
	res = search("text:blue", http.StatusOK)
	assert.ElementsMatch(t, []string{"blue.jpg", "red-blue.jpg"}, names(res))
	for _, r := range res {
		assert.Equal(t, "Blue sign", r["text"])
	}
	assert.ElementsMatch(t, []string{"red.jpg", "red-blue.jpg"}, names(search(`text:"red sign"`, http.StatusOK)))
	assert.Nil(t, search("show:red", http.StatusOK)[0]["text"])

	// Places.
	page := string(get(webClientFilesPath, http.StatusOK))
	assert.Contains(t, page, `href="/web/client/places"`)
	assert.NotContains(t, page, `href="/web/client/people"`, "faces disabled")
	assert.Contains(t, string(get(webClientPlacesPath, http.StatusOK)), `id="places_map"`)
	var pts struct {
		Points [][3]float64 `json:"points"`
		Files  []string     `json:"files"`
		Places []struct {
			City    string `json:"city"`
			State   string `json:"state"`
			Country string `json:"country"`
			Photos  int    `json:"photos"`
		} `json:"places"`
	}
	require.NoError(t, json.Unmarshal(get(webClientPlacesPath+"/points", http.StatusOK), &pts))
	if exiftool == "" {
		assert.Empty(t, pts.Points)
		return
	}
	require.Len(t, pts.Points, 2, "the photo in the unlistable dir is not shown")
	assert.ElementsMatch(t, []string{"/red.jpg", "/trip/red-blue.jpg"}, pts.Files)
	require.Len(t, pts.Places, 2)
	assert.ElementsMatch(t, []string{"Myrtle Beach", "New York City"}, []string{pts.Places[0].City, pts.Places[1].City})

	res = search("place:myrtle", http.StatusOK)
	assert.Equal(t, []string{"red.jpg"}, names(res))
	assert.Equal(t, "Myrtle Beach, South Carolina, United States", res[0]["place"])
	assert.Equal(t, []string{"red-blue.jpg"}, names(search(`place:"new york" show:red`, http.StatusOK)))
	assert.Empty(t, search("place:paris", http.StatusOK))
}
