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
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"

	"github.com/drakkan/sftpgo/v2/internal/logger"
)

const (
	// minFacePixels is the minimum side, in preview pixels, of a face worth
	// recognizing: smaller faces (background crowds) give poor embeddings
	// and would only add noise to the groups.
	minFacePixels = 36
	// faceRetryDelay is how long to wait before retrying when the face
	// recognition service is unreachable; it doubles up to faceMaxRetryDelay.
	faceRetryDelay    = time.Minute
	faceMaxRetryDelay = 30 * time.Minute
)

// Face states of a photo.
const (
	facesTodo   = 0
	facesDone   = 1
	facesFailed = 2
)

// faceCluster is a person (named or not) as used for automatic matching: the
// running sum of its faces' embeddings and its normalized centroid.
type faceCluster struct {
	sum      []float32
	centroid []float32
	n        int
	named    bool
}

func (c *faceCluster) add(emb []float32) {
	if c.sum == nil {
		c.sum = make([]float32, len(emb))
	}
	if len(c.sum) != len(emb) {
		return
	}
	for i := range emb {
		c.sum[i] += emb[i]
	}
	c.n++
	c.centroid = slices.Clone(c.sum)
	normalize(c.centroid)
}

// faceEngine holds the in-memory clusters. All the changes to faces and
// people go through it, serialized by mu.
type faceEngine struct {
	mu        sync.Mutex
	clusters  map[int64]*faceCluster
	threshold float32
}

