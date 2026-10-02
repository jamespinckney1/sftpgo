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
	"archive/zip"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/drakkan/sftpgo/v2/internal/photoindex/mltest"
)

// writeGeonames writes a tiny gazetteer in the GeoNames format, the cities
// zipped like the cities500.zip download.
func writeGeonames(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "admin1CodesASCII.txt"), []byte(
		"US.SC\tSouth Carolina\tSouth Carolina\t4597040\nUS.NY\tNew York\tNew York\t5128638\n"+
			"IT.09\tLiguria\tLiguria\t3174725\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "countryInfo.txt"), []byte(
		"# GeoNames country info\n#ISO\tISO3\tISO-Numeric\tfips\tCountry\tCapital\n"+
			"US\tUSA\t840\tUS\tUnited States\tWashington\nIT\tITA\t380\tIT\tItaly\tRome\n"), 0644))
	cities := "" +
		"4588718\tMyrtle Beach\tMyrtle Beach\t\t33.68906\t-78.88669\tP\tPPL\tUS\t\tSC\t051\t\t\t35682\t\t9\tAmerica/New_York\t2011-05-14\n" +
		"4574324\tCharleston\tCharleston\t\t32.77657\t-79.93092\tP\tPPLA2\tUS\t\tSC\t019\t\t\t150227\t\t5\tAmerica/New_York\t2019-09-05\n" +
		"5128581\tNew York City\tNew York City\t\t40.71427\t-74.00597\tP\tPPL\tUS\t\tNY\t\t\t\t8804190\t10\t57\tAmerica/New_York\t2022-11-16\n" +
		"3176219\tGenoa\tGenova\t\t44.40478\t8.94439\tP\tPPLA\tIT\t\t09\tGE\t\t\t580223\t\t20\tEurope/Rome\t2023-01-13\n" +
		"broken line\n"
	f, err := os.Create(filepath.Join(dir, "cities500.zip"))
	require.NoError(t, err)
	zw := zip.NewWriter(f)
	w, err := zw.Create("cities500.txt")
	require.NoError(t, err)
	_, err = w.Write([]byte(cities))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, f.Close())
}

func TestGeocoder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "geonames")
	writeGeonames(t, dir)
	assert.Equal(t, dir, findGeonamesDir(dir, ""))
	assert.Equal(t, dir, findGeonamesDir("", filepath.Dir(dir)), "geonames in the config dir")
	assert.Empty(t, findGeonamesDir(filepath.Join(dir, "missing"), ""))

	g, err := loadGeocoder(dir)
	require.NoError(t, err)
	assert.Len(t, g.places, 4)
	p, ok := g.lookup(33.70, -78.87) // on the beach
	require.True(t, ok)
	assert.Equal(t, Place{City: "Myrtle Beach", State: "South Carolina", Country: "United States"}, p)
	assert.Equal(t, "Myrtle Beach, South Carolina, United States", p.String())
	p, ok = g.lookup(32.79, -79.95)
	require.True(t, ok)
	assert.Equal(t, "Charleston", p.City)
	p, ok = g.lookup(44.41, 8.93)
	require.True(t, ok)
	assert.Equal(t, Place{City: "Genoa", State: "Liguria", Country: "Italy"}, p)
	_, ok = g.lookup(30.0, -60.0) // the middle of the Atlantic
	assert.False(t, ok)
	_, ok = g.lookup(34.5, -78.88) // ~90 km from Myrtle Beach: too far
	assert.False(t, ok)

	_, err = loadGeocoder(t.TempDir())
	assert.Error(t, err)
}

func TestPlaces(t *testing.T) {
	geoDir := filepath.Join(t.TempDir(), "geonames")
	writeGeonames(t, geoDir)
	root := filepath.Join(t.TempDir(), "photos")
	m := newTestManager(t, root, Config{ExiftoolPath: "/nonexistent", VipsPath: "/nonexistent", GeonamesDir: geoDir})
	require.True(t, m.PlacesEnabled())

	add := func(name string, lat, lon float64, geoDone bool) {
		rec := &Media{Path: filepath.Join(root, name), Size: 1, ModTime: 1, Kind: KindImage,
			Taken: "2020-01-01 00:00:00", TakenSrc: TakenSrcExif, Lat: &lat, Lon: &lon, IndexedAt: 1, GeoDone: geoDone}
		if geoDone {
			m.geocode(rec)
		}
		_, err := m.store.upsert(rec)
		require.NoError(t, err)
	}
	add("beach.jpg", 33.70, -78.87, true)
	add("nyc.jpg", 40.75, -73.99, true)
	add("old.jpg", 44.41, 8.93, false) // indexed before the gazetteer was available
	add("sea.jpg", 30, -60, true)

	assert.Empty(t, searchNames(t, m, root, "place:genoa"))
	m.backfillPlaces()
	assert.Equal(t, []string{"old.jpg"}, searchNames(t, m, root, "place:genoa"))
	assert.Equal(t, []string{"beach.jpg"}, searchNames(t, m, root, "place:myrtle"))
	assert.Equal(t, []string{"beach.jpg"}, searchNames(t, m, root, `place:"south carolina"`))
	assert.Equal(t, []string{"beach.jpg", "nyc.jpg"}, searchNames(t, m, root, `place:"united states"`))
	assert.Equal(t, []string{"nyc.jpg"}, searchNames(t, m, root, `place:"united states" place:york`))
	assert.Empty(t, searchNames(t, m, root, "place:paris"))

	var points []MapPoint
	require.NoError(t, m.MapPoints([]string{root}, func(p MapPoint) { points = append(points, p) }))
	assert.Len(t, points, 4)
	for _, p := range points {
		if filepath.Base(p.Path) == "sea.jpg" {
			assert.Empty(t, p.Place.City)
		}
		if filepath.Base(p.Path) == "beach.jpg" {
			assert.Equal(t, "Myrtle Beach", p.Place.City)
		}
	}

	q, err := ParseQuery(`place:"New York" beach`)
	require.NoError(t, err)
	assert.True(t, q.Photo)
	assert.Equal(t, []string{"New York"}, q.filter.placeTerms)
	_, err = ParseQuery("place:")
	assert.Error(t, err)
}

