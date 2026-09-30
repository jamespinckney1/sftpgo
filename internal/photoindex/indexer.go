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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/drakkan/sftpgo/v2/internal/logger"
)

const (
	priorityQueueSize = 4096
	backfillQueueSize = 64
	eventQueueSize    = 4096
	// rootsRefreshInterval limits how often an event for a path outside every
	// known root triggers a new discovery of the roots (e.g. for a new user).
	rootsRefreshInterval = time.Minute
)

// Filesystem event operations, as reported by the common package.
const (
	OpUpload = "upload"
	OpDelete = "delete"
	OpRmdir  = "rmdir"
	OpRename = "rename"
	OpCopy   = "copy"
)

// RootsFunc returns the directories to index. The default implementation
// returns the home directories and virtual folders of the local-filesystem
// users; tests replace it.
type RootsFunc func() ([]string, error)

// Manager owns the index database and the background indexing goroutines.
type Manager struct {
	cfg          Config
	dataDir      string
	previewDir   string
	store        *store
	exif         *exiftool
	exiftoolPath string
	vipsPath     string
	rootsFunc    RootsFunc

	prioQueue     chan string
	backfillQueue chan string
	events        chan fsEvent
	rescanCh      chan struct{}
	stop          chan struct{}
	wg            sync.WaitGroup

	rootsMu         sync.RWMutex
	roots           []string
	rootsRefreshed  time.Time
	scanning        atomic.Bool
	lastScanEnd     atomic.Int64
	processed       atomic.Int64
	scanQueued      atomic.Int64
	scanDone        atomic.Int64
	previewDisabled bool
}

type fsEvent struct {
	op     string
	path   string
	target string
}

var (
	mgrMu sync.RWMutex
	mgr   *Manager
)

// Get returns the running index manager, or nil if the index is disabled.
func Get() *Manager {
	mgrMu.RLock()
	defer mgrMu.RUnlock()
	return mgr
}

// Initialize starts the photo index with the given configuration, stopping any
// previously started instance. If the index is disabled it only stops the
// previous instance. rootsFunc may be nil to index the local users' files.
func Initialize(cfg Config, configDir string, rootsFunc RootsFunc) error {
	mgrMu.Lock()
	defer mgrMu.Unlock()
	if mgr != nil {
		mgr.Stop()
		mgr = nil
	}
	if !cfg.Enabled {
		logger.Info(logSender, "", "photo index disabled")
		return nil
	}
	m, err := newManager(cfg, configDir, rootsFunc)
	if err != nil {
		logger.Error(logSender, "", "unable to start the photo index: %v", err)
		return err
	}
	m.start()
	mgr = m
	return nil
}

func newManager(cfg Config, configDir string, rootsFunc RootsFunc) (*Manager, error) {
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if rootsFunc == nil {
		rootsFunc = localUserRoots
	}
	dataDir := cfg.resolveDataDir(configDir)
	previewDir := filepath.Join(dataDir, "previews")
	if err := os.MkdirAll(previewDir, 0700); err != nil {
		return nil, fmt.Errorf("unable to create data dir %q: %w", dataDir, err)
	}
	s, err := openStore(filepath.Join(dataDir, "index.db"))
	if err != nil {
		return nil, fmt.Errorf("unable to open the index database: %w", err)
	}
	m := &Manager{
		cfg:           cfg,
		dataDir:       dataDir,
		previewDir:    previewDir,
		store:         s,
		exiftoolPath:  resolveTool(cfg.ExiftoolPath, "exiftool"),
		vipsPath:      resolveTool(cfg.VipsPath, "vipsthumbnail"),
		rootsFunc:     rootsFunc,
		prioQueue:     make(chan string, priorityQueueSize),
		backfillQueue: make(chan string, backfillQueueSize),
		events:        make(chan fsEvent, eventQueueSize),
		rescanCh:      make(chan struct{}, 1),
		stop:          make(chan struct{}),
	}
	if m.exiftoolPath != "" {
		m.exif = newExiftool(m.exiftoolPath)
	}
	m.previewDisabled = cfg.PreviewSize <= 0 || m.vipsPath == ""
	logger.Info(logSender, "", "photo index enabled, data dir %q, exiftool: %q, vipsthumbnail: %q, workers: %d",
		dataDir, m.exiftoolPath, m.vipsPath, cfg.Workers)
	if m.exiftoolPath == "" {
		logger.Warn(logSender, "", "exiftool not found: dates will be derived from file names and modification times only")
	}
	if m.vipsPath == "" {
		logger.Warn(logSender, "", "vipsthumbnail not found: no previews will be generated (no HEIC/RAW thumbnails)")
	}
	return m, nil
}

