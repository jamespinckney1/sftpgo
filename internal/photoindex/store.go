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
	"errors"
	"fmt"
	"strings"
	"time"
)

// schemaVersion is the current version of the index schema. Later phases (CLIP
// embeddings, faces) add tables through new migrations.
const schemaVersion = 2

var migrations = []string{
	// version 1
	`CREATE TABLE media (
		id INTEGER PRIMARY KEY,
		path TEXT NOT NULL UNIQUE,
		size INTEGER NOT NULL,
		mtime INTEGER NOT NULL,
		kind TEXT NOT NULL,
		taken TEXT NOT NULL,
		taken_src TEXT NOT NULL,
		tz_offset TEXT NOT NULL DEFAULT '',
		width INTEGER NOT NULL DEFAULT 0,
		height INTEGER NOT NULL DEFAULT 0,
		lat REAL,
		lon REAL,
		camera TEXT NOT NULL DEFAULT '',
		has_preview INTEGER NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '',
		indexed_at INTEGER NOT NULL
	);
	CREATE INDEX media_taken_idx ON media(taken);`,
	// version 2: duplicate detection. hash is the SHA-256 of the content,
	// computed only for files sharing their size with another file; phash is
	// a 64-bit perceptual hash (dHash) of the image, NULL if not computed.
	`ALTER TABLE media ADD COLUMN hash TEXT NOT NULL DEFAULT '';
	ALTER TABLE media ADD COLUMN phash INTEGER;
	ALTER TABLE media ADD COLUMN phash_tried INTEGER NOT NULL DEFAULT 0;
	CREATE INDEX media_size_idx ON media(size);
	CREATE INDEX media_hash_idx ON media(hash) WHERE hash != '';`,
}

// Media is an indexed photo or video.
type Media struct {
	ID         int64
	Path       string // absolute filesystem path
	Size       int64
	ModTime    int64 // unix nanoseconds
	Kind       string
	Taken      string // local wall clock time, takenLayout
	TakenSrc   string
	TZOffset   string
	Width      int
	Height     int
	Lat        *float64
	Lon        *float64
	Camera     string
	HasPreview bool
	Error      string
	IndexedAt  int64
	Hash       string // SHA-256 of the content, empty if not computed
	PHash      *int64 // perceptual hash, nil if not computed
}

// TakenTime returns the date taken as a time.Time in the UTC location holding
// the wall clock time.
func (m *Media) TakenTime() time.Time {
	t, _ := time.Parse(takenLayout, m.Taken)
	return t
}

type store struct {
	db *sql.DB
}

