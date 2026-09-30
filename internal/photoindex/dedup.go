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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	// Decoders for computing perceptual hashes without a preview.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"

	"github.com/drakkan/sftpgo/v2/internal/logger"
)

const (
	// similarMaxDistance is the maximum Hamming distance between the
	// perceptual hashes of two photos considered similar. Resized or
	// re-compressed copies are usually within 3; unrelated photos are around
	// 32. Bursts of nearly identical shots can also match, which is fine for
	// a review list.
	similarMaxDistance = 6
	// dupBatchSize is the number of files hashed per database round trip.
	dupBatchSize = 100
	// dupIdleDelay is how long the duplicate worker waits after activity
	// (uploads, scans) before resuming, so it does not compete for the disk.
	dupIdleDelay = 5 * time.Second
)

// goDecodable are the formats Go can decode to compute a perceptual hash
// when there is no preview (e.g. libvips is not installed).
var goDecodable = map[string]bool{
	".jpg": true, ".jpeg": true, ".jpe": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true,
}

// dHash computes a 64-bit difference hash: the image is reduced to a 9x8
// grayscale grid (averaging every pixel, so noise and compression artifacts
// cancel out) and each bit records whether a cell is brighter than its right
// neighbour. Visually identical images have (nearly) identical hashes
// whatever their size, format or compression.
func dHash(img image.Image) int64 {
	const w, h = 9, 8
	var sum [w * h]float64
	var cnt [w * h]float64
	b := img.Bounds()
	bw, bh := b.Dx(), b.Dy()
	if bw <= 0 || bh <= 0 {
		return 0
	}
	cell := func(x, y int) int {
		cx := (x - b.Min.X) * w / bw
		cy := (y - b.Min.Y) * h / bh
		return cy*w + cx
	}
	if ycc, ok := img.(*image.YCbCr); ok {
		// JPEG fast path: the Y plane is the luminance.
		for y := b.Min.Y; y < b.Max.Y; y++ {
			row := ycc.Y[(y-ycc.Rect.Min.Y)*ycc.YStride:]
			for x := b.Min.X; x < b.Max.X; x++ {
				i := cell(x, y)
				sum[i] += float64(row[x-ycc.Rect.Min.X])
				cnt[i]++
			}
		}
	} else {
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				g := color.GrayModel.Convert(img.At(x, y)).(color.Gray)
				i := cell(x, y)
				sum[i] += float64(g.Y)
				cnt[i]++
			}
		}
	}
	var hash uint64
	for y := range h {
		for x := range w - 1 {
			l, r := y*w+x, y*w+x+1
			if sum[l]/max(cnt[l], 1) > sum[r]/max(cnt[r], 1) {
				hash |= 1 << uint(y*(w-1)+x)
			}
		}
	}
	return int64(hash)
}

// hamming returns the number of differing bits between two hashes.
func hamming(a, b int64) int {
	return bits.OnesCount64(uint64(a ^ b))
}

// computePHash returns the perceptual hash of an indexed image, from its
// preview if there is one or else from the source file if Go can decode it.
func (m *Manager) computePHash(rec *Media) (*int64, error) {
	src := ""
	if rec.HasPreview {
		src = m.previewPath(rec.ID)
	} else if goDecodable[strings.ToLower(filepath.Ext(rec.Path))] {
		if m.cfg.MaxSourceSize > 0 && rec.Size > int64(m.cfg.MaxSourceSize)*1024*1024 {
			return nil, errors.New("file too large")
		}
		src = rec.Path
	} else {
		return nil, errors.New("no preview")
	}
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("unable to decode image: %w", err)
	}
	h := dHash(img)
	return &h, nil
}

