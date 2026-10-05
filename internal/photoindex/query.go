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
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Query is a parsed WebClient search query. Filters are written as
// "key:value" tokens; the remaining text is matched against file names.
//
// Photo filters, answered from the photo index:
//
//	taken:2023                  the whole year
//	taken:2023-06               a month
//	taken:2023-06-14            a day
//	taken:2023-06..2023-08      an inclusive range, either side may be omitted
//	taken:any                   every indexed photo/video, newest first
//	taken:unknown               files with no date in their metadata or name
//	is:duplicate                byte-identical copies, grouped
//	is:similar                  visually similar photos (resized, re-encoded
//	                            or exported copies), grouped
//	person:rose                 photos showing a person whose name contains
//	                            "rose"; person:"Grandma Rose" for spaces,
//	                            person:#12 for a person by id. Repeat the
//	                            filter for photos showing several people
//	place:myrtle                photos taken in a town, state or country whose
//	                            name contains "myrtle" (place:"New York")
//	show:dog                    photos showing something, described in words:
//	                            show:"dog on the beach", best matches first
//
// Size filters, for any file (with the photo filters, only photos/videos):
//
//	larger:100MB                files of at least 100 MB (B, KB, MB, GB, TB)
//	smaller:1MB                 files of at most 1 MB
//	sort:size                   largest first
type Query struct {
	// Text is the rest of the query, matched against file names.
	Text string
	// Photo is true if the query contains photo filters and must be answered
	// from the index.
	Photo bool
	// Duplicates is the duplicate detection mode, DupNone for a normal search.
	Duplicates DupMode
	// MinSize and MaxSize are the size bounds in bytes, 0 means unbounded.
	MinSize int64
	MaxSize int64
	// SortBySize is true if the results should be listed largest first.
	SortBySize bool
	// PersonTerms are the "person:" values; the photos must show a person
	// matching each of them.
	PersonTerms []string
	// Show is the description of what the photos must show ("show:" values
	// joined), ranked by similarity.
	Show   string
	filter dateFilter
}

// DupMode is a duplicate detection mode.
type DupMode int

// Duplicate detection modes.
const (
	DupNone DupMode = iota
	DupExact
	DupSimilar
)

// HasSizeFilter reports whether the query restricts or sorts by size.
func (q *Query) HasSizeFilter() bool {
	return q.MinSize > 0 || q.MaxSize > 0 || q.SortBySize
}

// SizeMatches reports whether a file of the given size satisfies the size
// filters.
func (q *Query) SizeMatches(size int64) bool {
	return (q.MinSize <= 0 || size >= q.MinSize) && (q.MaxSize <= 0 || size <= q.MaxSize)
}

var takenPartRe = regexp.MustCompile(`^(\d{4})(?:-(\d{1,2})(?:-(\d{1,2}))?)?$`)

// ParseQuery parses a search query. It returns an error describing the problem
// if a filter is malformed.
func ParseQuery(q string) (Query, error) {
	var res Query
	var text []string
	for _, tok := range splitQuery(q) {
		key, value, found := strings.Cut(tok, ":")
		if !found {
			text = append(text, tok)
			continue
		}
		var err error
		switch strings.ToLower(key) {
		case "taken":
			res.Photo = true
			err = res.addTakenFilter(tok, strings.ToLower(value))
		case "is":
			err = res.addIsFilter(tok, strings.ToLower(value))
		case "larger", "smaller":
			err = res.addSizeFilter(tok, strings.ToLower(key), value)
		case "person", "who", "place", "where", "text", "says", "show", "pictured":
			err = res.addPhotoTerm(strings.ToLower(key), tok, value)
		case "sort":
			if !strings.EqualFold(value, "size") {
				err = fmt.Errorf("invalid sort %q, use sort:size", tok)
			}
			res.SortBySize = true
		default:
			text = append(text, tok)
		}
		if err != nil {
			return res, err
		}
	}
	res.Text = strings.Join(text, " ")
	res.filter.minSize = res.MinSize
	res.filter.maxSize = res.MaxSize
	res.filter.sortBySize = res.SortBySize
	return res, nil
}

// photoTermHints are what the photo terms expect, for error messages.
var photoTermHints = map[string]string{
	"person": "name", "who": "name", "place": "name", "where": "name",
	"text": "words", "says": "words", "show": "description", "pictured": "description",
}

