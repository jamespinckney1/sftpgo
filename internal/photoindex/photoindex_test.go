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
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sftpgo/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drakkan/sftpgo/v2/internal/dataprovider"
	"github.com/drakkan/sftpgo/v2/internal/vfs"
)

func TestParseExifDate(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		offset string
		ok     bool
	}{
		{"2023:06:14 10:22:33", "2023-06-14 10:22:33", "", true},
		{"2023:06:14 10:22:33.123+02:00", "2023-06-14 10:22:33", "+02:00", true},
		{"2023-06-14T10:22:33Z", "2023-06-14 10:22:33", "Z", true},
		{"2023:06:14 10:22:33-0400", "2023-06-14 10:22:33", "-04:00", true},
		{"2023:06:14", "2023-06-14 00:00:00", "", true},
		{"0000:00:00 00:00:00", "", "", false},
		{"1970:01:01 00:00:00", "", "", false},
		{"2023:13:01 00:00:00", "", "", false},
		{"", "", "", false},
		{"garbage", "", "", false},
	}
	for _, c := range cases {
		d, ok := parseExifDate(c.in)
		assert.Equal(t, c.ok, ok, c.in)
		if ok {
			assert.Equal(t, c.want, d.String(), c.in)
			assert.Equal(t, c.offset, d.offset, c.in)
		}
	}
}

func TestDateFromFilename(t *testing.T) {
	cases := map[string]string{
		"IMG_20230614_102233.jpg":            "2023-06-14 10:22:33",
		"PXL_20230614_102233123.jpg":         "2023-06-14 10:22:33",
		"2023-06-14 10.22.33.jpg":            "2023-06-14 10:22:33",
		"Screenshot_2023-06-14-10-22-33.png": "2023-06-14 10:22:33",
		"IMG-20230614-WA0001.jpg":            "2023-06-14 00:00:00",
		"/photos/1999/scan 1999-12-31.png":   "1999-12-31 00:00:00",
	}
	for name, want := range cases {
		d, ok := dateFromFilename(name)
		if assert.True(t, ok, name) {
			assert.Equal(t, want, d.String(), name)
		}
	}
	for _, name := range []string{"IMG_1234.JPG", "DSC00001.jpg", "photo 20231345.jpg", "1686739353000.jpg"} {
		_, ok := dateFromFilename(name)
		assert.False(t, ok, name)
	}
}

func TestResolveMetadata(t *testing.T) {
	mtime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	// EXIF wins over everything.
	md := resolveMetadata(exifFields{
		"EXIF:DateTimeOriginal":   "2021:07:04 18:30:00",
		"EXIF:OffsetTimeOriginal": "-04:00",
		"EXIF:CreateDate":         "2021:07:05 00:00:00",
		"File:ImageWidth":         float64(1200),
		"File:ImageHeight":        float64(900),
		"EXIF:Orientation":        float64(6),
		"Composite:GPSLatitude":   40.7,
		"Composite:GPSLongitude":  -74.0,
		"EXIF:Make":               "Apple",
		"EXIF:Model":              "iPhone 12",
	}, "IMG_20200101_000000.jpg", mtime)
	assert.Equal(t, "2021-07-04 18:30:00", md.taken.String())
	assert.Equal(t, "-04:00", md.taken.offset)
	assert.Equal(t, TakenSrcExif, md.takenSrc)
	assert.Equal(t, 900, md.width)
	assert.Equal(t, 1200, md.height)
	require.NotNil(t, md.lat)
	assert.InDelta(t, -74.0, *md.lon, 0.0001)
	assert.Equal(t, "Apple iPhone 12", md.camera)

	// Apple video keys carry local time with the offset.
	md = resolveMetadata(exifFields{
		"QuickTime:CreationDate": "2022:08:01 09:15:00-07:00",
		"QuickTime:CreateDate":   "2022:08:01 16:15:00",
	}, "clip.mov", mtime)
	assert.Equal(t, "2022-08-01 09:15:00", md.taken.String())
	assert.Equal(t, TakenSrcVideo, md.takenSrc)

	// QuickTime CreateDate alone is UTC and converted to local time.
	md = resolveMetadata(exifFields{"QuickTime:CreateDate": "2022:08:01 16:15:00"}, "clip.mp4", mtime)
	want := time.Date(2022, 8, 1, 16, 15, 0, 0, time.UTC).In(time.Local).Format(takenLayout)
	assert.Equal(t, want, md.taken.String())

	// Fallbacks: file name, then modification time.
	md = resolveMetadata(nil, "IMG_20200101_120000.jpg", mtime)
	assert.Equal(t, TakenSrcFilename, md.takenSrc)
	assert.Equal(t, "2020-01-01 12:00:00", md.taken.String())
	md = resolveMetadata(exifFields{"EXIF:DateTimeOriginal": "0000:00:00 00:00:00"}, "a.jpg", mtime)
	assert.Equal(t, TakenSrcModTime, md.takenSrc)
	assert.Equal(t, mtime.In(time.Local).Format(takenLayout), md.taken.String())

	assert.Equal(t, "Canon EOS 80D", cameraName("Canon", "Canon EOS 80D"))
	assert.Equal(t, "NIKON D750", cameraName("NIKON CORPORATION", "NIKON D750"))
	assert.Equal(t, "SONY ILCE-7M3", cameraName("SONY", "ILCE-7M3"))
}