// imageDimensions reads the size of an image from its header, or returns zeros.
func imageDimensions(fsPath string) (int, int) {
	f, err := os.Open(fsPath)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// hashFile returns the hex SHA-256 of the file content.
func hashFile(fsPath string) (string, error) {
	f, err := os.Open(fsPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 1024*1024)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// kickDuplicates wakes the duplicate worker after new files were indexed.
func (m *Manager) kickDuplicates() {
	select {
	case m.dupCh <- struct{}{}:
	default:
	}
}

// busy reports whether files are being scanned or indexed.
func (m *Manager) busy() bool {
	return m.scanning.Load() || len(m.prioQueue) > 0 || len(m.backfillQueue) > 0 || m.inFlight.Load() > 0
}

// dupLoop computes, in the background and only while the indexer is idle, the
// perceptual hashes missing (e.g. after an upgrade) and the content hashes of
// the files that could be exact duplicates.
func (m *Manager) dupLoop() {
	for {
		select {
		case <-m.dupCh:
		case <-m.stop:
			return
		}
		for {
			// Let bursts of uploads and scans finish first.
			for m.busy() {
				if !m.sleep(dupIdleDelay) {
					return
				}
			}
			n, err := m.dupBatch()
			if err != nil {
				logger.Warn(logSender, "", "duplicate detection: %v", err)
				break
			}
			if n == 0 || m.stopping() {
				break
			}
		}
	}
}

// dupBatch processes one batch of pending perceptual and content hashes and
// returns how many files it processed.
func (m *Manager) dupBatch() (int, error) {
	processed := 0
	recs, err := m.store.phashCandidates(dupBatchSize)
	if err != nil {
		return 0, err
	}
	for i := range recs {
		if m.stopping() || m.busy() {
			return processed, nil
		}
		ph, err := m.computePHash(&recs[i])
		if err != nil {
			logger.Debug(logSender, "", "perceptual hash of %q not computed: %v", recs[i].Path, err)
		}
		if err := m.store.setPHash(&recs[i], ph); err != nil {
			return processed, err
		}
		processed++
	}
	recs, err = m.store.hashCandidates(dupBatchSize)
	if err != nil {
		return processed, err
	}
	for i := range recs {
		if m.stopping() || m.busy() {
			return processed, nil
		}
		h, err := hashFile(recs[i].Path)
		if err != nil {
			// Missing or unreadable: mark it so it is not retried forever; a
			// change to the file resets the hash.
			logger.Debug(logSender, "", "unable to hash %q: %v", recs[i].Path, err)
			h = "error"
		}
		if err := m.store.setHash(&recs[i], h); err != nil {
			return processed, err
		}
		processed++
		if m.cfg.PauseBetweenFiles > 0 && !m.sleep(time.Duration(m.cfg.PauseBetweenFiles)*time.Millisecond) {
			return processed, nil
		}
	}
	return processed, nil
}

// DupFile is a file in a duplicate group, as seen by the searching user.
type DupFile struct {
	Media
	VirtualPath string
	// Keep marks the copy suggested to keep in the group.
	Keep bool
}

// DupGroup is a set of files with the same content (exact) or that look the
// same (similar).
type DupGroup struct {
	Files []DupFile
	// Exact is true if all the files have the same content.
	Exact bool
	// Reclaimable is the space freed by deleting every copy but the kept one.
	Reclaimable int64
}

// Visibility maps an indexed file to the virtual path a user sees it at, or
// returns false if the user cannot see it. See UserScope.
type Visibility func(fsPath string) (string, bool)

// FindDuplicates returns the duplicate groups among the files inside dirs that
// visible reports as visible, the groups wasting the most space first. Only
// the files the user can see are considered: a group needs at least two of
// them.
func (m *Manager) FindDuplicates(dirs []string, q Query, visible Visibility) ([]DupGroup, error) {
	f := q.filter
	var files []DupFile
	if q.Duplicates == DupExact {
		f.onlyHashed = true
	} else {
		f.onlyPHash = true
	}
	f.sortBySize = false
	err := m.store.query(dirs, f, func(rec Media) bool {
		if vp, ok := visible(rec.Path); ok {
			files = append(files, DupFile{Media: rec, VirtualPath: vp})
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	var groups [][]int
	if q.Duplicates == DupExact {
		groups = groupExact(files)
	} else {
		groups = groupSimilar(files, similarMaxDistance)
	}
	res := make([]DupGroup, 0, len(groups))
	for _, g := range groups {
		// The index may lag behind a deletion by a moment: only list files
		// that still exist, so a refreshed list never shows deleted copies.
		existing := g[:0]
		for _, i := range g {
			if _, err := os.Stat(files[i].Path); err == nil {
				existing = append(existing, i)
			}
		}
		if len(existing) > 1 {
			res = append(res, newDupGroup(files, existing))
		}
	}
	slices.SortStableFunc(res, func(a, b DupGroup) int {
		if a.Reclaimable != b.Reclaimable {
			if a.Reclaimable > b.Reclaimable {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Files[0].VirtualPath, b.Files[0].VirtualPath)
	})
	return res, nil
}

func groupExact(files []DupFile) [][]int {
	byHash := make(map[string][]int)
	var order []string
	for i := range files {
		h := files[i].Hash
		if _, ok := byHash[h]; !ok {
			order = append(order, h)
		}
		byHash[h] = append(byHash[h], i)
	}
	var res [][]int
	for _, h := range order {
		if len(byHash[h]) > 1 {
			res = append(res, byHash[h])
		}
	}
	return res
}

// groupSimilar groups the files whose perceptual hashes differ by at most
// maxDist bits. Candidates are found by splitting the 64 bits into 8 bytes:
// two hashes within 7 bits of each other necessarily share at least one byte
// (pigeonhole principle), so only files sharing a byte are compared instead of
// every pair.
func groupSimilar(files []DupFile, maxDist int) [][]int {
	uf := newUnionFind(len(files))
	var buckets [8]map[uint8][]int
	for band := range buckets {
		buckets[band] = make(map[uint8][]int)
	}
	for i := range files {
		if featureless(*files[i].PHash) {
			continue
		}
		h := uint64(*files[i].PHash)
		for band := range 8 {
			key := uint8(h >> (8 * band))
			for _, j := range buckets[band][key] {
				if uf.find(i) != uf.find(j) && similar(&files[i], &files[j], maxDist) {
					uf.union(i, j)
				}
			}
			buckets[band][key] = append(buckets[band][key], i)
		}
	}
	return uf.groups()
}

// similar reports whether two files with perceptual hashes look the same and
// are not a pair kept on purpose.
func similar(a, b *DupFile, maxDist int) bool {
	return hamming(*a.PHash, *b.PHash) <= maxDist && sameAspect(a, b) && !intentionalPair(a, b)
}

// unionFind tracks which files belong to the same group.
type unionFind struct {
	parent []int
}

func newUnionFind(n int) *unionFind {
	uf := &unionFind{parent: make([]int, n)}
	for i := range uf.parent {
		uf.parent[i] = i
	}
	return uf
}

func (uf *unionFind) find(i int) int {
	for uf.parent[i] != i {
		uf.parent[i] = uf.parent[uf.parent[i]]
		i = uf.parent[i]
	}
	return i
}

func (uf *unionFind) union(i, j int) {
	uf.parent[uf.find(i)] = uf.find(j)
}

// groups returns the sets with more than one member, in order of first member.
func (uf *unionFind) groups() [][]int {
	byRoot := make(map[int][]int)
	var roots []int
	for i := range uf.parent {
		r := uf.find(i)
		if _, ok := byRoot[r]; !ok {
			roots = append(roots, r)
		}
		byRoot[r] = append(byRoot[r], i)
	}
	var res [][]int
	for _, r := range roots {
		if len(byRoot[r]) > 1 {
			res = append(res, byRoot[r])
		}
	}
	return res
}

// featureless reports whether a perceptual hash carries too little
// information to compare: nearly all its bits are equal, as for plain skies,
// dark frames or smooth gradients, which would all look "similar".
func featureless(h int64) bool {
	n := bits.OnesCount64(uint64(h))
	return n <= 4 || n >= 60
}

// sameAspect reports whether two images have the same shape (within 3%), or
// their dimensions are unknown. A resized or re-encoded copy keeps its aspect
// ratio, while the dHash of e.g. a portrait and a landscape photo could match
// by chance.
func sameAspect(a, b *DupFile) bool {
	if a.Width <= 0 || a.Height <= 0 || b.Width <= 0 || b.Height <= 0 {
		return true
	}
	ra := float64(a.Width) / float64(a.Height)
	rb := float64(b.Width) / float64(b.Height)
	return max(ra, rb)/min(ra, rb) <= 1.03
}

// intentionalPair reports whether two files are variants kept on purpose:
// the same name with a different extension in the same folder, such as the
// RAW+JPEG pairs written by cameras or HEIC+JPG from phones.
func intentionalPair(a, b *DupFile) bool {
	if filepath.Dir(a.Path) != filepath.Dir(b.Path) {
		return false
	}
	stem := func(p string) string {
		base := filepath.Base(p)
		return strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base)))
	}
	return stem(a.Path) == stem(b.Path) && !strings.EqualFold(filepath.Ext(a.Path), filepath.Ext(b.Path))
}

// newDupGroup builds a group and picks the copy to keep: the highest
// resolution, then the largest file (least compressed), then the one with the
// oldest modification time (the original), then the shortest path. The kept
// copy is first.
func newDupGroup(files []DupFile, idx []int) DupGroup {
	g := DupGroup{Exact: true}
	for _, i := range idx {
		g.Files = append(g.Files, files[i])
	}
	hash := g.Files[0].Hash
	for _, f := range g.Files {
		if f.Hash == "" || f.Hash != hash {
			g.Exact = false
		}
	}
	slices.SortStableFunc(g.Files, func(a, b DupFile) int {
		pa, pb := int64(a.Width)*int64(a.Height), int64(b.Width)*int64(b.Height)
		switch {
		case pa != pb:
			return cmpDesc(pa, pb)
		case a.Size != b.Size:
			return cmpDesc(a.Size, b.Size)
		case a.ModTime != b.ModTime:
			return -cmpDesc(a.ModTime, b.ModTime)
		case len(a.VirtualPath) != len(b.VirtualPath):
			return len(a.VirtualPath) - len(b.VirtualPath)
		}
		return strings.Compare(a.VirtualPath, b.VirtualPath)
	})
	g.Files[0].Keep = true
	// Files kept on purpose next to the kept copy (RAW+JPEG, HEIC+JPG) are
	// kept too: they can join a group through a copy stored elsewhere.
	for i := 1; i < len(g.Files); i++ {
		if intentionalPair(&g.Files[0], &g.Files[i]) {
			g.Files[i].Keep = true
		}
	}
	for _, f := range g.Files {
		if !f.Keep {
			g.Reclaimable += f.Size
		}
	}
	return g
}

func cmpDesc(a, b int64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}