func (s *store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var current int
	err := s.db.QueryRow(`SELECT version FROM schema_version LIMIT 1`).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.Exec(`INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if current > schemaVersion {
		return fmt.Errorf("the photo index database has version %d, newer than the supported version %d",
			current, schemaVersion)
	}
	for v := current; v < schemaVersion; v++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback() //nolint:errcheck
			return fmt.Errorf("migration to version %d failed: %w", v+1, err)
		}
		if _, err := tx.Exec(`UPDATE schema_version SET version = ?`, v+1); err != nil {
			tx.Rollback() //nolint:errcheck
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) close() error {
	return s.db.Close()
}

// prefixRange returns the bounds of the paths strictly inside dir: every such
// path is >= dir+"/" and < dir+"0" ("0" is the byte after "/"). Range queries
// use the unique index on path, unlike LIKE.
func prefixRange(dir string) (string, string) {
	dir = strings.TrimSuffix(dir, "/")
	return dir + "/", dir + "0"
}

const mediaColumns = `id, path, size, mtime, kind, taken, taken_src, tz_offset, width, height, lat, lon,
	camera, has_preview, error, indexed_at, hash, phash`

type scanner interface {
	Scan(dest ...any) error
}

func scanMedia(row scanner) (Media, error) {
	var m Media
	var lat, lon sql.NullFloat64
	var phash sql.NullInt64
	err := row.Scan(&m.ID, &m.Path, &m.Size, &m.ModTime, &m.Kind, &m.Taken, &m.TakenSrc, &m.TZOffset,
		&m.Width, &m.Height, &lat, &lon, &m.Camera, &m.HasPreview, &m.Error, &m.IndexedAt, &m.Hash, &phash)
	if lat.Valid && lon.Valid {
		m.Lat, m.Lon = &lat.Float64, &lon.Float64
	}
	if phash.Valid {
		m.PHash = &phash.Int64
	}
	return m, err
}

func (s *store) get(fsPath string) (Media, bool, error) {
	m, err := scanMedia(s.db.QueryRow(`SELECT `+mediaColumns+` FROM media WHERE path = ?`, fsPath))
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, nil
	}
	return m, err == nil, err
}

type fileSig struct {
	id    int64
	size  int64
	mtime int64
}

// signatures returns the size and modification time of every file indexed
// under dir, used by the scanner to skip unchanged files without querying the
// database once per file.
func (s *store) signatures(dir string) (map[string]fileSig, error) {
	lo, hi := prefixRange(dir)
	rows, err := s.db.Query(`SELECT id, path, size, mtime FROM media WHERE path >= ? AND path < ?`, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := make(map[string]fileSig)
	for rows.Next() {
		var p string
		var sig fileSig
		if err := rows.Scan(&sig.id, &p, &sig.size, &sig.mtime); err != nil {
			return nil, err
		}
		res[p] = sig
	}
	return res, rows.Err()
}

// upsert inserts or replaces the row for m.Path and returns its id. The id of
// an existing row is preserved so its preview file name stays valid.
func (s *store) upsert(m *Media) (int64, error) {
	var lat, lon any
	if m.Lat != nil && m.Lon != nil {
		lat, lon = *m.Lat, *m.Lon
	}
	var id int64
	err := s.db.QueryRow(`INSERT INTO media (path, size, mtime, kind, taken, taken_src, tz_offset, width, height,
		lat, lon, camera, has_preview, error, indexed_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime, kind=excluded.kind,
		taken=excluded.taken, taken_src=excluded.taken_src, tz_offset=excluded.tz_offset, width=excluded.width,
		height=excluded.height, lat=excluded.lat, lon=excluded.lon, camera=excluded.camera,
		has_preview=excluded.has_preview, error=excluded.error, indexed_at=excluded.indexed_at,
		hash='', phash=NULL, phash_tried=0
		RETURNING id`,
		m.Path, m.Size, m.ModTime, m.Kind, m.Taken, m.TakenSrc, m.TZOffset, m.Width, m.Height, lat, lon,
		m.Camera, m.HasPreview, m.Error, m.IndexedAt).Scan(&id)
	return id, err
}

func (s *store) setPreview(id int64, hasPreview bool, errMsg string) error {
	_, err := s.db.Exec(`UPDATE media SET has_preview = ?, error = ? WHERE id = ?`, hasPreview, errMsg, id)
	return err
}

// deleteTree removes fsPath and, if it is a directory, everything indexed
// below it. It returns the ids of the removed rows so their previews can be
// deleted.
func (s *store) deleteTree(fsPath string) ([]int64, error) {
	lo, hi := prefixRange(fsPath)
	rows, err := s.db.Query(`DELETE FROM media WHERE path = ? OR (path >= ? AND path < ?) RETURNING id`,
		fsPath, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *store) deleteIDs(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM media WHERE id = ?`, id); err != nil {
			tx.Rollback() //nolint:errcheck
			return err
		}
	}
	return tx.Commit()
}