func TestParseQuery(t *testing.T) {
	q, err := ParseQuery("beach taken:2023")
	require.NoError(t, err)
	assert.True(t, q.Photo)
	assert.Equal(t, "beach", q.Text)
	assert.Equal(t, "2023-01-01 00:00:00", q.filter.from)
	assert.Equal(t, "2024-01-01 00:00:00", q.filter.to)

	q, err = ParseQuery("Taken:2023-06..2023-08")
	require.NoError(t, err)
	assert.Equal(t, "2023-06-01 00:00:00", q.filter.from)
	assert.Equal(t, "2023-09-01 00:00:00", q.filter.to)

	q, err = ParseQuery("taken:..2010-12-31")
	require.NoError(t, err)
	assert.Empty(t, q.filter.from)
	assert.Equal(t, "2011-01-01 00:00:00", q.filter.to)

	q, err = ParseQuery("taken:2020..")
	require.NoError(t, err)
	assert.Equal(t, "2020-01-01 00:00:00", q.filter.from)
	assert.Empty(t, q.filter.to)

	q, err = ParseQuery("taken:any *.heic")
	require.NoError(t, err)
	assert.True(t, q.Photo)
	assert.Equal(t, "*.heic", q.Text)

	q, err = ParseQuery("taken:unknown")
	require.NoError(t, err)
	assert.True(t, q.filter.unknownOnly)

	q, err = ParseQuery("holiday 2023")
	require.NoError(t, err)
	assert.False(t, q.Photo)
	assert.Equal(t, "holiday 2023", q.Text)

	for _, bad := range []string{"taken:2023-13", "taken:2023-02-30", "taken:..", "taken:yesterday", "taken:"} {
		_, err = ParseQuery(bad)
		assert.Error(t, err, bad)
	}
}

func TestNormalizeRoots(t *testing.T) {
	roots := normalizeRoots([]string{"/srv/data/bob", "/srv/data", "/srv/data2", "relative", "", "/srv/data/"})
	assert.Equal(t, []string{"/srv/data", "/srv/data2"}, roots)
	assert.True(t, isInside("/srv/data/a.jpg", "/srv/data"))
	assert.False(t, isInside("/srv/data2/a.jpg", "/srv/data"))
}