func (m *Manager) start() {
	for range m.cfg.Workers {
		m.wg.Go(m.worker)
	}
	m.wg.Go(m.eventLoop)
	m.wg.Go(m.scanLoop)
}

// Stop stops the background goroutines and closes the database.
func (m *Manager) Stop() {
	close(m.stop)
	m.wg.Wait()
	if m.exif != nil {
		m.exif.close()
	}
	m.store.close() //nolint:errcheck
}

// Rescan requests a full scan as soon as the current one, if any, completes.
func (m *Manager) Rescan() {
	select {
	case m.rescanCh <- struct{}{}:
	default:
	}
}

func (m *Manager) stopping() bool {
	select {
	case <-m.stop:
		return true
	default:
		return false
	}
}

func (m *Manager) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-m.stop:
		return false
	}
}

func (m *Manager) scanLoop() {
	if !m.sleep(time.Duration(m.cfg.StartupDelay) * time.Second) {
		return
	}
	for {
		m.scan()
		var interval <-chan time.Time
		var timer *time.Timer
		if m.cfg.RescanInterval > 0 {
			timer = time.NewTimer(time.Duration(m.cfg.RescanInterval) * time.Hour)
			interval = timer.C
		}
		select {
		case <-interval:
		case <-m.rescanCh:
		case <-m.stop:
			if timer != nil {
				timer.Stop()
			}
			return
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// refreshRoots reloads the directories to index, dropping directories nested
// inside other ones so no file is walked twice.
func (m *Manager) refreshRoots() []string {
	roots, err := m.rootsFunc()
	if err != nil {
		logger.Warn(logSender, "", "unable to get the directories to index: %v", err)
		m.rootsMu.RLock()
		defer m.rootsMu.RUnlock()
		return m.roots
	}
	roots = normalizeRoots(roots)
	m.rootsMu.Lock()
	m.roots = roots
	m.rootsRefreshed = time.Now()
	m.rootsMu.Unlock()
	return roots
}

func normalizeRoots(roots []string) []string {
	cleaned := make([]string, 0, len(roots))
	for _, r := range roots {
		if r == "" || !filepath.IsAbs(r) {
			continue
		}
		cleaned = append(cleaned, filepath.Clean(r))
	}
	slices.Sort(cleaned)
	cleaned = slices.Compact(cleaned)
	res := cleaned[:0]
	for _, r := range cleaned {
		if len(res) > 0 && isInside(r, res[len(res)-1]) {
			continue
		}
		res = append(res, r)
	}
	return res
}

// isInside reports whether p is dir or a path below dir.
func isInside(p, dir string) bool {
	if dir == "/" {
		return strings.HasPrefix(p, "/")
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// underRoot reports whether p is inside a known root, refreshing the roots at
// most once per rootsRefreshInterval if p is outside all of them.
func (m *Manager) underRoot(p string) bool {
	m.rootsMu.RLock()
	roots := m.roots
	refreshed := m.rootsRefreshed
	m.rootsMu.RUnlock()
	for _, r := range roots {
		if isInside(p, r) {
			return true
		}
	}
	if time.Since(refreshed) < rootsRefreshInterval {
		return false
	}
	for _, r := range m.refreshRoots() {
		if isInside(p, r) {
			return true
		}
	}
	return false
}

// skipDir reports whether a directory should not be indexed: hidden
// directories and the metadata folders of common NAS systems.
func skipDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "@eaDir", "#recycle", "#snapshot", "$RECYCLE.BIN", "lost+found":
		return true
	}
	return false
}

// scan walks every root, queues new and changed files and removes from the
// index the files that no longer exist.
func (m *Manager) scan() {
	m.scanning.Store(true)
	m.scanQueued.Store(0)
	m.scanDone.Store(0)
	defer func() {
		m.scanning.Store(false)
		m.lastScanEnd.Store(time.Now().Unix())
	}()

	start := time.Now()
	roots := m.refreshRoots()
	logger.Info(logSender, "", "photo index scan started, directories: %v", roots)
	for _, root := range roots {
		if !m.scanRoot(root) {
			return
		}
	}
	m.removeOrphans(roots)
	logger.Info(logSender, "", "photo index scan completed in %s, files queued: %d",
		time.Since(start).Round(time.Second), m.scanQueued.Load())
}

func (m *Manager) scanRoot(root string) bool {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		// The disk may be unmounted: never treat this as "everything was deleted".
		logger.Warn(logSender, "", "skipping directory %q, not accessible: %v", root, err)
		return true
	}
	known, err := m.store.signatures(root)
	if err != nil {
		logger.Error(logSender, "", "unable to read the index for %q: %v", root, err)
		return true
	}
	res := m.walkRoot(root, known)
	if res.stopped {
		return false
	}
	if res.err != nil || res.errors > 0 {
		logger.Warn(logSender, "", "scan of %q had %d errors, stale entries are kept: %v", root, res.errors, res.err)
		return true
	}
	if len(res.seen) == 0 && len(known) > 0 {
		logger.Warn(logSender, "", "scan of %q found no files but %d are indexed, stale entries are kept",
			root, len(known))
		return true
	}
	m.pruneDeleted(root, known, res.seen)
	return true
}

type walkResult struct {
	seen    map[string]bool
	errors  int
	stopped bool
	err     error
}

// walkRoot walks root, queueing the new and changed media files for
// processing, and returns the media files found.
func (m *Manager) walkRoot(root string, known map[string]fileSig) walkResult {
	res := walkResult{seen: make(map[string]bool, len(known))}
	res.err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if m.stopping() {
			res.stopped = true
			return filepath.SkipAll
		}
		if err != nil {
			res.errors++
			logger.Debug(logSender, "", "scan: unable to read %q: %v", p, err)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || KindForName(p) == "" {
			return nil
		}
		res.seen[p] = true
		if !changedSince(d, known[p]) {
			return nil
		}
		m.scanQueued.Add(1)
		select {
		case m.backfillQueue <- p:
			return nil
		case <-m.stop:
			res.stopped = true
			return filepath.SkipAll
		}
	})
	return res
}

// changedSince reports whether the file differs from its indexed signature
// (the zero signature for files not indexed yet).
func changedSince(d fs.DirEntry, sig fileSig) bool {
	fi, err := d.Info()
	if err != nil {
		return false
	}
	return sig.id == 0 || sig.size != fi.Size() || sig.mtime != fi.ModTime().UnixNano()
}

// pruneDeleted removes from the index the files under root that were not
// found by the last walk.
func (m *Manager) pruneDeleted(root string, known map[string]fileSig, seen map[string]bool) {
	var removed []int64
	for p, sig := range known {
		if !seen[p] {
			removed = append(removed, sig.id)
		}
	}
	if len(removed) == 0 {
		return
	}
	if err := m.store.deleteIDs(removed); err != nil {
		logger.Error(logSender, "", "unable to remove deleted files from the index: %v", err)
		return
	}
	m.removePreviews(removed)
	logger.Info(logSender, "", "removed %d deleted files under %q from the index", len(removed), root)
}

// removeOrphans removes the files outside every root, for example those of a
// deleted user. Nothing is removed if there are no roots at all.
func (m *Manager) removeOrphans(roots []string) {
	if len(roots) == 0 {
		return
	}
	var orphans []int64
	err := m.store.allPaths(func(id int64, p string) {
		for _, r := range roots {
			if isInside(p, r) {
				return
			}
		}
		orphans = append(orphans, id)
	})
	if err != nil || len(orphans) == 0 {
		return
	}
	if err := m.store.deleteIDs(orphans); err == nil {
		m.removePreviews(orphans)
		logger.Info(logSender, "", "removed %d files outside the indexed directories", len(orphans))
	}
}

func (m *Manager) worker() {
	for {
		var p string
		// Files uploaded through SFTPGo are processed before the backfill.
		select {
		case p = <-m.prioQueue:
		default:
			select {
			case p = <-m.prioQueue:
			case p = <-m.backfillQueue:
				m.scanDone.Add(1)
			case <-m.stop:
				return
			}
		}
		if err := m.processFile(p); err != nil {
			logger.Debug(logSender, "", "unable to index %q: %v", p, err)
		}
		m.processed.Add(1)
		if m.cfg.PauseBetweenFiles > 0 {
			if !m.sleep(time.Duration(m.cfg.PauseBetweenFiles) * time.Millisecond) {
				return
			}
		}
	}
}

// processFile extracts the metadata of fsPath, generates its preview and
// stores the result. Unchanged files are skipped.
func (m *Manager) processFile(fsPath string) error {
	kind := KindForName(fsPath)
	if kind == "" {
		return nil
	}
	info, err := os.Stat(fsPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return m.removeTree(fsPath)
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	existing, found, err := m.store.get(fsPath)
	if err != nil {
		return err
	}
	if found && existing.Size == info.Size() && existing.ModTime == info.ModTime().UnixNano() {
		return nil
	}

	rec := m.readMetadata(fsPath, kind, info)
	id, err := m.store.upsert(rec)
	if err != nil {
		return err
	}
	return m.updatePreview(id, rec)
}

// updatePreview generates the preview for an indexed file, if applicable, and
// records the outcome.
func (m *Manager) updatePreview(id int64, rec *Media) error {
	if rec.Kind == KindVideo || m.previewDisabled {
		return nil
	}
	if m.cfg.MaxSourceSize > 0 && rec.Size > int64(m.cfg.MaxSourceSize)*1024*1024 {
		return nil
	}
	errMsg := rec.Error
	if err := m.generatePreview(id, rec.Path, rec.Kind); err != nil {
		if errMsg != "" {
			errMsg += "; "
		}
		errMsg += "preview: " + err.Error()
		return m.store.setPreview(id, false, errMsg)
	}
	return m.store.setPreview(id, true, errMsg)
}

// readMetadata builds the index record for a file from its metadata.
func (m *Manager) readMetadata(fsPath, kind string, info os.FileInfo) *Media {
	var fields exifFields
	var errMsg string
	if m.exif != nil {
		var err error
		fields, err = m.exif.read(fsPath, kind)
		if err != nil {
			errMsg = "metadata: " + err.Error()
		}
	}
	md := resolveMetadata(fields, fsPath, info.ModTime())
	return &Media{
		Path:      fsPath,
		Size:      info.Size(),
		ModTime:   info.ModTime().UnixNano(),
		Kind:      kind,
		Taken:     md.taken.String(),
		TakenSrc:  md.takenSrc,
		TZOffset:  md.taken.offset,
		Width:     md.width,
		Height:    md.height,
		Lat:       md.lat,
		Lon:       md.lon,
		Camera:    md.camera,
		Error:     errMsg,
		IndexedAt: time.Now().Unix(),
	}
}

func (m *Manager) previewPath(id int64) string {
	return filepath.Join(m.previewDir, fmt.Sprintf("%02x", id%256), strconv.FormatInt(id, 10)+".jpg")
}

func (m *Manager) generatePreview(id int64, fsPath, kind string) error {
	dst := m.previewPath(id)
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(m.dataDir, "preview-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	src := fsPath
	if kind == KindRaw {
		if m.exiftoolPath == "" {
			return errors.New("exiftool not available")
		}
		src = filepath.Join(tmpDir, "raw.jpg")
		if err := extractRawPreview(m.exiftoolPath, fsPath, src); err != nil {
			return err
		}
	}
	tmp := filepath.Join(tmpDir, "preview.jpg")
	if err := makePreview(m.vipsPath, src, tmp, m.cfg.PreviewSize); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func (m *Manager) removePreviews(ids []int64) {
	for _, id := range ids {
		os.Remove(m.previewPath(id)) //nolint:errcheck
	}
}

func (m *Manager) removeTree(fsPath string) error {
	ids, err := m.store.deleteTree(fsPath)
	m.removePreviews(ids)
	return err
}

// OnFsEvent is called for every successful filesystem operation done through
// SFTPGo, with the real filesystem paths. It never blocks: if the event queue
// is full the event is dropped and the next scan catches up.
func (m *Manager) OnFsEvent(op, fsPath, fsTarget string) {
	switch op {
	case OpUpload, OpDelete, OpRmdir, OpRename, OpCopy:
	default:
		return
	}
	select {
	case m.events <- fsEvent{op: op, path: fsPath, target: fsTarget}:
	default:
	}
}

func (m *Manager) eventLoop() {
	for {
		select {
		case ev := <-m.events:
			m.handleEvent(ev)
		case <-m.stop:
			return
		}
	}
}

func (m *Manager) handleEvent(ev fsEvent) {
	switch ev.op {
	case OpUpload:
		m.enqueueTree(ev.path)
	case OpCopy:
		m.enqueueTree(ev.target)
	case OpDelete, OpRmdir:
		if m.underRoot(ev.path) {
			if err := m.removeTree(ev.path); err != nil {
				logger.Warn(logSender, "", "unable to remove %q from the index: %v", ev.path, err)
			}
		}
	case OpRename:
		if !m.underRoot(ev.target) {
			if err := m.removeTree(ev.path); err != nil {
				logger.Warn(logSender, "", "unable to remove %q from the index: %v", ev.path, err)
			}
			return
		}
		// Moving the rows keeps the work already done (and the previews, which
		// are named by row id) instead of indexing the files again.
		replaced, err := m.store.renameTree(ev.path, ev.target)
		if err != nil {
			logger.Warn(logSender, "", "unable to rename %q to %q in the index: %v", ev.path, ev.target, err)
		}
		m.removePreviews(replaced)
		// The target may be a file renamed to a photo extension, or a directory
		// containing files we did not know yet.
		m.enqueueTree(ev.target)
	}
}

// enqueueTree queues fsPath, or every media file below it if it is a
// directory, for priority processing. Unchanged files are skipped by the
// worker.
func (m *Manager) enqueueTree(fsPath string) {
	if !m.underRoot(fsPath) {
		return
	}
	info, err := os.Stat(fsPath)
	if err != nil {
		return
	}
	if !info.IsDir() {
		if KindForName(fsPath) != "" {
			m.enqueuePriority(fsPath)
		}
		return
	}
	filepath.WalkDir(fsPath, func(p string, d fs.DirEntry, err error) error { //nolint:errcheck
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != fsPath && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && KindForName(p) != "" {
			m.enqueuePriority(p)
		}
		return nil
	})
}

func (m *Manager) enqueuePriority(p string) {
	select {
	case m.prioQueue <- p:
	default:
		// Full: the periodic scan will pick the file up.
	}
}

// Search calls fn, newest first, for every indexed file inside one of dirs
// (absolute filesystem paths) matching the date filters in q, until fn
// returns false. The caller is responsible for the access control.
func (m *Manager) Search(dirs []string, q Query, fn func(Media) bool) error {
	return m.store.query(dirs, q.filter, fn)
}

// Lookup returns the index entry for fsPath if it is up to date with the given
// modification time and size.
func (m *Manager) Lookup(fsPath string, modTime time.Time, size int64) (Media, bool) {
	rec, found, err := m.store.get(fsPath)
	if err != nil || !found || rec.Size != size || rec.ModTime != modTime.UnixNano() {
		return Media{}, false
	}
	return rec, true
}

// PreviewPath returns the path of the preview JPEG for fsPath if it exists and
// is up to date with the given modification time and size.
func (m *Manager) PreviewPath(fsPath string, modTime time.Time, size int64) (string, bool) {
	rec, ok := m.Lookup(fsPath, modTime, size)
	if !ok || !rec.HasPreview {
		return "", false
	}
	p := m.previewPath(rec.ID)
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return p, true
}

// CanRender reports whether RenderJPEG can be used for the given file.
func (m *Manager) CanRender(name string) bool {
	if m == nil || m.vipsPath == "" {
		return false
	}
	switch KindForName(name) {
	case KindImage:
		return true
	case KindRaw:
		return m.exiftoolPath != ""
	}
	return false
}

// Status describes the index progress.
type Status struct {
	Counts
	Scanning    bool  `json:"scanning"`
	ScanQueued  int64 `json:"scan_queued"`
	ScanDone    int64 `json:"scan_done"`
	Pending     int   `json:"pending"`
	LastScanEnd int64 `json:"last_scan_end,omitempty"`
}

// Status returns the index progress.
func (m *Manager) Status() Status {
	c, err := m.store.counts()
	if err != nil {
		logger.Debug(logSender, "", "unable to count the indexed files: %v", err)
	}
	return Status{
		Counts:      c,
		Scanning:    m.scanning.Load() || m.scanDone.Load() < m.scanQueued.Load(),
		ScanQueued:  m.scanQueued.Load(),
		ScanDone:    m.scanDone.Load(),
		Pending:     len(m.prioQueue) + len(m.backfillQueue),
		LastScanEnd: m.lastScanEnd.Load(),
	}
}
