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
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/drakkan/sftpgo/v2/internal/logger"
)

// "Things pictured": CLIP maps images and texts to the same space, so a text
// such as "dog on the beach" can be compared with every photo. The image
// embeddings are computed once per photo by the machine-learning service and
// stored in the index; for search they are kept in memory quantized to int8
// (512 bytes per photo).

const (
	// showMaxResults is the number of best matches returned by a "show:"
	// search: CLIP ranks every photo, so the tail is irrelevant.
	showMaxResults = 300
	// showMinScore drops the matches that are clearly unrelated. Measured
	// with ViT-B-32__openai on a real photo: 0.20-0.26 for matching
	// descriptions, 0.12-0.185 for unrelated ones.
	showMinScore = 0.19
	// textCacheSize is the number of text embeddings kept in memory.
	textCacheSize = 256
)

type clipIndex struct {
	mu     sync.RWMutex
	loaded bool
	vecs   map[int64][]int8

	textMu    sync.Mutex
	textCache map[string][]float32
	textOrder []string
}

func quantize(v []float32) []int8 {
	q := make([]int8, len(v))
	for i, x := range v {
		q[i] = int8(math.Max(-127, math.Min(127, math.Round(float64(x)*127))))
	}
	return q
}

func dotQuantized(text []float32, img []int8) float32 {
	if len(text) != len(img) {
		return -1
	}
	var s float32
	for i := range text {
		s += text[i] * float32(img[i])
	}
	return s / 127
}

// storeClip stores the CLIP embedding of a photo, unless it changed meanwhile.
func (m *Manager) storeClip(rec *Media, emb []float32) error {
	res, err := m.store.db.Exec(`UPDATE media SET clip = ?, clip_state = ? WHERE id = ? AND size = ? AND mtime = ?`,
		encodeEmbedding(emb), facesDone, rec.ID, rec.Size, rec.ModTime)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	m.clip.mu.Lock()
	if m.clip.loaded {
		m.clip.vecs[rec.ID] = quantize(emb)
	}
	m.clip.mu.Unlock()
	return nil
}

// loadClip loads the image embeddings in memory, once.
func (m *Manager) loadClip() error {
	m.clip.mu.RLock()
	loaded := m.clip.loaded
	m.clip.mu.RUnlock()
	if loaded {
		return nil
	}
	m.clip.mu.Lock()
	defer m.clip.mu.Unlock()
	if m.clip.loaded {
		return nil
	}
	rows, err := m.store.db.Query(`SELECT id, clip FROM media WHERE clip_state = ? AND clip IS NOT NULL`, facesDone)
	if err != nil {
		return err
	}
	defer rows.Close()
	vecs := make(map[int64][]int8)
	for rows.Next() {
		var id int64
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return err
		}
		vecs[id] = quantize(decodeEmbedding(b))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	m.clip.vecs = vecs
	m.clip.loaded = true
	return nil
}

// checkModels records the models the stored analysis was made with. The
// embeddings of different CLIP models cannot be compared, so the photos are
// analyzed again after a change. Face descriptors cannot be compared either,
// but analyzing the faces again would lose the names given to people, so a
// face model change is left to the administrator.
func (m *Manager) checkModels() error {
	if m.clipOn {
		prev, err := m.store.setting("clip_model")
		if err != nil {
			return err
		}
		if prev != m.cfg.ClipModel {
			if prev != "" {
				logger.Info(logSender, "", "CLIP model changed from %q to %q, every photo will be analyzed again",
					prev, m.cfg.ClipModel)
				if _, err := m.store.db.Exec(`UPDATE media SET clip = NULL, clip_state = 0 WHERE clip_state <> 0`); err != nil {
					return err
				}
			}
			if err := m.store.setSetting("clip_model", m.cfg.ClipModel); err != nil {
				return err
			}
		}
	}
	if m.facesOn {
		prev, err := m.store.setting("face_model")
		if err != nil {
			return err
		}
		switch {
		case prev == "":
			return m.store.setSetting("face_model", m.cfg.FaceModel)
		case prev != m.cfg.FaceModel:
			logger.Warn(logSender, "", "the faces were found with the model %q, not with the configured %q: "+
				"new faces will not match the existing people well. Set face_model back to %q, or delete the "+
				"index to start over", prev, m.cfg.FaceModel, prev)
		}
	}
	return nil
}

// textEmbedding returns the CLIP embedding of a search text, cached.
func (m *Manager) textEmbedding(text string) ([]float32, error) {
	key := strings.ToLower(strings.Join(strings.Fields(text), " "))
	c := &m.clip
	c.textMu.Lock()
	if v, ok := c.textCache[key]; ok {
		c.textMu.Unlock()
		return v, nil
	}
	c.textMu.Unlock()
	// The text model may need to be loaded: allow some time.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	v, err := m.ml.encodeText(ctx, key)
	if err != nil {
		return nil, err
	}
	c.textMu.Lock()
	defer c.textMu.Unlock()
	if c.textCache == nil {
		c.textCache = make(map[string][]float32)
	}
	if len(c.textOrder) >= textCacheSize {
		delete(c.textCache, c.textOrder[0])
		c.textOrder = c.textOrder[1:]
	}
	c.textCache[key] = v
	c.textOrder = append(c.textOrder, key)
	return v, nil
}

// ErrShowUnavailable is returned by a "show:" search when the feature is not
// configured or the machine-learning service cannot be reached.
var ErrShowUnavailable = errors.New(`"show:" search is not available`)

// searchShow ranks the photos matching the other filters by similarity to
// the "show:" description and calls fn with the best ones, best first.
func (m *Manager) searchShow(dirs []string, q Query, f dateFilter, fn func(Media) bool) error {
	if !m.clipOn {
		return ErrShowUnavailable
	}
	text, err := m.textEmbedding(q.Show)
	if err != nil {
		if errors.Is(err, errMLUnavailable) {
			return errors.Join(ErrShowUnavailable, err)
		}
		return err
	}
	if err := m.loadClip(); err != nil {
		return err
	}
	f.sortBySize = false
	var hits []Media
	m.clip.mu.RLock()
	err = m.store.query(dirs, f, func(rec Media) bool {
		if v, ok := m.clip.vecs[rec.ID]; ok {
			if rec.Score = dotQuantized(text, v); rec.Score >= showMinScore {
				hits = append(hits, rec)
			}
		}
		return true
	})
	m.clip.mu.RUnlock()
	if err != nil {
		return err
	}
	slices.SortStableFunc(hits, func(a, b Media) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return 0
	})
	if len(hits) > showMaxResults {
		hits = hits[:showMaxResults]
	}
	for _, h := range hits {
		if !fn(h) {
			break
		}
	}
	return nil
}