// addPhotoTerm adds a "person:", "place:", "text:" or "show:" term, or one
// of their aliases.
func (q *Query) addPhotoTerm(key, tok, value string) error {
	value = strings.TrimSpace(value)
	if key == "text" || key == "says" {
		value = normalizeText(value)
	}
	if value == "" {
		return fmt.Errorf("invalid filter %q, use %s:%s", tok, key, photoTermHints[key])
	}
	q.Photo = true
	switch key {
	case "person", "who":
		q.PersonTerms = append(q.PersonTerms, value)
	case "place", "where":
		q.filter.placeTerms = append(q.filter.placeTerms, value)
	case "text", "says":
		q.filter.visibleTerms = append(q.filter.visibleTerms, value)
	default:
		q.Show = strings.TrimSpace(q.Show + " " + value)
	}
	return nil
}

// splitQuery splits a query on spaces, except inside double quotes, which
// are removed: `person:"Grandma Rose" beach` gives `person:Grandma Rose` and
// `beach`.
func splitQuery(q string) []string {
	var res []string
	var cur strings.Builder
	inQuotes := false
	flush := func() {
		if cur.Len() > 0 {
			res = append(res, cur.String())
			cur.Reset()
		}
	}
	for _, r := range q {
		switch {
		case r == '"':
			inQuotes = !inQuotes
		case !inQuotes && (r == ' ' || r == '\t' || r == '\n'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return res
}

func (q *Query) addIsFilter(tok, value string) error {
	switch value {
	case "duplicate", "duplicates", "dup":
		q.Duplicates = DupExact
	case "similar":
		q.Duplicates = DupSimilar
	default:
		return fmt.Errorf("invalid filter %q, use is:duplicate or is:similar", tok)
	}
	q.Photo = true
	return nil
}

var sizeRe = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([kmgt]?i?b?)$`)

func (q *Query) addSizeFilter(tok, key, value string) error {
	size, err := ParseSize(value)
	if err != nil {
		return fmt.Errorf("invalid size filter %q: %w", tok, err)
	}
	if key == "larger" {
		q.MinSize = max(q.MinSize, size)
	} else if q.MaxSize == 0 || size < q.MaxSize {
		q.MaxSize = size
	}
	return nil
}

// ParseSize parses a size such as "500", "100KB", "1.5GB" or "2GiB". Units are
// binary (1 KB = 1024 bytes), matching how the WebClient displays sizes.
func ParseSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return 0, errors.New("use a number with an optional unit: B, KB, MB, GB or TB")
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	mult := float64(1)
	if m[2] != "" {
		switch m[2][0] {
		case 'k':
			mult = 1 << 10
		case 'm':
			mult = 1 << 20
		case 'g':
			mult = 1 << 30
		case 't':
			mult = 1 << 40
		}
	}
	return int64(n * mult), nil
}

// addTakenFilter narrows the query with the value of a "taken:" token.
func (q *Query) addTakenFilter(tok, value string) error {
	switch value {
	case "any", "all", "*":
		return nil
	case "unknown", "none":
		q.filter.unknownOnly = true
		return nil
	}
	fromPart, toPart, isRange := strings.Cut(value, "..")
	if !isRange {
		toPart = fromPart
	}
	if fromPart == "" && toPart == "" {
		return fmt.Errorf("invalid date filter %q", tok)
	}
	if fromPart != "" {
		start, _, err := dateBounds(fromPart)
		if err != nil {
			return fmt.Errorf("invalid date filter %q: %w", tok, err)
		}
		if q.filter.from == "" || start > q.filter.from {
			q.filter.from = start
		}
	}
	if toPart != "" {
		_, end, err := dateBounds(toPart)
		if err != nil {
			return fmt.Errorf("invalid date filter %q: %w", tok, err)
		}
		if q.filter.to == "" || end < q.filter.to {
			q.filter.to = end
		}
	}
	return nil
}

// dateBounds returns the first instant of the given year, month or day and
// the first instant after it, both formatted with takenLayout.
func dateBounds(s string) (string, string, error) {
	m := takenPartRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", fmt.Errorf("use YYYY, YYYY-MM or YYYY-MM-DD")
	}
	year, _ := strconv.Atoi(m[1])
	month, day := 1, 1
	if m[2] != "" {
		month, _ = strconv.Atoi(m[2])
		if month < 1 || month > 12 {
			return "", "", fmt.Errorf("invalid month")
		}
	}
	if m[3] != "" {
		day, _ = strconv.Atoi(m[3])
		if day < 1 || day > 31 {
			return "", "", fmt.Errorf("invalid day")
		}
	}
	start := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if start.Day() != day {
		return "", "", fmt.Errorf("invalid day")
	}
	var end time.Time
	switch {
	case m[3] != "":
		end = start.AddDate(0, 0, 1)
	case m[2] != "":
		end = start.AddDate(0, 1, 0)
	default:
		end = start.AddDate(1, 0, 0)
	}
	return start.Format(takenLayout), end.Format(takenLayout), nil
}
