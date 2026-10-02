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
	"context"
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drakkan/sftpgo/v2/internal/photoindex/mltest"
)

// facePhoto writes a photo with a face (a colored square, see mltest) for
// each identity, at distinct positions.
func facePhoto(t *testing.T, p string, size int, identities ...int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{128, 128, 128, 255}}, image.Point{}, draw.Src)
	for i, id := range identities {
		mltest.DrawFace(img, id, 30+i*150, 100+i*20, size)
	}
	saveJPEG(t, p, img, 95)
}

// indexWithPreviews indexes root and, since tests may run without libvips,
// uses each photo itself as its preview.
func indexWithPreviews(t *testing.T, m *Manager, root string) {
	t.Helper()
	m.refreshRoots()
	m.enqueueTree(root)
	drain(t, m)
	err := m.store.allPaths(func(id int64, p string) {
		dst := m.previewPath(id)
		require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0700))
		data, err := os.ReadFile(p)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(dst, data, 0600))
	})
	require.NoError(t, err)
	_, err = m.store.db.Exec(`UPDATE media SET has_preview = 1`)
	require.NoError(t, err)
}

func searchNames(t *testing.T, m *Manager, root, q string) []string {
	t.Helper()
	var names []string
	for _, r := range searchAll(t, m, []string{root}, q) {
		names = append(names, filepath.Base(r.Path))
	}
	sort.Strings(names)
	return names
}

func TestParseEmbedding(t *testing.T) {
	v, err := parseEmbedding([]byte(`"[3, 4]"`))
	require.NoError(t, err)
	assert.InDelta(t, 0.6, v[0], 1e-6)
	assert.InDelta(t, 0.8, v[1], 1e-6)
	v, err = parseEmbedding([]byte(`[0, 2]`))
	require.NoError(t, err)
	assert.InDelta(t, 1, v[1], 1e-6)
	_, err = parseEmbedding([]byte(`"[]"`))
	assert.Error(t, err)
	_, err = parseEmbedding([]byte(`"oops"`))
	assert.Error(t, err)
	assert.Equal(t, []float32{1.5, -2}, decodeEmbedding(encodeEmbedding([]float32{1.5, -2})))
}

func TestPersonQuery(t *testing.T) {
	q, err := ParseQuery(`person:"Grandma  Rose" beach person:bob taken:2020`)
	require.NoError(t, err)
	assert.True(t, q.Photo)
	assert.Equal(t, []string{"Grandma  Rose", "bob"}, q.PersonTerms)
	assert.Equal(t, "beach", q.Text)
	_, err = ParseQuery(`person:`)
	assert.Error(t, err)
	assert.Equal(t, []string{"a", "b c", "d"}, splitQuery(`a "b c"  d`))
}

