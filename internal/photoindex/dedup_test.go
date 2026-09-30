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
	"database/sql"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/image/draw"
)

// testPattern draws a picture with some structure so different seeds give
// clearly different perceptual hashes.
func testPattern(w, h, seed int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := uint8((x*(seed+3) + y*(seed*7+1) + (x*y*seed)/(w+1)) % 256)
			img.Set(x, y, color.RGBA{v, uint8(255 - int(v)), uint8((x + seed*40) % 256), 255})
		}
	}
	return img
}

func saveJPEG(t *testing.T, p string, img image.Image, quality int) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0755))
	f, err := os.Create(p)
	require.NoError(t, err)
	require.NoError(t, jpeg.Encode(f, img, &jpeg.Options{Quality: quality}))
	require.NoError(t, f.Close())
}

func resized(img image.Image, w, h int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)
	return dst
}

func TestDHash(t *testing.T) {
	a := testPattern(800, 600, 1)
	b := testPattern(800, 600, 5)
	ha := dHash(a)
	assert.LessOrEqual(t, hamming(ha, dHash(resized(a, 200, 150))), 3, "resized copy")
	assert.Greater(t, hamming(ha, dHash(b)), 12, "different picture")
	// JPEG fast path gives the same result as the generic path.
	p := filepath.Join(t.TempDir(), "a.jpg")
	saveJPEG(t, p, a, 60)
	f, err := os.Open(p)
	require.NoError(t, err)
	decoded, err := jpeg.Decode(f)
	f.Close()
	require.NoError(t, err)
	_, isYCbCr := decoded.(*image.YCbCr)
	assert.True(t, isYCbCr)
	assert.LessOrEqual(t, hamming(ha, dHash(decoded)), 3, "re-compressed copy")
	assert.Equal(t, int64(0), dHash(image.NewRGBA(image.Rect(0, 0, 0, 0))))
}

