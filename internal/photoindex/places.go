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
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/drakkan/sftpgo/v2/internal/logger"
)

// Offline reverse geocoding: the GPS position of a photo is turned into the
// nearest town, its state/region and its country using the GeoNames
// gazetteer (https://www.geonames.org, CC BY 4.0), loaded in memory:
//
//   - cities500.txt (or cities500.zip, cities1000.txt/.zip): places with at
//     least 500 (1000) inhabitants
//   - admin1CodesASCII.txt: state/region names
//   - countryInfo.txt: country names
//
// The Docker image downloads them at build time.

const (
	// geoMaxDistanceKm is the maximum distance from a photo to the nearest
	// known town for the photo to be placed there.
	geoMaxDistanceKm = 50
	earthRadiusKm    = 6371
)

type geoPlace struct {
	lat, lon float32
	name     string
	admin1   string // "US.SC"
	country  string // "US"
}

// Place is where a photo was taken.
type Place struct {
	City    string `json:"city"`
	State   string `json:"state"`
	Country string `json:"country"`
}

type geocoder struct {
	places    []geoPlace
	cells     map[[2]int16][]int32
	admin1    map[string]string
	countries map[string]string
}

func cellOf(lat, lon float64) [2]int16 {
	return [2]int16{int16(math.Floor(lat)), int16(math.Floor(lon))}
}

// findGeonamesDir returns the directory holding the GeoNames files: the
// configured one, or the first existing default location.
func findGeonamesDir(configured, configDir string) string {
	candidates := []string{configured}
	if configured == "" {
		candidates = []string{filepath.Join(configDir, "geonames"), "/usr/share/sftpgo/geonames"}
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(configDir, dir)
		}
		if _, err := os.Stat(filepath.Join(dir, "admin1CodesASCII.txt")); err == nil {
			return dir
		}
	}
	return ""
}

// openCities opens the cities file of dir, plain or zipped.
func openCities(dir string) (io.ReadCloser, error) {
	for _, base := range []string{"cities500", "cities1000", "cities5000", "cities15000"} {
		if f, err := os.Open(filepath.Join(dir, base+".txt")); err == nil {
			return f, nil
		}
		zr, err := zip.OpenReader(filepath.Join(dir, base+".zip"))
		if err != nil {
			continue
		}
		for _, zf := range zr.File {
			if zf.Name == base+".txt" {
				rc, err := zf.Open()
				if err != nil {
					zr.Close()
					return nil, err
				}
				return &zipEntry{ReadCloser: rc, zr: zr}, nil
			}
		}
		zr.Close()
	}
	return nil, errors.New("no cities file found")
}

type zipEntry struct {
	io.ReadCloser
	zr *zip.ReadCloser
}

func (z *zipEntry) Close() error {
	z.ReadCloser.Close()
	return z.zr.Close()
}

// loadGeocoder loads the GeoNames files from dir.
func loadGeocoder(dir string) (*geocoder, error) {
	g := &geocoder{
		cells:     make(map[[2]int16][]int32),
		admin1:    make(map[string]string),
		countries: make(map[string]string),
	}
	err := readTSV(filepath.Join(dir, "admin1CodesASCII.txt"), func(f []string) {
		if len(f) >= 2 {
			g.admin1[f[0]] = f[1]
		}
	})
	if err != nil {
		return nil, err
	}
	err = readTSV(filepath.Join(dir, "countryInfo.txt"), func(f []string) {
		if len(f) >= 5 {
			g.countries[f[0]] = f[4]
		}
	})
	if err != nil {
		return nil, err
	}
	rc, err := openCities(dir)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	err = scanTSV(rc, func(f []string) {
		// geonameid, name, asciiname, alternatenames, latitude, longitude,
		// feature class, feature code, country code, cc2, admin1 code, ...
		if len(f) < 11 {
			return
		}
		lat, err1 := strconv.ParseFloat(f[4], 64)
		lon, err2 := strconv.ParseFloat(f[5], 64)
		if err1 != nil || err2 != nil {
			return
		}
		idx := int32(len(g.places))
		g.places = append(g.places, geoPlace{
			lat: float32(lat), lon: float32(lon), name: f[1],
			admin1: f[8] + "." + f[10], country: f[8],
		})
		c := cellOf(lat, lon)
		g.cells[c] = append(g.cells[c], idx)
	})
	if err != nil {
		return nil, err
	}
	if len(g.places) == 0 {
		return nil, errors.New("the cities file is empty")
	}
	return g, nil
}

func readTSV(p string, fn func([]string)) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return scanTSV(f, fn)
}

