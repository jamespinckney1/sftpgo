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
	"math"
	"net/http"
	"slices"

	"github.com/go-chi/render"

	"github.com/drakkan/sftpgo/v2/internal/photoindex"
	"github.com/drakkan/sftpgo/v2/internal/util"
)

// Places (fork feature, see FORK_FEATURES.md): a map of the photos with a GPS
// position and the list of the places where they were taken.

type clientPlacesPage struct {
	baseClientPage
	PointsURL    string
	TileURL      string
	ThumbnailURL string
	PlaceNames   bool
}

// placesURLForUser returns the Places page URL if the photo index is enabled.
func placesURLForUser() string {
	if photoindex.Get() != nil {
		return webClientPlacesPath
	}
	return ""
}

func (s *httpdServer) handleClientPlacesPage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestSize)
	m := photoindex.Get()
	if m == nil {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	if _, ok := getPhotoUserContext(w, r, m); !ok {
		return
	}
	data := clientPlacesPage{
		baseClientPage: s.getBaseClientPageData(util.I18nPlacesTitle, webClientPlacesPath, w, r),
		PointsURL:      webClientPlacesPath + "/points",
		TileURL:        m.MapTileURL(),
		ThumbnailURL:   thumbnailURLForUser(),
		PlaceNames:     m.PlacesEnabled(),
	}
	renderClientTemplate(w, templateClientPlaces, data)
}

type placeSummary struct {
	photoindex.Place
	Photos int     `json:"photos"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
}

func round5(v float64) float64 {
	return math.Round(v*1e5) / 1e5
}

// handleClientPlacesPoints returns the photos with a GPS position the user
// can see, compactly: "points" are [lat, lon, index in "files"], "files" are
// the virtual paths, and "places" summarizes the named places, the ones with
// the most photos first.
func (s *httpdServer) handleClientPlacesPoints(w http.ResponseWriter, r *http.Request) {
	m := photoindex.Get()
	if m == nil {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	ctx, ok := getPhotoUserContext(w, r, m)
	if !ok {
		return
	}
	scope := photoindex.NewUserScope(&ctx.user, "/")
	points := make([][3]float64, 0)
	files := make([]string, 0)
	byPlace := make(map[photoindex.Place]*placeSummary)
	seen := make(map[string]bool)
	err := m.MapPoints(scope.Dirs, func(p photoindex.MapPoint) {
		vPath, ok := ctx.virtualPath(p.Path)
		if !ok || seen[vPath] {
			return
		}
		seen[vPath] = true
		points = append(points, [3]float64{round5(p.Lat), round5(p.Lon), float64(len(files))})
		files = append(files, vPath)
		if p.Place.City == "" && p.Place.Country == "" {
			return
		}
		sum := byPlace[p.Place]
		if sum == nil {
			sum = &placeSummary{Place: p.Place}
			byPlace[p.Place] = sum
		}
		sum.Photos++
		sum.Lat += p.Lat
		sum.Lon += p.Lon
	})
	if err != nil {
		sendAPIResponse(w, r, err, "", http.StatusInternalServerError)
		return
	}
	places := make([]placeSummary, 0, len(byPlace))
	for _, sum := range byPlace {
		sum.Lat = round5(sum.Lat / float64(sum.Photos))
		sum.Lon = round5(sum.Lon / float64(sum.Photos))
		places = append(places, *sum)
	}
	slices.SortFunc(places, func(a, b placeSummary) int {
		if a.Photos != b.Photos {
			return b.Photos - a.Photos
		}
		if a.City < b.City {
			return -1
		}
		return 1
	})
	render.JSON(w, r, map[string]any{
		"points": points,
		"files":  files,
		"places": places,
	})
}