func encodeEmbedding(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func decodeEmbedding(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// loadClusters rebuilds the clusters from the database. Unnamed people left
// without faces are deleted.
func (m *Manager) loadClusters() error {
	fe := m.faces
	fe.mu.Lock()
	defer fe.mu.Unlock()
	if _, err := m.store.db.Exec(`DELETE FROM people WHERE name = '' AND id NOT IN
		(SELECT DISTINCT person_id FROM faces WHERE person_id IS NOT NULL)`); err != nil {
		return err
	}
	clusters := make(map[int64]*faceCluster)
	rows, err := m.store.db.Query(`SELECT id, name FROM people`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		clusters[id] = &faceCluster{named: name != ""}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = m.store.db.Query(`SELECT person_id, embedding FROM faces WHERE person_id IS NOT NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var pid int64
		var emb []byte
		if err := rows.Scan(&pid, &emb); err != nil {
			return err
		}
		if c := clusters[pid]; c != nil {
			c.add(decodeEmbedding(emb))
		}
	}
	fe.clusters = clusters
	return rows.Err()
}

// reloadCluster recomputes one cluster from its faces. fe.mu must be held.
func (m *Manager) reloadCluster(tx *sql.Tx, pid int64) error {
	var name string
	err := tx.QueryRow(`SELECT name FROM people WHERE id = ?`, pid).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		delete(m.faces.clusters, pid)
		return nil
	} else if err != nil {
		return err
	}
	c := &faceCluster{named: name != ""}
	rows, err := tx.Query(`SELECT embedding FROM faces WHERE person_id = ?`, pid)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var emb []byte
		if err := rows.Scan(&emb); err != nil {
			return err
		}
		c.add(decodeEmbedding(emb))
	}
	m.faces.clusters[pid] = c
	return rows.Err()
}

// bestCluster returns the person whose faces are the most similar to emb, if
// similar enough. Named people win over unnamed groups at equal similarity.
// fe.mu must be held.
func (fe *faceEngine) bestCluster(emb []float32) (int64, bool) {
	var best int64
	var bestSim float32 = -2
	bestNamed := false
	for id, c := range fe.clusters {
		if c.n == 0 {
			continue
		}
		sim := dot(c.centroid, emb)
		if sim > bestSim || (sim == bestSim && c.named && !bestNamed) {
			best, bestSim, bestNamed = id, sim, c.named
		}
	}
	return best, bestSim >= fe.threshold
}

// kickFaces wakes the face worker.
func (m *Manager) kickFaces() {
	if m.ml == nil {
		return
	}
	select {
	case m.faceCh <- struct{}{}:
	default:
	}
}

// faceLoop sends the previews of the photos not processed yet to the face
// recognition service while the indexer is idle, and retries with a growing
// delay while the service is unreachable.
func (m *Manager) faceLoop() {
	if err := m.loadClusters(); err != nil {
		logger.Error(logSender, "", "unable to load the face groups: %v", err)
	}
	retry := faceRetryDelay
	for {
		select {
		case <-m.faceCh:
		case <-m.stop:
			return
		}
		for {
			for m.busy() {
				if !m.sleep(dupIdleDelay) {
					return
				}
			}
			n, err := m.faceBatch()
			if errors.Is(err, errMLUnavailable) {
				m.mlDown.Store(true)
				logger.Warn(logSender, "", "face recognition paused, retrying in %s: %v", retry, err)
				if !m.sleep(retry) {
					return
				}
				retry = min(2*retry, faceMaxRetryDelay)
				continue
			}
			if err != nil {
				logger.Warn(logSender, "", "face recognition: %v", err)
				break
			}
			m.mlDown.Store(false)
			retry = faceRetryDelay
			if n == 0 || m.stopping() {
				break
			}
		}
	}
}

// faceCandidates returns photos still to process. Photos are read through
// their preview; when previews are disabled, the originals are used instead.
func (s *store) faceCandidates(limit int, noPreviews bool) ([]Media, error) {
	rows, err := s.db.Query(`SELECT `+mediaColumns+` FROM media WHERE faces_state = ? AND (has_preview = 1 OR ?)
		AND kind != ? ORDER BY id LIMIT ?`, facesTodo, noPreviews, KindVideo, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []Media
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		res = append(res, m)
	}
	return res, rows.Err()
}

func (m *Manager) faceBatch() (int, error) {
	recs, err := m.store.faceCandidates(dupBatchSize, m.previewDisabled)
	if err != nil {
		return 0, err
	}
	processed := 0
	for i := range recs {
		if m.stopping() || m.busy() {
			return processed, nil
		}
		if err := m.detectFaces(&recs[i]); err != nil {
			if errors.Is(err, errMLUnavailable) {
				return processed, err
			}
			logger.Debug(logSender, "", "face detection failed for %q: %v", recs[i].Path, err)
			if _, err := m.store.db.Exec(`UPDATE media SET faces_state = ? WHERE id = ? AND size = ? AND mtime = ?`,
				facesFailed, recs[i].ID, recs[i].Size, recs[i].ModTime); err != nil {
				return processed, err
			}
		}
		processed++
	}
	return processed, nil
}

// detectFaces finds the faces of an indexed photo, groups them and stores
// them.
func (m *Manager) detectFaces(rec *Media) error {
	data, err := m.faceSource(rec.ID, rec.Path, rec.Size)
	if err != nil {
		return err
	}
	res, err := m.ml.detectFaces(context.Background(), data)
	if err != nil {
		return err
	}
	w, h := res.Width, res.Height
	if w <= 0 || h <= 0 {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return err
		}
		w, h = cfg.Width, cfg.Height
	}
	var faces []MLFace
	for _, f := range res.Faces {
		if f.Score < m.cfg.FaceMinScore || f.X2-f.X1 < minFacePixels || f.Y2-f.Y1 < minFacePixels {
			continue
		}
		f.X1, f.X2 = clamp01(f.X1/float64(w)), clamp01(f.X2/float64(w))
		f.Y1, f.Y2 = clamp01(f.Y1/float64(h)), clamp01(f.Y2/float64(h))
		faces = append(faces, f)
	}
	return m.storeFaces(rec, faces)
}

// faceSource returns the JPEG to analyze for a photo: its preview, or the
// original if there is no preview and it is a format the service can read.
func (m *Manager) faceSource(mediaID int64, fsPath string, size int64) ([]byte, error) {
	if data, err := os.ReadFile(m.previewPath(mediaID)); err == nil {
		return data, nil
	}
	ext := strings.ToLower(filepath.Ext(fsPath))
	if ext != ".jpg" && ext != ".jpeg" && ext != ".jpe" && ext != ".png" {
		return nil, errors.New("no preview")
	}
	if m.cfg.MaxSourceSize > 0 && size > int64(m.cfg.MaxSourceSize)*1024*1024 {
		return nil, errors.New("file too large")
	}
	return os.ReadFile(fsPath)
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}

// storeFaces assigns each face to the most similar person, creating a new
// unnamed person when none is similar enough, and stores them, unless the
// photo changed meanwhile.
func (m *Manager) storeFaces(rec *Media, faces []MLFace) error {
	fe := m.faces
	fe.mu.Lock()
	defer fe.mu.Unlock()
	tx, err := m.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var state int
	err = tx.QueryRow(`SELECT faces_state FROM media WHERE id = ? AND size = ? AND mtime = ?`,
		rec.ID, rec.Size, rec.ModTime).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && state != facesTodo) {
		return nil // changed, deleted or already done
	} else if err != nil {
		return err
	}
	newClusters := make(map[int64]*faceCluster)
	pids := make([]int64, len(faces))
	for i := range faces {
		if pids[i], err = fe.insertFace(tx, rec.ID, &faces[i], newClusters); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE media SET faces_state = ? WHERE id = ?`, facesDone, rec.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for i, pid := range pids {
		c := fe.clusters[pid]
		if c == nil {
			c = &faceCluster{}
			fe.clusters[pid] = c
		}
		c.add(faces[i].Embedding)
	}
	return nil
}

// insertFace stores a face with the most similar person, among the known ones
// and the groups started by this same photo, or with a new unnamed person.
// fe.mu must be held.
func (fe *faceEngine) insertFace(tx *sql.Tx, mediaID int64, f *MLFace, newClusters map[int64]*faceCluster) (int64, error) {
	pid, ok := fe.bestCluster(f.Embedding)
	if !ok {
		pid, ok = bestOf(newClusters, f.Embedding, fe.threshold)
	}
	if !ok {
		now := time.Now().Unix()
		if err := tx.QueryRow(`INSERT INTO people (name, created_at, updated_at) VALUES ('', ?, ?) RETURNING id`,
			now, now).Scan(&pid); err != nil {
			return 0, err
		}
		newClusters[pid] = &faceCluster{}
	}
	if _, err := tx.Exec(`INSERT INTO faces (media_id, person_id, x1, y1, x2, y2, score, embedding)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, mediaID, pid, f.X1, f.Y1, f.X2, f.Y2, f.Score,
		encodeEmbedding(f.Embedding)); err != nil {
		return 0, err
	}
	if c := newClusters[pid]; c != nil {
		c.add(f.Embedding)
	}
	return pid, nil
}

// count runs a COUNT query, returning 0 on error (it only feeds status
// displays).
func (s *store) count(query string, args ...any) int64 {
	var n int64
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		logger.Debug(logSender, "", "count query failed: %v", err)
	}
	return n
}

func bestOf(clusters map[int64]*faceCluster, emb []float32, threshold float32) (int64, bool) {
	var best int64
	var bestSim float32 = -2
	for id, c := range clusters {
		if c.n == 0 {
			continue
		}
		if sim := dot(c.centroid, emb); sim > bestSim {
			best, bestSim = id, sim
		}
	}
	return best, bestSim >= threshold
}

// Person is a person as seen by a user: only the faces in photos the user can
// see are counted.
type Person struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Hidden bool   `json:"hidden"`
	Faces  int    `json:"faces"`
	Photos int    `json:"photos"`
	// Cover is the face shown for the person.
	Cover int64 `json:"cover"`
}

// Face is a face in a photo, with its box relative to the image.
type Face struct {
	ID       int64   `json:"id"`
	PersonID int64   `json:"person_id"`
	MediaID  int64   `json:"-"`
	Path     string  `json:"-"`
	X1       float64 `json:"-"`
	Y1       float64 `json:"-"`
	X2       float64 `json:"-"`
	Y2       float64 `json:"-"`
	Score    float64 `json:"-"`
	Locked   bool    `json:"locked"`
}

// Visible reports whether the user can see the photo stored at fsPath.
type Visible func(fsPath string) bool

func cachedVisible(visible Visible) func(mediaID int64, fsPath string) bool {
	cache := make(map[int64]bool)
	return func(mediaID int64, fsPath string) bool {
		v, ok := cache[mediaID]
		if !ok {
			v = visible(fsPath)
			cache[mediaID] = v
		}
		return v
	}
}

const faceColumns = `f.id, COALESCE(f.person_id, 0), f.media_id, m.path, f.x1, f.y1, f.x2, f.y2, f.score, f.locked`

func scanFace(row scanner) (Face, error) {
	var f Face
	err := row.Scan(&f.ID, &f.PersonID, &f.MediaID, &f.Path, &f.X1, &f.Y1, &f.X2, &f.Y2, &f.Score, &f.Locked)
	return f, err
}

// coverQuality ranks the faces to show for a person: large and confident.
func (f *Face) coverQuality() float64 {
	return f.Score * (f.X2 - f.X1) * (f.Y2 - f.Y1)
}

// People returns the people with at least one face in a photo the user can
// see. Unnamed groups need at least minFaces visible faces.
func (m *Manager) People(visible Visible, minFaces int) ([]Person, error) {
	rows, err := m.store.db.Query(`SELECT ` + faceColumns + `, p.name, p.hidden FROM faces f
		JOIN media m ON m.id = f.media_id JOIN people p ON p.id = f.person_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	isVisible := cachedVisible(visible)
	type acc struct {
		Person
		photos    map[int64]bool
		coverQual float64
	}
	byID := make(map[int64]*acc)
	for rows.Next() {
		var f Face
		var name string
		var hidden bool
		if err := rows.Scan(&f.ID, &f.PersonID, &f.MediaID, &f.Path, &f.X1, &f.Y1, &f.X2, &f.Y2, &f.Score,
			&f.Locked, &name, &hidden); err != nil {
			return nil, err
		}
		if !isVisible(f.MediaID, f.Path) {
			continue
		}
		a := byID[f.PersonID]
		if a == nil {
			a = &acc{Person: Person{ID: f.PersonID, Name: name, Hidden: hidden}, photos: make(map[int64]bool)}
			byID[f.PersonID] = a
		}
		a.Faces++
		a.photos[f.MediaID] = true
		if q := f.coverQuality(); q > a.coverQual {
			a.coverQual, a.Cover = q, f.ID
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res := make([]Person, 0, len(byID))
	for _, a := range byID {
		if a.Name == "" && a.Faces < minFaces {
			continue
		}
		a.Photos = len(a.photos)
		res = append(res, a.Person)
	}
	// Named people first, by name; then unnamed groups, the largest first.
	slices.SortFunc(res, func(a, b Person) int {
		switch {
		case (a.Name != "") != (b.Name != ""):
			if a.Name != "" {
				return -1
			}
			return 1
		case a.Name != "":
			return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		case a.Faces != b.Faces:
			return b.Faces - a.Faces
		}
		return int(a.ID - b.ID)
	})
	return res, nil
}

// PersonFaces returns the faces of a person in photos the user can see, the
// best ones first.
func (m *Manager) PersonFaces(personID int64, visible Visible) ([]Face, error) {
	rows, err := m.store.db.Query(`SELECT `+faceColumns+` FROM faces f JOIN media m ON m.id = f.media_id
		WHERE f.person_id = ?`, personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	isVisible := cachedVisible(visible)
	var res []Face
	for rows.Next() {
		f, err := scanFace(rows)
		if err != nil {
			return nil, err
		}
		if isVisible(f.MediaID, f.Path) {
			res = append(res, f)
		}
	}
	slices.SortStableFunc(res, func(a, b Face) int {
		qa, qb := a.coverQuality(), b.coverQuality()
		switch {
		case qa > qb:
			return -1
		case qa < qb:
			return 1
		}
		return int(a.ID - b.ID)
	})
	return res, rows.Err()
}

// GetFace returns a face.
func (m *Manager) GetFace(id int64) (Face, bool, error) {
	f, err := scanFace(m.store.db.QueryRow(`SELECT `+faceColumns+` FROM faces f JOIN media m ON m.id = f.media_id
		WHERE f.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return f, false, nil
	}
	return f, err == nil, err
}

// GetPerson returns a person's name and hidden flag.
func (m *Manager) GetPerson(id int64) (Person, bool, error) {
	p := Person{ID: id}
	err := m.store.db.QueryRow(`SELECT name, hidden FROM people WHERE id = ?`, id).Scan(&p.Name, &p.Hidden)
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

// PersonNames returns the names of the named people, sorted.
func (m *Manager) PersonNames() ([]string, error) {
	rows, err := m.store.db.Query(`SELECT name FROM people WHERE name != '' AND hidden = 0 ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var res []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		res = append(res, n)
	}
	return res, rows.Err()
}

// ErrNameRequired is returned when a person name is empty.
var ErrNameRequired = errors.New("a name is required")

func cleanName(name string) string {
	return strings.Join(strings.Fields(name), " ")
}

// findPersonByName returns the person with the given name, ignoring case.
func findPersonByName(tx *sql.Tx, name string) (int64, bool, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM people WHERE name = ? COLLATE NOCASE ORDER BY id LIMIT 1`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// withFaceTx runs fn in a transaction with the face engine locked, then
// recomputes the clusters of the people it reports as changed.
func (m *Manager) withFaceTx(fn func(tx *sql.Tx) ([]int64, error)) error {
	fe := m.faces
	fe.mu.Lock()
	defer fe.mu.Unlock()
	tx, err := m.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	changed, err := fn(tx)
	if err != nil {
		return err
	}
	for _, pid := range changed {
		if err := m.reloadCluster(tx, pid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RenamePerson names a person. If another person already has that name the
// two are merged, since they are the same person. It returns the id of the
// person that now holds the faces.
func (m *Manager) RenamePerson(id int64, name string) (int64, error) {
	name = cleanName(name)
	if name == "" {
		return 0, ErrNameRequired
	}
	result := id
	err := m.withFaceTx(func(tx *sql.Tx) ([]int64, error) {
		other, found, err := findPersonByName(tx, name)
		if err != nil {
			return nil, err
		}
		if found && other != id {
			result = other
			return mergeTx(tx, id, other)
		}
		_, err = tx.Exec(`UPDATE people SET name = ?, updated_at = ? WHERE id = ?`, name, time.Now().Unix(), id)
		return []int64{id}, err
	})
	return result, err
}

// MergePeople moves every face of src to dst and deletes src.
func (m *Manager) MergePeople(src, dst int64) error {
	if src == dst {
		return nil
	}
	return m.withFaceTx(func(tx *sql.Tx) ([]int64, error) {
		return mergeTx(tx, src, dst)
	})
}

func mergeTx(tx *sql.Tx, src, dst int64) ([]int64, error) {
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM people WHERE id = ?`, dst).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, fmt.Errorf("person %d not found", dst)
	}
	if _, err := tx.Exec(`UPDATE faces SET person_id = ? WHERE person_id = ?`, dst, src); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM people WHERE id = ?`, src); err != nil {
		return nil, err
	}
	_, err := tx.Exec(`UPDATE people SET updated_at = ? WHERE id = ?`, time.Now().Unix(), dst)
	return []int64{src, dst}, err
}

// SetPersonHidden hides or shows a person. Faces of hidden people (strangers
// in the background) keep being grouped with them, so they stay out of the
// way.
func (m *Manager) SetPersonHidden(id int64, hidden bool) error {
	_, err := m.store.db.Exec(`UPDATE people SET hidden = ?, updated_at = ? WHERE id = ?`, hidden,
		time.Now().Unix(), id)
	return err
}

// MoveFaces moves faces to the person with the given name, creating it if
// needed, or, with an empty name, removes them from their person. Moved faces
// are locked: automatic matching never moves them again.
func (m *Manager) MoveFaces(faceIDs []int64, name string) (int64, error) {
	name = cleanName(name)
	var target int64
	err := m.withFaceTx(func(tx *sql.Tx) ([]int64, error) {
		changed := map[int64]bool{}
		if name != "" {
			id, found, err := findPersonByName(tx, name)
			if err != nil {
				return nil, err
			}
			if !found {
				now := time.Now().Unix()
				if err := tx.QueryRow(`INSERT INTO people (name, created_at, updated_at) VALUES (?, ?, ?) RETURNING id`,
					name, now, now).Scan(&id); err != nil {
					return nil, err
				}
			}
			target = id
			changed[id] = true
		}
		for _, fid := range faceIDs {
			var old sql.NullInt64
			if err := tx.QueryRow(`SELECT person_id FROM faces WHERE id = ?`, fid).Scan(&old); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return nil, err
			}
			if old.Valid {
				changed[old.Int64] = true
			}
			var pid any
			if target != 0 {
				pid = target
			}
			if _, err := tx.Exec(`UPDATE faces SET person_id = ?, locked = 1 WHERE id = ?`, pid, fid); err != nil {
				return nil, err
			}
		}
		// Drop the unnamed groups left empty.
		if _, err := tx.Exec(`DELETE FROM people WHERE name = '' AND id NOT IN
			(SELECT DISTINCT person_id FROM faces WHERE person_id IS NOT NULL)`); err != nil {
			return nil, err
		}
		ids := make([]int64, 0, len(changed))
		for id := range changed {
			ids = append(ids, id)
		}
		return ids, nil
	})
	return target, err
}

// resolvePeople returns the ids of the people matching a "person:" search
// term: "#12" is a person id, anything else matches the visible people whose
// name contains it, ignoring case.
func (m *Manager) resolvePeople(term string) ([]int64, error) {
	if id, err := strconv.ParseInt(strings.TrimPrefix(term, "#"), 10, 64); err == nil && strings.HasPrefix(term, "#") {
		return []int64{id}, nil
	}
	rows, err := m.store.db.Query(`SELECT id FROM people WHERE hidden = 0 AND name != '' AND instr(lower(name), lower(?)) > 0`,
		term)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FaceStatus describes the face recognition progress.
type FaceStatus struct {
	Enabled    bool  `json:"enabled"`
	Pending    int64 `json:"pending"`
	Faces      int64 `json:"faces"`
	Named      int64 `json:"named"`
	ServiceOff bool  `json:"service_off"`
}

func (m *Manager) faceStatus() FaceStatus {
	st := FaceStatus{Enabled: m.ml != nil, ServiceOff: m.mlDown.Load()}
	if m.ml == nil {
		return st
	}
	st.Pending = m.store.count(`SELECT COUNT(*) FROM media WHERE faces_state = ? AND (has_preview = 1 OR ?)
		AND kind != ?`, facesTodo, m.previewDisabled, KindVideo)
	st.Faces = m.store.count(`SELECT COUNT(*) FROM faces`)
	st.Named = m.store.count(`SELECT COUNT(*) FROM people WHERE name != ''`)
	return st
}

// FacesEnabled reports whether face recognition is configured.
func (m *Manager) FacesEnabled() bool {
	return m != nil && m.ml != nil
}

// CheckFaces sends one image to the face recognition service and returns the
// result, to verify the setup.
func CheckFaces(ctx context.Context, url, model string, minScore float64, jpeg []byte) (MLResult, error) {
	c := newMLClient(url, model, minScore)
	if err := c.ping(ctx); err != nil {
		return MLResult{}, err
	}
	return c.detectFaces(ctx, jpeg)
}

// FaceCrop returns a square JPEG thumbnail, size pixels wide, of a face cut
// from its photo's preview, with some margin around it. Thumbnails are
// cached; face ids are never reused, so a cached thumbnail never goes stale.
func (m *Manager) FaceCrop(f Face, size int) ([]byte, error) {
	cache := filepath.Join(m.dataDir, "facecrops", fmt.Sprintf("%02x", f.ID%256), strconv.FormatInt(f.ID, 10)+".jpg")
	if data, err := os.ReadFile(cache); err == nil {
		return data, nil
	}
	data, err := m.faceSource(f.MediaID, f.Path, 0)
	if err != nil {
		return nil, err
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())
	cx, cy := (f.X1+f.X2)/2*w, (f.Y1+f.Y2)/2*h
	side := math.Max((f.X2-f.X1)*w, (f.Y2-f.Y1)*h) * 1.5
	side = math.Min(side, math.Min(w, h))
	x0 := math.Max(0, math.Min(cx-side/2, w-side))
	y0 := math.Max(0, math.Min(cy-side/2, h-side))
	rect := image.Rect(b.Min.X+int(x0), b.Min.Y+int(y0), b.Min.X+int(x0+side), b.Min.Y+int(y0+side))
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, rect, xdraw.Src, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cache), 0700); err == nil {
		tmp := cache + ".tmp"
		if os.WriteFile(tmp, buf.Bytes(), 0600) == nil {
			os.Rename(tmp, cache) //nolint:errcheck
		}
	}
	return buf.Bytes(), nil
}