func scanTSV(r io.Reader, fn func([]string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		fn(strings.Split(line, "\t"))
	}
	return sc.Err()
}

func distanceKm(lat1, lon1, lat2, lon2 float64) float64 {
	const rad = math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusKm * math.Asin(math.Sqrt(math.Min(1, a)))
}

// lookup returns the place nearest to the given position, if a town is
// within geoMaxDistanceKm.
func (g *geocoder) lookup(lat, lon float64) (Place, bool) {
	c := cellOf(lat, lon)
	best := -1
	bestDist := math.MaxFloat64
	for dLat := int16(-1); dLat <= 1; dLat++ {
		for dLon := int16(-1); dLon <= 1; dLon++ {
			lonCell := c[1] + dLon
			// Wrap around the antimeridian.
			if lonCell < -180 {
				lonCell += 360
			} else if lonCell >= 180 {
				lonCell -= 360
			}
			for _, idx := range g.cells[[2]int16{c[0] + dLat, lonCell}] {
				p := &g.places[idx]
				if d := distanceKm(lat, lon, float64(p.lat), float64(p.lon)); d < bestDist {
					best, bestDist = int(idx), d
				}
			}
		}
	}
	if best < 0 || bestDist > geoMaxDistanceKm {
		return Place{}, false
	}
	p := &g.places[best]
	return Place{City: p.name, State: g.admin1[p.admin1], Country: g.countries[p.country]}, true
}

// geocode fills in the place of a record from its GPS position.
func (m *Manager) geocode(rec *Media) {
	if m.geo == nil || rec.Lat == nil || rec.Lon == nil {
		return
	}
	if p, ok := m.geo.lookup(*rec.Lat, *rec.Lon); ok {
		rec.City, rec.State, rec.Country = p.City, p.State, p.Country
	}
}

// backfillPlaces places the photos indexed before the gazetteer was
// available, e.g. after an upgrade.
func (m *Manager) backfillPlaces() {
	if m.geo == nil {
		return
	}
	rows, err := m.store.db.Query(`SELECT id, lat, lon FROM media WHERE geo_done = 0 AND lat IS NOT NULL
		AND lon IS NOT NULL`)
	if err != nil {
		logger.Warn(logSender, "", "unable to list the photos to place: %v", err)
		return
	}
	type todo struct {
		id       int64
		lat, lon float64
	}
	var items []todo
	for rows.Next() {
		var t todo
		if err := rows.Scan(&t.id, &t.lat, &t.lon); err == nil {
			items = append(items, t)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		logger.Warn(logSender, "", "unable to list the photos to place: %v", err)
		return
	}
	if len(items) == 0 {
		return
	}
	tx, err := m.store.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback() //nolint:errcheck
	for _, t := range items {
		p, _ := m.geo.lookup(t.lat, t.lon)
		if _, err := tx.Exec(`UPDATE media SET place_city = ?, place_state = ?, place_country = ?, geo_done = 1
			WHERE id = ?`, p.City, p.State, p.Country, t.id); err != nil {
			logger.Warn(logSender, "", "unable to place photo %d: %v", t.id, err)
			return
		}
	}
	if err := tx.Commit(); err == nil {
		logger.Info(logSender, "", "placed %d photos on the map", len(items))
	}
}

// PlaceCount is a place and the number of photos taken there.
type PlaceCount struct {
	Place
	Photos int     `json:"photos"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
}

// MapPoint is a photo with a GPS position.
type MapPoint struct {
	ID       int64
	Path     string
	Lat, Lon float64
	Place    Place
}

// MapPoints calls fn for every photo with a GPS position inside dirs.
func (m *Manager) MapPoints(dirs []string, fn func(MapPoint)) error {
	for _, d := range dirs {
		lo, hi := prefixRange(d)
		rows, err := m.store.db.Query(`SELECT id, path, lat, lon, place_city, place_state, place_country FROM media
			WHERE path >= ? AND path < ? AND lat IS NOT NULL AND lon IS NOT NULL`, lo, hi)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p MapPoint
			if err := rows.Scan(&p.ID, &p.Path, &p.Lat, &p.Lon, &p.Place.City, &p.Place.State, &p.Place.Country); err != nil {
				rows.Close()
				return err
			}
			fn(p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// PlacesEnabled reports whether photos can be placed by name.
func (m *Manager) PlacesEnabled() bool {
	return m != nil && m.geo != nil
}

func (p Place) String() string {
	parts := make([]string, 0, 3)
	for _, s := range []string{p.City, p.State, p.Country} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

func (g *geocoder) String() string {
	return fmt.Sprintf("%d places", len(g.places))
}
