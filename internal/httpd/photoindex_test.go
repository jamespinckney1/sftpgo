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
	"image/jpeg"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drakkan/sftpgo/v2/internal/dataprovider"
	"github.com/drakkan/sftpgo/v2/internal/httpdtest"
	"github.com/drakkan/sftpgo/v2/internal/photoindex"
)

const webClientPhotoStatusPath = "/web/client/photoindex/status"

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