func TestFaces(t *testing.T) {
	ml := mltest.NewServer()
	defer ml.Close()

	root := filepath.Join(t.TempDir(), "photos")
	const red, blue, green = 0, 1, 2
	facePhoto(t, filepath.Join(root, "p1.jpg"), 60, red, blue)
	facePhoto(t, filepath.Join(root, "p2.jpg"), 70, red)
	facePhoto(t, filepath.Join(root, "p3.jpg"), 60, blue, green)
	facePhoto(t, filepath.Join(root, "tiny.jpg"), 20, red) // too small to recognize
	facePhoto(t, filepath.Join(root, "empty.jpg"), 60)

	m := newTestManager(t, root, Config{ExiftoolPath: "/nonexistent", VipsPath: "/nonexistent",
		MLURL: ml.URL, FaceModel: "buffalo_l", FaceMinScore: 0.7, FaceMatchThreshold: 0.5})
	require.True(t, m.FacesEnabled())
	indexWithPreviews(t, m, root)
	require.NoError(t, m.loadClusters())

	assert.Equal(t, int64(5), m.faceStatus().Pending)
	n, err := m.mlBatch()
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	st := m.faceStatus()
	assert.Zero(t, st.Pending)
	assert.Equal(t, int64(5), st.Faces)

	all := func(string) bool { return true }
	people, err := m.People(all, 1)
	require.NoError(t, err)
	require.Len(t, people, 3, "red, blue and green are grouped automatically")
	byFaces := map[int]int{}
	for _, p := range people {
		assert.Empty(t, p.Name)
		byFaces[p.Faces]++
		assert.NotZero(t, p.Cover)
	}
	assert.Equal(t, map[int]int{2: 2, 1: 1}, byFaces)
	people, err = m.People(all, 2)
	require.NoError(t, err)
	assert.Len(t, people, 2, "single-face unnamed groups can be filtered out")

	// Identify each group by the photos it appears in.
	groupPhotos := func(id int64) string {
		t.Helper()
		faces, err := m.PersonFaces(id, all)
		require.NoError(t, err)
		var names []string
		for _, f := range faces {
			names = append(names, filepath.Base(f.Path))
		}
		sort.Strings(names)
		return strings.Join(names, ",")
	}
	people, err = m.People(all, 1)
	require.NoError(t, err)
	var redID, blueID, greenID int64
	for _, p := range people {
		switch groupPhotos(p.ID) {
		case "p1.jpg,p2.jpg":
			redID = p.ID
		case "p1.jpg,p3.jpg":
			blueID = p.ID
		case "p3.jpg":
			greenID = p.ID
		}
	}
	require.NotZero(t, redID)
	require.NotZero(t, blueID)
	require.NotZero(t, greenID)

	// Naming and searching.
	id, err := m.RenamePerson(redID, "  Grandma   Rose ")
	require.NoError(t, err)
	assert.Equal(t, redID, id)
	p, found, err := m.GetPerson(redID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "Grandma Rose", p.Name)
	_, err = m.RenamePerson(blueID, "Bob")
	require.NoError(t, err)
	assert.Equal(t, []string{"p1.jpg", "p2.jpg"}, searchNames(t, m, root, "person:rose"))
	assert.Equal(t, []string{"p1.jpg", "p2.jpg"}, searchNames(t, m, root, `person:"grandma rose"`))
	assert.Equal(t, []string{"p1.jpg"}, searchNames(t, m, root, "person:rose person:bob"))
	assert.Equal(t, []string{"p1.jpg", "p3.jpg"}, searchNames(t, m, root, "person:#"+itoa(blueID)))
	assert.Empty(t, searchNames(t, m, root, "person:nobody"))
	names, err := m.PersonNames()
	require.NoError(t, err)
	assert.Equal(t, []string{"Bob", "Grandma Rose"}, names)
	_, err = m.RenamePerson(blueID, " ")
	assert.ErrorIs(t, err, ErrNameRequired)

	// Naming a group like an existing person merges them.
	id, err = m.RenamePerson(greenID, "grandma rose")
	require.NoError(t, err)
	assert.Equal(t, redID, id)
	_, found, err = m.GetPerson(greenID)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, []string{"p1.jpg", "p2.jpg", "p3.jpg"}, searchNames(t, m, root, "person:rose"))

	// "Not this person": the green face goes back out, locked.
	faces, err := m.PersonFaces(redID, all)
	require.NoError(t, err)
	require.Len(t, faces, 3)
	var greenFace int64
	for _, f := range faces {
		if filepath.Base(f.Path) == "p3.jpg" {
			greenFace = f.ID
		}
	}
	_, err = m.MoveFaces([]int64{greenFace}, "")
	require.NoError(t, err)
	f, found, err := m.GetFace(greenFace)
	require.NoError(t, err)
	require.True(t, found)
	assert.Zero(t, f.PersonID)
	assert.True(t, f.Locked)
	assert.Equal(t, []string{"p1.jpg", "p2.jpg"}, searchNames(t, m, root, "person:rose"))
	// ...and then to a new person by name.
	carolID, err := m.MoveFaces([]int64{greenFace}, "Carol")
	require.NoError(t, err)
	assert.NotZero(t, carolID)
	assert.Equal(t, []string{"p3.jpg"}, searchNames(t, m, root, "person:carol"))

	// A new photo of a named person is recognized automatically.
	facePhoto(t, filepath.Join(root, "new.jpg"), 80, red)
	indexWithPreviews(t, m, root)
	_, err = m.mlBatch()
	require.NoError(t, err)
	assert.Equal(t, []string{"new.jpg", "p1.jpg", "p2.jpg"}, searchNames(t, m, root, "person:rose"))

	// Hidden people are not searchable by name.
	require.NoError(t, m.SetPersonHidden(blueID, true))
	assert.Empty(t, searchNames(t, m, root, "person:bob"))
	people, err = m.People(all, 1)
	require.NoError(t, err)
	for _, p := range people {
		if p.ID == blueID {
			assert.True(t, p.Hidden)
		}
	}

	// Users only see the people in photos they can see.
	onlyP2 := func(p string) bool { return filepath.Base(p) == "p2.jpg" }
	people, err = m.People(onlyP2, 1)
	require.NoError(t, err)
	require.Len(t, people, 1)
	assert.Equal(t, "Grandma Rose", people[0].Name)
	assert.Equal(t, 1, people[0].Faces)

	// A modified photo is processed again; a deleted one loses its faces.
	facePhoto(t, filepath.Join(root, "p2.jpg"), 70, red, green)
	indexWithPreviews(t, m, root)
	assert.Equal(t, int64(1), m.faceStatus().Pending)
	_, err = m.mlBatch()
	require.NoError(t, err)
	assert.Equal(t, []string{"p2.jpg", "p3.jpg"}, searchNames(t, m, root, "person:carol"),
		"the new green face matches Carol, whose face was placed by hand")
	require.NoError(t, os.Remove(filepath.Join(root, "p1.jpg")))
	require.NoError(t, m.removeTree(filepath.Join(root, "p1.jpg")))
	assert.Equal(t, []string{"new.jpg", "p2.jpg"}, searchNames(t, m, root, "person:rose"))
	require.NoError(t, m.loadClusters())

	// Merging explicitly.
	require.NoError(t, m.MergePeople(carolID, redID))
	assert.Equal(t, []string{"new.jpg", "p2.jpg", "p3.jpg"}, searchNames(t, m, root, "person:rose"))

	// While the service is down, nothing is marked as done.
	facePhoto(t, filepath.Join(root, "later.jpg"), 60, red)
	indexWithPreviews(t, m, root)
	ml.Down.Store(true)
	_, err = m.mlBatch()
	assert.ErrorIs(t, err, errMLUnavailable)
	assert.Equal(t, int64(1), m.faceStatus().Pending)
	ml.Down.Store(false)
	_, err = m.mlBatch()
	require.NoError(t, err)
	assert.Zero(t, m.faceStatus().Pending)

	// The setup check.
	data, err := os.ReadFile(filepath.Join(root, "p3.jpg"))
	require.NoError(t, err)
	res, err := CheckFaces(context.Background(), ml.URL, "buffalo_l", 0.7, data)
	require.NoError(t, err)
	assert.Len(t, res.Faces, 2)
	assert.Len(t, res.Faces[0].Embedding, 512)
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
