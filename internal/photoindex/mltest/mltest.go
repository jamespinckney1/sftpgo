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

// Package mltest provides a stand-in for the Immich machine-learning service,
// for tests. It speaks the same /predict protocol but, instead of running
// neural networks, "detects" as faces the solid squares drawn in pure
// identity colors (see Identities) and returns, for each, an embedding close
// to that identity's fixed vector.
package mltest

import (
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"

	// Decoders for the received images.
	_ "image/jpeg"
	_ "image/png"

	"image/color"
)

// Identities are the colors recognized as faces, one per person.
var Identities = []color.RGBA{
	{220, 30, 30, 255},  // 0: red
	{30, 30, 220, 255},  // 1: blue
	{30, 200, 30, 255},  // 2: green
	{230, 200, 20, 255}, // 3: yellow
}

const dims = 512

func identityVector(i int) []float64 {
	r := rand.New(rand.NewSource(int64(1000 + i)))
	v := make([]float64, dims)
	for j := range v {
		v[j] = r.NormFloat64()
	}
	return v
}

// DrawFace draws a face of the given identity at (x, y) with the given size.
func DrawFace(img draw.Image, identity, x, y, size int) {
	draw.Draw(img, image.Rect(x, y, x+size, y+size), &image.Uniform{Identities[identity]}, image.Point{}, draw.Src)
}

// Server is a fake machine-learning service.
type Server struct {
	*httptest.Server
	// Requests counts the /predict requests.
	Requests atomic.Int64
	// Down makes the service answer 503.
	Down atomic.Bool
}

// NewServer starts a fake machine-learning service.
func NewServer() *Server {
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) {
		if s.Down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("pong")) //nolint:errcheck
	})
	mux.HandleFunc("POST /predict", s.predict)
	s.Server = httptest.NewServer(mux)
	return s
}

func (s *Server) predict(w http.ResponseWriter, r *http.Request) {
	if s.Down.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	s.Requests.Add(1)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	var entries map[string]map[string]map[string]any
	if err := json.Unmarshal([]byte(r.FormValue("entries")), &entries); err != nil {
		http.Error(w, "bad entries: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	fr := entries["facial-recognition"]
	if fr == nil || fr["detection"]["modelName"] == nil || fr["recognition"]["modelName"] == nil {
		http.Error(w, "unexpected entries", http.StatusUnprocessableEntity)
		return
	}
	f, _, err := r.FormFile("image")
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	b := img.Bounds()
	faces := findFaces(img)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"facial-recognition": faces,
		"imageHeight":        b.Dy(),
		"imageWidth":         b.Dx(),
	})
}

type box struct{ x1, y1, x2, y2, n int }

// findFaces returns a face for each identity color covering enough pixels.
func findFaces(img image.Image) []map[string]any {
	b := img.Bounds()
	boxes := make([]box, len(Identities))
	for i := range boxes {
		boxes[i] = box{x1: math.MaxInt, y1: math.MaxInt, x2: -1, y2: -1}
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			for i, c := range Identities {
				if near(r>>8, c.R) && near(g>>8, c.G) && near(bl>>8, c.B) {
					bx := &boxes[i]
					bx.x1, bx.y1 = min(bx.x1, x), min(bx.y1, y)
					bx.x2, bx.y2 = max(bx.x2, x+1), max(bx.y2, y+1)
					bx.n++
				}
			}
		}
	}
	faces := []map[string]any{}
	for i, bx := range boxes {
		if bx.n < 50 {
			continue
		}
		faces = append(faces, map[string]any{
			"boundingBox": map[string]any{"x1": bx.x1, "y1": bx.y1, "x2": bx.x2, "y2": bx.y2},
			// Immich sends the embedding as a string holding a JSON array.
			"embedding": embedding(i, bx),
			"score":     0.95,
		})
	}
	return faces
}

// embedding returns the identity's vector plus some noise, as a JSON array
// in a string.
func embedding(identity int, bx box) string {
	base := identityVector(identity)
	noise := rand.New(rand.NewSource(int64(bx.x1*7919 + bx.y1)))
	var sb strings.Builder
	sb.WriteString("[")
	for j := range base {
		if j > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "%.5f", base[j]+0.35*noise.NormFloat64())
	}
	sb.WriteString("]")
	return sb.String()
}

func near(v uint32, c uint8) bool {
	d := int(v) - int(c)
	return d > -45 && d < 45
}