func TestParseSizeAndFilters(t *testing.T) {
	for in, want := range map[string]int64{
		"500": 500, "500b": 500, "1KB": 1024, "1kib": 1024, "1.5MB": 1572864, "2GB": 2 << 30, "1t": 1 << 40,
	} {
		got, err := ParseSize(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "MB", "-1MB", "1XB", "1.2.3"} {
		_, err := ParseSize(bad)
		assert.Error(t, err, bad)
	}

	q, err := ParseQuery("vacation larger:10MB smaller:1GB sort:size")
	require.NoError(t, err)
	assert.False(t, q.Photo, "size filters alone do not need the index")
	assert.True(t, q.HasSizeFilter())
	assert.Equal(t, "vacation", q.Text)
	assert.Equal(t, int64(10<<20), q.MinSize)
	assert.Equal(t, int64(1<<30), q.MaxSize)
	assert.True(t, q.SortBySize)
	assert.True(t, q.SizeMatches(20<<20))
	assert.False(t, q.SizeMatches(5<<20))
	assert.False(t, q.SizeMatches(2<<30))

	q, err = ParseQuery("is:duplicate")
	require.NoError(t, err)
	assert.True(t, q.Photo)
	assert.Equal(t, DupExact, q.Duplicates)
	q, err = ParseQuery("IS:Similar taken:2020")
	require.NoError(t, err)
	assert.Equal(t, DupSimilar, q.Duplicates)
	assert.Equal(t, "2020-01-01 00:00:00", q.filter.from)

	q, err = ParseQuery("note: todo")
	require.NoError(t, err)
	assert.Equal(t, "note: todo", q.Text, "unknown keys are plain text")

	for _, bad := range []string{"is:blurry", "sort:name", "larger:big", "smaller:"} {
		_, err := ParseQuery(bad)
		assert.Error(t, err, bad)
	}
}

func TestSimilarGuards(t *testing.T) {
	assert.True(t, featureless(0))
	assert.True(t, featureless(-1))
	assert.False(t, featureless(0x0F0F0F0F0F0F0F0F))
	dims := func(w, h int) *DupFile { return &DupFile{Media: Media{Width: w, Height: h}} }
	assert.True(t, sameAspect(dims(4032, 3024), dims(1024, 768)))
	assert.True(t, sameAspect(dims(4032, 3024), dims(0, 0)), "unknown dimensions")
	assert.False(t, sameAspect(dims(4032, 3024), dims(3024, 4032)))
	assert.False(t, sameAspect(dims(1600, 900), dims(1200, 900)))

	// Two unrelated but featureless pictures and a portrait/landscape pair
	// with matching hashes are not grouped.
	h := int64(0x0F0F0F0F0F0F0F0F)
	zero := int64(0)
	files := []DupFile{
		{Media: Media{Path: "/a/1.jpg", PHash: &zero}},
		{Media: Media{Path: "/b/2.jpg", PHash: &zero}},
		{Media: Media{Path: "/a/3.jpg", PHash: &h, Width: 1200, Height: 800}},
		{Media: Media{Path: "/b/4.jpg", PHash: &h, Width: 800, Height: 1200}},
		{Media: Media{Path: "/c/5.jpg", PHash: &h, Width: 600, Height: 400}},
	}
	groups := groupSimilar(files, similarMaxDistance)
	require.Len(t, groups, 1)
	assert.ElementsMatch(t, []int{2, 4}, groups[0])
}

func TestIntentionalPair(t *testing.T) {
	f := func(p string) *DupFile { return &DupFile{Media: Media{Path: p}} }
	assert.True(t, intentionalPair(f("/a/IMG_1.CR2"), f("/a/IMG_1.JPG")))
	assert.True(t, intentionalPair(f("/a/IMG_1.heic"), f("/a/img_1.jpg")))
	assert.False(t, intentionalPair(f("/a/IMG_1.jpg"), f("/b/IMG_1.jpg")))
	assert.False(t, intentionalPair(f("/a/IMG_1.jpg"), f("/a/IMG_2.jpg")))
	assert.False(t, intentionalPair(f("/a/IMG_1.jpg"), f("/b/IMG_1.CR2")))
}

func allVisible(p string) (string, bool) { return p, true }

func TestFindDuplicates(t *testing.T) {
	root := filepath.Join(t.TempDir(), "photos")
	a := testPattern(640, 480, 1)
	b := testPattern(640, 480, 9)
	saveJPEG(t, filepath.Join(root, "orig", "IMG_1.jpg"), a, 90)
	data, err := os.ReadFile(filepath.Join(root, "orig", "IMG_1.jpg"))
	require.NoError(t, err)
	// Two byte-identical copies, one visible later only to "another user".
	require.NoError(t, os.MkdirAll(filepath.Join(root, "backup"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "backup", "IMG_1 (1).jpg"), data, 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "other"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "other", "copy.jpg"), data, 0644))
	// A smaller re-encoded copy (similar, not exact).
	saveJPEG(t, filepath.Join(root, "whatsapp", "IMG-WA0001.jpg"), resized(a, 320, 240), 50)
	// Same picture as PNG next to the original: an intentional pair.
	f, err := os.Create(filepath.Join(root, "orig", "IMG_1.png"))
	require.NoError(t, err)
	require.NoError(t, png.Encode(f, resized(a, 320, 240)))
	require.NoError(t, f.Close())
	// An unrelated photo.
	saveJPEG(t, filepath.Join(root, "orig", "IMG_2.jpg"), b, 90)

	m := newTestManager(t, root, Config{ExiftoolPath: "/nonexistent", VipsPath: "/nonexistent"})
	m.refreshRoots()
	m.enqueueTree(root)
	drain(t, m)

	// Perceptual hashes are computed while indexing (from the source here,
	// since there is no libvips); content hashes only for same-size files.
	cands, err := m.store.hashCandidates(100)
	require.NoError(t, err)
	assert.Len(t, cands, 3, "only the three same-size copies need hashing")
	n, err := m.dupBatch()
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	pending, err := m.store.pendingDuplicateWork()
	require.NoError(t, err)
	assert.Zero(t, pending)

	q, _ := ParseQuery("is:duplicate")
	groups, err := m.FindDuplicates([]string{root}, q, allVisible)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	g := groups[0]
	assert.True(t, g.Exact)
	require.Len(t, g.Files, 3)
	assert.True(t, g.Files[0].Keep)
	assert.False(t, g.Files[1].Keep)
	assert.Equal(t, 2*int64(len(data)), g.Reclaimable)

	// A user who cannot see "other" gets a group of two.
	hideOther := func(p string) (string, bool) {
		return p, !isInside(p, filepath.Join(root, "other"))
	}
	groups, err = m.FindDuplicates([]string{root}, q, hideOther)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.Len(t, groups[0].Files, 2)
	// A user who sees a single copy has no duplicates.
	onlyOrig := func(p string) (string, bool) { return p, isInside(p, filepath.Join(root, "orig")) }
	groups, err = m.FindDuplicates([]string{root}, q, onlyOrig)
	require.NoError(t, err)
	assert.Empty(t, groups)

	// Similar: the exact copies, the re-encoded one and the PNG (through the
	// copies), not the unrelated photo.
	q, _ = ParseQuery("is:similar")
	groups, err = m.FindDuplicates([]string{root}, q, allVisible)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	g = groups[0]
	assert.False(t, g.Exact)
	kept := map[string]bool{}
	for _, f := range g.Files {
		kept[filepath.Base(f.Path)] = f.Keep
	}
	assert.Len(t, kept, 5)
	assert.NotContains(t, kept, "IMG_2.jpg")
	assert.True(t, g.Files[0].Keep)
	assert.NotEqual(t, "IMG-WA0001.jpg", filepath.Base(g.Files[0].Path), "the small copy is never the one to keep")
	assert.False(t, kept["IMG-WA0001.jpg"])
	// Highest resolution, then oldest, then shortest path.
	assert.Equal(t, filepath.Join(root, "orig", "IMG_1.jpg"), g.Files[0].Path)
	assert.Equal(t, 640, g.Files[0].Width)
	assert.True(t, kept["IMG_1.png"], "the PNG next to the kept original is kept with it")
	assert.False(t, kept["copy.jpg"])
	assert.False(t, kept["IMG_1 (1).jpg"])

	// Deleted copies disappear from the groups even before the index catches up.
	require.NoError(t, os.Remove(filepath.Join(root, "other", "copy.jpg")))
	require.NoError(t, os.Remove(filepath.Join(root, "backup", "IMG_1 (1).jpg")))
	q, _ = ParseQuery("is:duplicate")
	groups, err = m.FindDuplicates([]string{root}, q, allVisible)
	require.NoError(t, err)
	assert.Empty(t, groups)

	// Modifying a file resets its hashes.
	p := filepath.Join(root, "orig", "IMG_2.jpg")
	saveJPEG(t, p, a, 70)
	m.enqueueTree(p)
	drain(t, m)
	rec, found, err := m.store.get(p)
	require.NoError(t, err)
	require.True(t, found)
	assert.Empty(t, rec.Hash)
	require.NotNil(t, rec.PHash)
}

