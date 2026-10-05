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
	"slices"
	"strings"
	"unicode"

	"github.com/drakkan/sftpgo/v2/internal/logger"
)

// "Visible text": the text in the photos (signs, documents, screenshots,
// shirts...) is read once per photo by the OCR model of the machine-learning
// service and stored in the index, where text: searches look for it.

// ocrDisableAfter is the number of photos in a row whose analysis fails only
// when the text is requested after which text reading is turned off: the
// service is likely too old to support it, and the other analyses must go on.
const ocrDisableAfter = 3

// ErrTextUnavailable is returned by a "text:" search when text reading is
// not configured or not supported by the machine-learning service.
var ErrTextUnavailable = errors.New(`"text:" search is not available`)

// normalizeText prepares a text for search: lower case, words separated by
// single spaces.
func normalizeText(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), unicode.IsSpace), " ")
}

// storeOCR stores the text read in a photo, unless the photo changed
// meanwhile.
func (m *Manager) storeOCR(rec *Media, lines []string) error {
	text := strings.Join(lines, "\n")
	_, err := m.store.db.Exec(`UPDATE media SET ocr_text = ?, ocr_search = ?, ocr_state = ?
		WHERE id = ? AND size = ? AND mtime = ?`, text, normalizeText(text), facesDone, rec.ID, rec.Size, rec.ModTime)
	return err
}

// ocrFailed records a failure of an analysis that included the text, and
// turns text reading off after too many in a row.
func (m *Manager) ocrFailed(err error) {
	n := m.ocrFailures.Add(1)
	if n >= ocrDisableAfter && m.ocrOn.CompareAndSwap(true, false) {
		logger.Warn(logSender, "", "reading the text in photos failed for %d photos in a row, it is turned off "+
			"until the next restart; the machine-learning service may be too old: %v", n, err)
	}
}

// withMatchedText wraps a search callback to fill in the line of text that
// matched the "text:" terms.
func (m *Manager) withMatchedText(terms []string, fn func(Media) bool) func(Media) bool {
	return func(rec Media) bool {
		var text string
		if err := m.store.db.QueryRow(`SELECT ocr_text FROM media WHERE id = ?`, rec.ID).Scan(&text); err == nil {
			rec.MatchedText = matchedLine(text, terms)
		}
		return fn(rec)
	}
}

// matchedLine returns the first line of text containing one of the terms,
// shortened around the match, or the start of the text if the terms span
// several lines.
func matchedLine(text string, terms []string) string {
	const maxLen = 80
	lines := strings.Split(text, "\n")
	best := lines[0]
	for _, line := range lines {
		norm := normalizeText(line)
		if slices.ContainsFunc(terms, func(t string) bool { return strings.Contains(norm, t) }) {
			best = line
			break
		}
	}
	if r := []rune(best); len(r) > maxLen {
		best = string(r[:maxLen-1]) + "…"
	}
	return best
}

// OCREnabled reports whether "visible text" search is configured and
// working.
func (m *Manager) OCREnabled() bool {
	return m != nil && m.ocrOn.Load()
}

// CheckOCR sends one image to the OCR model of the machine-learning service
// and returns the text read, to verify the setup.
func CheckOCR(ctx context.Context, url, model string, jpeg []byte) ([]string, error) {
	c := newMLClient(url, Config{OCRModel: model})
	if err := c.ping(ctx); err != nil {
		return nil, err
	}
	res, err := c.analyze(ctx, jpeg, mlTasks{ocr: true})
	if err != nil {
		return nil, err
	}
	return res.Text, nil
}