func TestUserScope(t *testing.T) {
	user := &dataprovider.User{}
	user.Username = "alice"
	user.HomeDir = "/srv/data/alice"
	user.FsConfig.Provider = sdk.LocalFilesystemProvider
	user.Permissions = map[string][]string{
		"/":        {dataprovider.PermAny},
		"/private": {dataprovider.PermUpload},
	}
	user.Filters.FilePatterns = []sdk.PatternsFilter{
		{Path: "/", DeniedPatterns: []string{"*.secret.jpg"}, DenyPolicy: sdk.DenyPolicyHide},
	}
	user.VirtualFolders = []vfs.VirtualFolder{
		{
			BaseVirtualFolder: vfs.BaseVirtualFolder{
				Name: "family", MappedPath: "/srv/shared/family",
				FsConfig: vfs.Filesystem{Provider: sdk.LocalFilesystemProvider},
			},
			VirtualPath: "/Family",
		},
		{
			BaseVirtualFolder: vfs.BaseVirtualFolder{
				Name: "cloud", MappedPath: "/srv/cloud",
				FsConfig: vfs.Filesystem{Provider: sdk.S3FilesystemProvider},
			},
			VirtualPath: "/Cloud",
		},
	}

	s := NewUserScope(user, "/")
	assert.Equal(t, []string{"/srv/data/alice", "/srv/shared/family"}, s.Dirs)

	v, ok := s.VirtualPath("/srv/data/alice/2023/a.jpg")
	assert.True(t, ok)
	assert.Equal(t, "/2023/a.jpg", v)
	v, ok = s.VirtualPath("/srv/shared/family/trip/b.heic")
	assert.True(t, ok)
	assert.Equal(t, "/Family/trip/b.heic", v)
	// A file in the home dir hidden behind the virtual folder is not visible.
	_, ok = s.VirtualPath("/srv/data/alice/Family/c.jpg")
	assert.False(t, ok)
	_, ok = s.VirtualPath("/srv/data/bob/d.jpg")
	assert.False(t, ok)

	assert.True(t, s.Visible("/2023/a.jpg"))
	assert.False(t, s.Visible("/private/e.jpg"))
	assert.False(t, s.Visible("/2023/x.secret.jpg"))

	// Scoped to a subdirectory of the virtual folder.
	s = NewUserScope(user, "/Family/trip")
	assert.Equal(t, []string{"/srv/shared/family/trip"}, s.Dirs)
	_, ok = s.VirtualPath("/srv/shared/family/other/f.jpg")
	assert.False(t, ok)
	v, ok = s.VirtualPath("/srv/shared/family/trip/b.heic")
	assert.True(t, ok)
	assert.Equal(t, "/Family/trip/b.heic", v)

	// Scoped to a home subdirectory.
	s = NewUserScope(user, "/2023")
	assert.Equal(t, []string{"/srv/data/alice/2023"}, s.Dirs)
}

func writeJPEG(t *testing.T, p string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0755))
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for x := range 64 {
		for y := range 48 {
			img.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 5), 128, 255})
		}
	}
	f, err := os.Create(p)
	require.NoError(t, err)
	require.NoError(t, jpeg.Encode(f, img, nil))
	require.NoError(t, f.Close())
}

func newTestManager(t *testing.T, root string, cfg Config) *Manager {
	t.Helper()
	cfg.Enabled = true
	cfg.DataDir = filepath.Join(t.TempDir(), "index")
	m, err := newManager(cfg, "", func() ([]string, error) { return []string{root}, nil })
	require.NoError(t, err)
	t.Cleanup(func() {
		if m.exif != nil {
			m.exif.close()
		}
		m.store.close() //nolint:errcheck
	})
	return m
}

// drain processes the queued files synchronously.
func drain(t *testing.T, m *Manager) {
	t.Helper()
	for {
		select {
		case p := <-m.prioQueue:
			require.NoError(t, m.processFile(p))
		case p := <-m.backfillQueue:
			require.NoError(t, m.processFile(p))
		default:
			return
		}
	}
}

func searchAll(t *testing.T, m *Manager, dirs []string, q string) []Media {
	t.Helper()
	parsed, err := ParseQuery(q)
	require.NoError(t, err)
	var res []Media
	require.NoError(t, m.Search(dirs, parsed, func(rec Media) bool {
		res = append(res, rec)
		return true
	}))
	return res
}

