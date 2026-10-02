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
// neural networks:
//
//   - it "detects" as faces the solid squares drawn in pure identity colors
//     (see Identities) and returns, for each, an embedding close to that
//     identity's fixed vector;
//   - its "CLIP" sees colors: an image shows the color names (see Concepts)
//     of the colors covering at least 1% of it, and a text means the color
//     names it contains, so "show:red" finds the images with red in them.
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
	fr, clip := entries["facial-recognition"], entries["clip"]
	if (fr == nil && clip == nil) ||
		(fr != nil && (fr["detection"]["modelName"] == nil || fr["recognition"]["modelName"] == nil)) {
		http.Error(w, "unexpected entries", http.StatusUnprocessableEntity)
		return
	}
	if clip != nil && clip["textual"] != nil {
		s.encodeText(w, r, clip)
		return
	}
	s.analyzeImage(w, r, fr != nil, clip)
}

func (s *Server) analyzeImage(w http.ResponseWriter, r *http.Request, faces bool, clip map[string]map[string]any) {
	if clip != nil && (clip["visual"] == nil || clip["visual"]["modelName"] == nil) {
		http.Error(w, "missing model", http.StatusUnprocessableEntity)
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
	resp := map[string]any{"imageHeight": b.Dy(), "imageWidth": b.Dx()}
	if faces {
		resp["facial-recognition"] = findFaces(img)
	}
	if clip != nil {
		resp["clip"] = vectorString(imageConcepts(img))
	}
	writeJSON(w, resp)
}

func (s *Server) encodeText(w http.ResponseWriter, r *http.Request, clip map[string]map[string]any) {
	if clip["textual"]["modelName"] == nil {
		http.Error(w, "missing model", http.StatusUnprocessableEntity)
		return
	}
	writeJSON(w, map[string]any{"clip": vectorString(textConcepts(r.FormValue("text")))})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// Concepts are the words the fake CLIP understands, with their colors.
var Concepts = map[string]color.RGBA{
	"red":    {220, 30, 30, 255},
	"blue":   {30, 30, 220, 255},
	"green":  {30, 200, 30, 255},
	"yellow": {230, 200, 20, 255},
}

// conceptVector is the fixed random direction of a word.
func conceptVector(word string) []float64 {
	var seed int64
	for _, r := range word {
		seed = seed*31 + int64(r)
	}
	r := rand.New(rand.NewSource(seed))
	v := make([]float64, dims)
	for j := range v {
		v[j] = r.NormFloat64()
	}
	return v
}

func sumVectors(words []string) []float64 {
	v := make([]float64, dims)
	for _, w := range words {
		for j, x := range conceptVector(w) {
			v[j] += x
		}
	}
	if len(words) == 0 {
		v = conceptVector("nothing")
	}
	return v
}

func textConcepts(text string) []float64 {
	var words []string
	for _, w := range strings.Fields(strings.ToLower(text)) {
		if _, ok := Concepts[w]; ok {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		return conceptVector("text:" + text)
	}
	return sumVectors(words)
}

func imageConcepts(img image.Image) []float64 {
	b := img.Bounds()
	counts := make(map[string]int)
	for y := b.Min.Y; y < b.Max.Y; y += 2 {
		for x := b.Min.X; x < b.Max.X; x += 2 {
			r, g, bl, _ := img.At(x, y).RGBA()
			for name, c := range Concepts {
				if near(r>>8, c.R) && near(g>>8, c.G) && near(bl>>8, c.B) {
					counts[name]++
				}
			}
		}
	}
	total := (b.Dx() / 2) * (b.Dy() / 2)
	var words []string
	for name, n := range counts {
		if n*100 >= total {
			words = append(words, name)
		}
	}
	return sumVectors(words)
}

func vectorString(v []float64) string {
	var sb strings.Builder
	sb.WriteString("[")
	for j, x := range v {
		if j > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "%.5f", x)
	}
	sb.WriteString("]")
	return sb.String()
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
