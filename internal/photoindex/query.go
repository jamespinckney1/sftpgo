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
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Query is a parsed WebClient search query. Photo filters are written as
// "key:value" tokens; the remaining text is matched against file names.
//
// Supported filters:
//
//	taken:2023                  the whole year
//	taken:2023-06               a month
//	taken:2023-06-14            a day
//	taken:2023-06..2023-08      an inclusive range, either side may be omitted
//	taken:any                   every indexed photo/video, newest first
//	taken:unknown               files with no date in their metadata or name
type Query struct {
	// Text is the rest of the query, matched against file names.
	Text string
	// Photo is true if the query contains photo filters and must be answered
	// from the index.
	Photo  bool
	filter dateFilter
}

var takenPartRe = regexp.MustCompile(`^(\d{4})(?:-(\d{1,2})(?:-(\d{1,2}))?)?$`)

// ParseQuery parses a search query. It returns an error describing the problem
// if a filter is malformed.
func ParseQuery(q string) (Query, error) {
	var res Query
	var text []string
	for _, tok := range strings.Fields(q) {
		key, value, found := strings.Cut(tok, ":")
		if !found || !strings.EqualFold(key, "taken") {
			text = append(text, tok)
			continue
		}
		res.Photo = true
		if err := res.addTakenFilter(tok, strings.ToLower(value)); err != nil {
			return res, err
		}
	}
	res.Text = strings.Join(text, " ")
	return res, nil
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