// colorPhoto writes a photo with a gray background and a square of each color.
func colorPhoto(t *testing.T, p string, colors ...color.RGBA) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 400, 300))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{128, 128, 128, 255}}, image.Point{}, draw.Src)
	for i, c := range colors {
		draw.Draw(img, image.Rect(20+i*120, 80, 120+i*120, 180), &image.Uniform{c}, image.Point{}, draw.Src)
	}
	saveJPEG(t, p, img, 95)
}

func TestShow(t *testing.T) {
	ml := mltest.NewServer()
	defer ml.Close()
	root := filepath.Join(t.TempDir(), "photos")
	red, blue, yellow := mltest.Concepts["red"], mltest.Concepts["blue"], mltest.Concepts["yellow"]
	colorPhoto(t, filepath.Join(root, "red.jpg"), red)
	colorPhoto(t, filepath.Join(root, "2019", "red-blue.jpg"), red, blue)
	colorPhoto(t, filepath.Join(root, "blue.jpg"), blue)
	colorPhoto(t, filepath.Join(root, "gray.jpg"))
	// A face (red square, big enough) too, to check that faces and CLIP are
	// computed in a single request.
	facePhoto(t, filepath.Join(root, "face.jpg"), 60, 0)

	m := newTestManager(t, root, Config{ExiftoolPath: "/nonexistent", VipsPath: "/nonexistent",
		MLURL: ml.URL, FaceModel: "buffalo_l", ClipModel: "ViT-B-32__openai", FaceMinScore: 0.7,
		FaceMatchThreshold: 0.5})
	require.True(t, m.ClipEnabled())
	require.True(t, m.FacesEnabled())
	m.refreshRoots()
	m.enqueueTree(root)
	drain(t, m)
	require.NoError(t, m.loadClusters())
	assert.Equal(t, int64(5), m.faceStatus().Pending)
	n, err := m.mlBatch()
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, int64(5), ml.Requests.Load(), "one request per photo for faces and CLIP")
	assert.Zero(t, m.faceStatus().Pending)

	res := searchAll(t, m, []string{root}, "show:red")
	var names []string
	for _, r := range res {
		names = append(names, filepath.Base(r.Path))
		assert.GreaterOrEqual(t, r.Score, float32(showMinScore))
	}
	assert.ElementsMatch(t, []string{"red.jpg", "red-blue.jpg", "face.jpg"}, names)
	// The photos only showing red rank above the one also showing blue.
	assert.ElementsMatch(t, []string{"red.jpg", "face.jpg"}, names[:2])
	assert.Equal(t, "red-blue.jpg", names[2])

	// Several "show:" values form one description; the photo showing both
	// colors is the best match. Other filters still apply.
	res = searchAll(t, m, []string{root}, "show:blue show:red taken:any")
	require.NotEmpty(t, res)
	assert.Equal(t, "red-blue.jpg", filepath.Base(res[0].Path))
	assert.Equal(t, []string{"red-blue.jpg"}, searchNames(t, m, filepath.Join(root, "2019"), "show:red"))
	assert.Empty(t, searchNames(t, m, root, "show:yellow"), "nothing yellow")
	assert.Empty(t, searchNames(t, m, root, "show:red place:nowhere"))
	_ = yellow

	// Text embeddings are cached: the same search does not call the service.
	before := ml.Requests.Load()
	searchAll(t, m, []string{root}, "show:red")
	searchAll(t, m, []string{root}, "show:RED")
	assert.Equal(t, before, ml.Requests.Load())

	// A modified photo is analyzed again.
	colorPhoto(t, filepath.Join(root, "gray.jpg"), red)
	m.enqueueTree(root)
	drain(t, m)
	_, err = m.mlBatch()
	require.NoError(t, err)
	assert.Contains(t, searchNames(t, m, root, "show:red"), "gray.jpg")

	// Service down: new searches report it.
	ml.Down.Store(true)
	q, err := ParseQuery("show:green")
	require.NoError(t, err)
	err = m.Search([]string{root}, q, func(Media) bool { return true })
	assert.True(t, errors.Is(err, ErrShowUnavailable), err)
	ml.Down.Store(false)

	// A new CLIP model: every photo is analyzed again, once. A new face model
	// only warns, the faces are kept.
	require.Zero(t, m.faceStatus().Pending)
	m.cfg.ClipModel = "ViT-B-16__openai"
	m.cfg.FaceModel = "buffalo_s"
	require.NoError(t, m.checkModels())
	assert.Equal(t, int64(5), m.faceStatus().Pending)
	n, err = m.mlBatch()
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	require.NoError(t, m.checkModels())
	assert.Zero(t, m.faceStatus().Pending)
	model, err := m.store.setting("face_model")
	require.NoError(t, err)
	assert.Equal(t, "buffalo_l", model)

	// Disabled.
	m.clipOn = false
	err = m.Search([]string{root}, q, func(Media) bool { return true })
	assert.ErrorIs(t, err, ErrShowUnavailable)
}