// renameTree moves the rows for oldPath, and everything below it, to newPath.
// Rows already present at the destination are replaced. It returns the ids of
// the replaced rows so their previews can be deleted.
func (s *store) renameTree(oldPath, newPath string) ([]int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	oldLo, oldHi := prefixRange(oldPath)
	rows, err := tx.Query(`SELECT id, path FROM media WHERE path = ? OR (path >= ? AND path < ?)`,
		oldPath, oldLo, oldHi)
	if err != nil {
		return nil, err
	}
	type entry struct {
		id   int64
		path string
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.path); err != nil {
			rows.Close()
			return nil, err
		}
		entries = append(entries, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var replaced []int64
	for _, e := range entries {
		dst := newPath + strings.TrimPrefix(e.path, oldPath)
		var existing int64
		err := tx.QueryRow(`DELETE FROM media WHERE path = ? RETURNING id`, dst).Scan(&existing)
		if err == nil {
			replaced = append(replaced, existing)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE media SET path = ? WHERE id = ?`, dst, e.id); err != nil {
			return nil, err
		}
	}
	return replaced, tx.Commit()
}

// allPaths calls fn for every indexed path.
func (s *store) allPaths(fn func(id int64, fsPath string)) error {
	rows, err := s.db.Query(`SELECT id, path FROM media`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return err
		}
		fn(id, p)
	}
	return rows.Err()
}

// dateFilter restricts a query on the date taken and the size.
type dateFilter struct {
	from        string // inclusive, takenLayout, empty means unbounded
	to          string // exclusive, takenLayout, empty means unbounded
	unknownOnly bool   // only files without a real date (taken from the mtime)
	minSize     int64  // inclusive, 0 means unbounded
	maxSize     int64  // inclusive, 0 means unbounded
	sortBySize  bool   // largest first instead of newest first
	onlyHashed  bool   // only files whose content hash is shared with another file
	onlyPHash   bool   // only files with a perceptual hash
}

// query calls fn, newest first, for every file indexed inside one of dirs
// that matches the filter, until fn returns false.
func (s *store) query(dirs []string, f dateFilter, fn func(Media) bool) error {
	if len(dirs) == 0 {
		return nil
	}
	var where []string
	var args []any
	var dirConds []string
	for _, d := range dirs {
		lo, hi := prefixRange(d)
		dirConds = append(dirConds, `(path >= ? AND path < ?)`)
		args = append(args, lo, hi)
	}
	where = append(where, "("+strings.Join(dirConds, " OR ")+")")
	if f.from != "" {
		where = append(where, `taken >= ?`)
		args = append(args, f.from)
	}
	if f.to != "" {
		where = append(where, `taken < ?`)
		args = append(args, f.to)
	}
	if f.unknownOnly {
		where = append(where, `taken_src = ?`)
		args = append(args, TakenSrcModTime)
	}
	if f.minSize > 0 {
		where = append(where, `size >= ?`)
		args = append(args, f.minSize)
	}
	if f.maxSize > 0 {
		where = append(where, `size <= ?`)
		args = append(args, f.maxSize)
	}
	if f.onlyHashed {
		where = append(where, `hash NOT IN ('', 'error') AND hash IN
			(SELECT hash FROM media WHERE hash NOT IN ('', 'error') GROUP BY hash HAVING COUNT(*) > 1)`)
	}
	if f.onlyPHash {
		where = append(where, `phash IS NOT NULL`)
	}
	order := ` ORDER BY taken DESC, path`
	if f.sortBySize {
		order = ` ORDER BY size DESC, path`
	}
	rows, err := s.db.Query(`SELECT `+mediaColumns+` FROM media WHERE `+strings.Join(where, " AND ")+
		order, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return err
		}
		if !fn(m) {
			break
		}
	}
	return rows.Err()
}

// Counts summarizes the index content.
type Counts struct {
	Total       int64 `json:"total"`
	WithDate    int64 `json:"with_date"`
	WithPreview int64 `json:"with_preview"`
	Errors      int64 `json:"errors"`
}

func (s *store) counts() (Counts, error) {
	var c Counts
	err := s.db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN taken_src != ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(has_preview), 0),
		COALESCE(SUM(CASE WHEN error != '' THEN 1 ELSE 0 END), 0) FROM media`, TakenSrcModTime).
		Scan(&c.Total, &c.WithDate, &c.WithPreview, &c.Errors)
	return c, err
}

// hashCandidates returns up to limit files that share their size with another
// file and whose content hash is not computed yet. Only these can be exact
// duplicates, so most files are never read for hashing.
func (s *store) hashCandidates(limit int) ([]Media, error) {
	rows, err := s.db.Query(`SELECT `+mediaColumns+` FROM media WHERE hash = '' AND size > 0 AND size IN
		(SELECT size FROM media WHERE size > 0 GROUP BY size HAVING COUNT(*) > 1) ORDER BY size LIMIT ?`, limit)
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

// setHash stores the content hash, if the file was not changed meanwhile.
func (s *store) setHash(m *Media, hash string) error {
	_, err := s.db.Exec(`UPDATE media SET hash = ? WHERE id = ? AND size = ? AND mtime = ?`,
		hash, m.ID, m.Size, m.ModTime)
	return err
}

// phashCandidates returns up to limit images whose perceptual hash has not been
// attempted yet.
func (s *store) phashCandidates(limit int) ([]Media, error) {
	rows, err := s.db.Query(`SELECT `+mediaColumns+` FROM media WHERE phash IS NULL AND phash_tried = 0
		AND kind != ? LIMIT ?`, KindVideo, limit)
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

// setPHash stores the perceptual hash (nil if it could not be computed), if
// the file was not changed meanwhile.
func (s *store) setPHash(m *Media, phash *int64) error {
	var v any
	if phash != nil {
		v = *phash
	}
	_, err := s.db.Exec(`UPDATE media SET phash = ?, phash_tried = 1 WHERE id = ? AND size = ? AND mtime = ?`,
		v, m.ID, m.Size, m.ModTime)
	return err
}

// pendingDuplicateWork returns the number of files still to hash or
// fingerprint.
func (s *store) pendingDuplicateWork() (int64, error) {
	var hashes, phashes int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM media WHERE hash = '' AND size > 0 AND size IN
		(SELECT size FROM media WHERE size > 0 GROUP BY size HAVING COUNT(*) > 1)`).Scan(&hashes)
	if err != nil {
		return 0, err
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM media WHERE phash IS NULL AND phash_tried = 0 AND kind != ?`,
		KindVideo).Scan(&phashes)
	return hashes + phashes, err
}