func TestIndexerLifecycle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "photos")
	writeJPEG(t, filepath.Join(root, "2019", "IMG_20190501_101010.jpg"))
	writeJPEG(t, filepath.Join(root, "2023", "IMG_20230614_120000.jpg"))
	writeJPEG(t, filepath.Join(root, "misc", "nodate.jpg"))
	writeJPEG(t, filepath.Join(root, ".hidden", "IMG_20230101_000000.jpg"))
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0644))

	// Tools disabled so the test does not depend on the environment.
	m := newTestManager(t, root, Config{ExiftoolPath: "/nonexistent", VipsPath: "/nonexistent"})
	m.refreshRoots()

	todo, ok := m.scanRoot(root)
	require.True(t, ok)
	assert.Len(t, todo, 3, "hidden directories and other files are skipped")
	for _, p := range todo {
		require.NoError(t, m.processFile(p))
	}

	st := m.Status()
	assert.Equal(t, int64(3), st.Total)
	assert.Equal(t, int64(2), st.WithDate)

	res := searchAll(t, m, []string{root}, "taken:2023")
	require.Len(t, res, 1)
	assert.Equal(t, "2023-06-14 12:00:00", res[0].Taken)
	assert.Equal(t, TakenSrcFilename, res[0].TakenSrc)

	res = searchAll(t, m, []string{root}, "taken:any")
	require.Len(t, res, 3)
	// Newest first; the undated file uses today's modification time.
	assert.Equal(t, "nodate.jpg", filepath.Base(res[0].Path))
	assert.Equal(t, "IMG_20190501_101010.jpg", filepath.Base(res[2].Path))

	res = searchAll(t, m, []string{root}, "taken:unknown")
	require.Len(t, res, 1)
	res = searchAll(t, m, []string{filepath.Join(root, "2019")}, "taken:any")
	require.Len(t, res, 1)

	// Rename a directory: rows move, no reprocessing needed.
	require.NoError(t, os.Rename(filepath.Join(root, "2019"), filepath.Join(root, "old")))
	oldID := res[0].ID
	oldIndexedAt := res[0].IndexedAt
	m.handleEvent(fsEvent{op: OpRename, path: filepath.Join(root, "2019"), target: filepath.Join(root, "old")})
	drain(t, m)
	res = searchAll(t, m, []string{filepath.Join(root, "old")}, "taken:2019")
	require.Len(t, res, 1)
	assert.Equal(t, oldID, res[0].ID)
	assert.Equal(t, oldIndexedAt, res[0].IndexedAt, "unchanged files must not be processed again")

	// Upload event.
	up := filepath.Join(root, "new", "IMG_20240101_000000.jpg")
	writeJPEG(t, up)
	m.handleEvent(fsEvent{op: OpUpload, path: up})
	drain(t, m)
	assert.Len(t, searchAll(t, m, []string{root}, "taken:2024"), 1)

	// Delete event.
	require.NoError(t, os.Remove(up))
	m.handleEvent(fsEvent{op: OpDelete, path: up})
	assert.Empty(t, searchAll(t, m, []string{root}, "taken:2024"))

	// A file deleted outside SFTPGo is removed by the next scan.
	require.NoError(t, os.Remove(filepath.Join(root, "misc", "nodate.jpg")))
	todo, ok = m.scanRoot(root)
	assert.True(t, ok)
	assert.Empty(t, todo)
	assert.Equal(t, int64(2), m.Status().Total)

	// Files below an entry that could not be read are kept, the others are
	// removed.
	known, err := m.store.signatures(root)
	require.NoError(t, err)
	m.pruneDeleted(root, known, walkResult{seen: map[string]bool{}, unreadable: []string{filepath.Join(root, "old")}})
	res = searchAll(t, m, []string{root}, "taken:any")
	require.Len(t, res, 1)
	assert.Equal(t, filepath.Join(root, "old", "IMG_20190501_101010.jpg"), res[0].Path)
	writeJPEG(t, filepath.Join(root, "2023", "IMG_20230614_120000.jpg"))
	todo, _ = m.scanRoot(root)
	require.Len(t, todo, 1)
	require.NoError(t, m.processFile(todo[0]))
	assert.Equal(t, int64(2), m.Status().Total)

	// An unreachable root (unmounted disk) keeps its entries.
	require.NoError(t, os.Rename(root, root+".moved"))
	_, ok = m.scanRoot(root)
	assert.True(t, ok)
	assert.Equal(t, int64(2), m.Status().Total)
	require.NoError(t, os.Rename(root+".moved", root))

	// Orphans outside every root are removed.
	m.removeOrphans([]string{filepath.Join(root, "old")})
	assert.Equal(t, int64(1), m.Status().Total)
}

