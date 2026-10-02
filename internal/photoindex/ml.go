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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// The face detection and recognition models run in a separate service: the
// machine-learning container of the Immich project
// (ghcr.io/immich-app/immich-machine-learning). It downloads the models on
// first use and answers POST /predict requests: a multipart form with an
// "entries" JSON field describing the tasks and an "image" file.

const (
	mlTaskFaces       = "facial-recognition"
	mlTypeDetection   = "detection"
	mlTypeRecognition = "recognition"
	mlTaskClip        = "clip"
	mlTypeVisual      = "visual"
	mlTypeTextual     = "textual"
	// mlTimeout is generous: the first request downloads the models.
	mlTimeout = 10 * time.Minute
)

// errMLUnavailable wraps failures to reach the service, as opposed to errors
// about a specific image: the work is retried later instead of being marked
// as failed.
var errMLUnavailable = errors.New("machine-learning service unavailable")

// MLFace is a face found in an image.
type MLFace struct {
	// Box in pixels of the image sent.
	X1, Y1, X2, Y2 float64
	Score          float64
	// Embedding is the face descriptor, normalized to unit length.
	Embedding []float32
}

// MLResult is the result of an image analysis.
type MLResult struct {
	Width, Height int
	Faces         []MLFace
	// Clip is the CLIP image embedding, normalized, if requested.
	Clip []float32
}

type mlClient struct {
	url       string
	faceModel string // empty disables face recognition
	clipModel string // empty disables "things pictured"
	minScore  float64
	client    *http.Client
}

func newMLClient(url, faceModel, clipModel string, minScore float64) *mlClient {
	return &mlClient{
		url:       strings.TrimSuffix(url, "/"),
		faceModel: faceModel,
		clipModel: clipModel,
		minScore:  minScore,
		client:    &http.Client{Timeout: mlTimeout},
	}
}

// ping checks that the service is reachable.
func (c *mlClient) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/ping", nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errMLUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: ping returned %s", errMLUnavailable, resp.Status)
	}
	return nil
}

// detectFaces sends a JPEG to the service and returns the faces found.
func (c *mlClient) detectFaces(ctx context.Context, jpeg []byte) (MLResult, error) {
	return c.analyze(ctx, jpeg, true, false)
}

// analyze sends a JPEG to the service, asking in a single request for the
// faces and/or the CLIP embedding.
func (c *mlClient) analyze(ctx context.Context, jpeg []byte, faces, clip bool) (MLResult, error) {
	entries := map[string]any{}
	if faces {
		entries[mlTaskFaces] = map[string]any{
			mlTypeDetection: map[string]any{
				"modelName": c.faceModel,
				"options":   map[string]any{"minScore": c.minScore},
			},
			mlTypeRecognition: map[string]any{"modelName": c.faceModel},
		}
	}
	if clip {
		entries[mlTaskClip] = map[string]any{
			mlTypeVisual: map[string]any{"modelName": c.clipModel},
		}
	}
	data, err := c.predict(ctx, entries, jpeg, "")
	if err != nil {
		return MLResult{}, err
	}
	return parseMLResponse(data, clip)
}

// encodeText returns the CLIP embedding of a text, to compare with the image
// embeddings.
func (c *mlClient) encodeText(ctx context.Context, text string) ([]float32, error) {
	entries := map[string]any{
		mlTaskClip: map[string]any{
			mlTypeTextual: map[string]any{"modelName": c.clipModel},
		},
	}
	data, err := c.predict(ctx, entries, nil, text)
	if err != nil {
		return nil, err
	}
	var r map[string]json.RawMessage
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("unable to parse the text embedding response: %w", err)
	}
	return parseEmbedding(r[mlTaskClip])
}

// predict calls POST /predict with the given tasks and an image or a text.
func (c *mlClient) predict(ctx context.Context, entries map[string]any, image []byte, text string) ([]byte, error) {
	entriesJSON, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("entries", string(entriesJSON)); err != nil {
		return nil, err
	}
	if image != nil {
		part, err := w.CreateFormFile("image", "image.jpg")
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(image); err != nil {
			return nil, err
		}
	} else if err := w.WriteField("text", text); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/predict", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errMLUnavailable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errMLUnavailable, err)
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s: %s", errMLUnavailable, resp.Status, truncate(string(data), 300))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("analysis failed: %s: %s", resp.Status, truncate(string(data), 300))
	}
	return data, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// mlResponse is the /predict response. Embeddings are sent as a JSON string
// containing an array (to be stored as is in pgvector by Immich) or, in some
// versions, as an array: both are accepted.
type mlResponse struct {
	Clip        json.RawMessage `json:"clip"`
	ImageWidth  int             `json:"imageWidth"`
	ImageHeight int             `json:"imageHeight"`
	Faces       []struct {
		BoundingBox struct {
			X1 float64 `json:"x1"`
			Y1 float64 `json:"y1"`
			X2 float64 `json:"x2"`
			Y2 float64 `json:"y2"`
		} `json:"boundingBox"`
		Embedding json.RawMessage `json:"embedding"`
		Score     float64         `json:"score"`
	} `json:"facial-recognition"`
}

func parseMLResponse(data []byte, wantClip bool) (MLResult, error) {
	var r mlResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return MLResult{}, fmt.Errorf("unable to parse the analysis response: %w", err)
	}
	res := MLResult{Width: r.ImageWidth, Height: r.ImageHeight}
	if wantClip {
		clip, err := parseEmbedding(r.Clip)
		if err != nil {
			return MLResult{}, fmt.Errorf("clip: %w", err)
		}
		res.Clip = clip
	}
	for i, f := range r.Faces {
		emb, err := parseEmbedding(f.Embedding)
		if err != nil {
			return MLResult{}, fmt.Errorf("face %d: %w", i, err)
		}
		res.Faces = append(res.Faces, MLFace{
			X1: f.BoundingBox.X1, Y1: f.BoundingBox.Y1, X2: f.BoundingBox.X2, Y2: f.BoundingBox.Y2,
			Score: f.Score, Embedding: emb,
		})
	}
	return res, nil
}

func parseEmbedding(raw json.RawMessage) ([]float32, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		raw = []byte(s)
	}
	var v []float32
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("invalid embedding: %w", err)
	}
	if len(v) == 0 {
		return nil, errors.New("empty embedding")
	}
	normalize(v)
	return v, nil
}

// normalize scales v to unit length, so the dot product of two embeddings is
// their cosine similarity.
func normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	n := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= n
	}
}

func dot(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}