func TestSizeQuery(t *testing.T) {
	root := filepath.Join(t.TempDir(), "photos")
	saveJPEG(t, filepath.Join(root, "small.jpg"), testPattern(64, 48, 1), 50)
	saveJPEG(t, filepath.Join(root, "big.jpg"), testPattern(1200, 900, 2), 95)
	m := newTestManager(t, root, Config{ExiftoolPath: "/nonexistent", VipsPath: "/nonexistent"})
	m.refreshRoots()
	m.enqueueTree(root)
	drain(t, m)

	res := searchAll(t, m, []string{root}, "taken:any sort:size")
	require.Len(t, res, 2)
	assert.Equal(t, "big.jpg", filepath.Base(res[0].Path))
	res = searchAll(t, m, []string{root}, "taken:any larger:20KB")
	require.Len(t, res, 1)
	assert.Equal(t, "big.jpg", filepath.Base(res[0].Path))
	res = searchAll(t, m, []string{root}, "taken:any smaller:20KB")
	require.Len(t, res, 1)
	assert.Equal(t, "small.jpg", filepath.Base(res[0].Path))
}

func TestSchemaUpgrade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	// Create a version 1 database with a row, as written by the first release.
	db, err := sql.Open("sqlite3", "file:"+dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL); INSERT INTO schema_version VALUES (1);`)
	require.NoError(t, err)
	_, err = db.Exec(migrations[0])
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO media (path, size, mtime, kind, taken, taken_src, indexed_at)
		VALUES ('/p/a.jpg', 10, 1, 'image', '2020-01-01 00:00:00', 'exif', 1)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	s, err := openStore(dbPath)
	require.NoError(t, err)
	defer s.close()
	rec, found, err := s.get("/p/a.jpg")
	require.NoError(t, err)
	require.True(t, found)
	assert.Empty(t, rec.Hash)
	assert.Nil(t, rec.PHash)
	cands, err := s.phashCandidates(10)
	require.NoError(t, err)
	assert.Len(t, cands, 1, "existing photos get a perceptual hash after the upgrade")
}