func TestIndexerWithTools(t *testing.T) {
	exiftoolPath, err := exec.LookPath("exiftool")
	if err != nil {
		t.Skip("exiftool not available")
	}
	vipsPath, err := exec.LookPath("vipsthumbnail")
	if err != nil {
		t.Skip("vipsthumbnail not available")
	}
	root := filepath.Join(t.TempDir(), "photos")
	jpg := filepath.Join(root, "a.jpg")
	writeJPEG(t, jpg)
	out, err := exec.Command(exiftoolPath, "-q", "-overwrite_original", "-DateTimeOriginal=2021:07:04 18:30:00",
		"-OffsetTimeOriginal=-04:00", "-Make=Apple", "-Model=iPhone 12", jpg).CombinedOutput()
	require.NoError(t, err, string(out))
	png := filepath.Join(root, "b.png")
	out, err = exec.Command("vips", "copy", jpg, png).CombinedOutput()
	if err != nil {
		t.Skipf("vips not available: %s", out)
	}
	heic := filepath.Join(root, "c.heic")
	haveHEIC := exec.Command("heif-enc", "-q", "50", jpg, "-o", heic).Run() == nil

	m := newTestManager(t, root, Config{ExiftoolPath: exiftoolPath, VipsPath: vipsPath, PreviewSize: 256})
	m.refreshRoots()
	m.enqueueTree(root)
	drain(t, m)

	info, err := os.Stat(jpg)
	require.NoError(t, err)
	rec, ok := m.Lookup(jpg, info.ModTime(), info.Size())
	require.True(t, ok)
	assert.Equal(t, "2021-07-04 18:30:00", rec.Taken)
	assert.Equal(t, TakenSrcExif, rec.TakenSrc)
	assert.Equal(t, "-04:00", rec.TZOffset)
	assert.Equal(t, "Apple iPhone 12", rec.Camera)
	assert.Equal(t, 64, rec.Width)
	assert.Empty(t, rec.Error)
	p, ok := m.PreviewPath(jpg, info.ModTime(), info.Size())
	require.True(t, ok)
	f, err := os.Open(p)
	require.NoError(t, err)
	cfg, err := jpeg.DecodeConfig(f)
	f.Close()
	require.NoError(t, err)
	assert.Equal(t, 64, cfg.Width, "previews are never upscaled")

	info, err = os.Stat(png)
	require.NoError(t, err)
	_, ok = m.PreviewPath(png, info.ModTime(), info.Size())
	assert.True(t, ok)

	if haveHEIC {
		info, err = os.Stat(heic)
		require.NoError(t, err)
		rec, ok := m.Lookup(heic, info.ModTime(), info.Size())
		require.True(t, ok)
		assert.Equal(t, "2021-07-04 18:30:00", rec.Taken)
		_, ok = m.PreviewPath(heic, info.ModTime(), info.Size())
		assert.True(t, ok, rec.Error)
		data, err := m.RenderJPEG(heic, 32)
		require.NoError(t, err)
		assert.Equal(t, []byte{0xFF, 0xD8}, data[:2])
	}

	// Deleting removes the preview file too.
	info, err = os.Stat(jpg)
	require.NoError(t, err)
	p, _ = m.PreviewPath(jpg, info.ModTime(), info.Size())
	require.NoError(t, m.removeTree(jpg))
	assert.NoFileExists(t, p)
}

func TestEventOverflow(t *testing.T) {
	root := filepath.Join(t.TempDir(), "photos")
	keep := filepath.Join(root, "IMG_20230614_120000.jpg")
	gone := filepath.Join(root, "big", "IMG_20220101_000000.jpg")
	writeJPEG(t, keep)
	writeJPEG(t, gone)
	m := newTestManager(t, root, Config{ExiftoolPath: "/nonexistent", VipsPath: "/nonexistent"})
	m.refreshRoots()
	todo, _ := m.scanRoot(root)
	for _, p := range todo {
		require.NoError(t, m.processFile(p))
	}
	require.Equal(t, int64(2), m.Status().Total)

	// A large delete fills the queue: the last events, including the folder
	// itself, must not be lost.
	for i := range eventQueueSize + 10 {
		m.OnFsEvent(OpDelete, filepath.Join(root, "big", fmt.Sprintf("other%d.jpg", i)), "")
	}
	m.OnFsEvent(OpRmdir, filepath.Join(root, "big"), "")
	assert.Len(t, m.evOverflow, 11)
	m.drainEvents()
	assert.Empty(t, m.evOverflow)
	assert.Empty(t, m.events)
	assert.Equal(t, int64(1), m.Status().Total)
}
