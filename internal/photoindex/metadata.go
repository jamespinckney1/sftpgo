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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Media kinds stored in the index.
const (
	KindImage = "image"
	KindRaw   = "raw"
	KindVideo = "video"
)

// Sources of the "taken" date, from the most to the least reliable.
const (
	TakenSrcExif     = "exif"
	TakenSrcVideo    = "video"
	TakenSrcXMP      = "xmp"
	TakenSrcFilename = "filename"
	TakenSrcModTime  = "mtime"
)

// takenLayout is the layout used to store the date taken. It is the local wall
// clock time at which the photo was taken, as the camera recorded it, so
// "taken:2023-06" means June 2023 wherever the photo was taken. Stored as text
// it sorts and compares correctly.
const takenLayout = "2006-01-02 15:04:05"

var mediaKinds = map[string]string{
	".jpg": KindImage, ".jpeg": KindImage, ".jpe": KindImage, ".png": KindImage,
	".gif": KindImage, ".webp": KindImage, ".bmp": KindImage, ".tif": KindImage,
	".tiff": KindImage, ".heic": KindImage, ".heif": KindImage, ".hif": KindImage,
	".avif": KindImage,
	".dng":  KindRaw, ".cr2": KindRaw, ".cr3": KindRaw, ".nef": KindRaw, ".nrw": KindRaw,
	".arw": KindRaw, ".orf": KindRaw, ".rw2": KindRaw, ".raf": KindRaw, ".pef": KindRaw,
	".srw": KindRaw,
	".mp4": KindVideo, ".mov": KindVideo, ".m4v": KindVideo, ".mkv": KindVideo,
	".webm": KindVideo, ".avi": KindVideo, ".3gp": KindVideo, ".mts": KindVideo,
	".m2ts": KindVideo,
}

// KindForName returns the media kind for the given file name, or an empty
// string if the file is not indexed.
func KindForName(name string) string {
	return mediaKinds[strings.ToLower(filepath.Ext(name))]
}

// NeedsPreviewForBrowser reports whether browsers generally cannot display the
// given file, so the WebClient should show the generated preview instead.
func NeedsPreviewForBrowser(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".heic", ".heif", ".hif", ".tif", ".tiff":
		return true
	}
	return KindForName(name) == KindRaw
}

// exifDateRe matches the date formats used by EXIF, XMP and QuickTime tags,
// for example "2023:06:14 10:22:33", "2023:06:14 10:22:33.123+02:00" or
// "2023-06-14T10:22:33Z". The time and the zone are optional.
var exifDateRe = regexp.MustCompile(`^(\d{4})[:-](\d{2})[:-](\d{2})(?:[ T](\d{2}):(\d{2})(?::(\d{2}))?(?:\.\d+)?\s*(Z|[+-]\d{2}:?\d{2})?)?$`)

// parsedDate is a wall clock time plus, when known, its UTC offset.
type parsedDate struct {
	wall   time.Time // the wall clock time, stored in a UTC time.Time
	offset string    // "+02:00", "Z" or "" if unknown
}

func (d parsedDate) String() string {
	return d.wall.Format(takenLayout)
}

// parseExifDate parses a metadata date. It returns false for empty, zeroed or
// implausible dates (cameras with an unset clock write "0000:00:00 00:00:00"
// or dates in 1970/1980).
func parseExifDate(s string) (parsedDate, bool) {
	s = strings.TrimSpace(s)
	m := exifDateRe.FindStringSubmatch(s)
	if m == nil {
		return parsedDate{}, false
	}
	n := func(v string) int {
		i, _ := strconv.Atoi(v)
		return i
	}
	year, month, day := n(m[1]), n(m[2]), n(m[3])
	hour, minute, sec := n(m[4]), n(m[5]), n(m[6])
	if !plausibleDate(year, month, day) || hour > 23 || minute > 59 || sec > 60 {
		return parsedDate{}, false
	}
	wall := time.Date(year, time.Month(month), day, hour, minute, sec, 0, time.UTC)
	offset := m[7]
	if offset != "" && offset != "Z" && !strings.Contains(offset, ":") {
		offset = offset[:3] + ":" + offset[3:]
	}
	return parsedDate{wall: wall, offset: offset}, true
}

func plausibleDate(year, month, day int) bool {
	if year < 1900 || year > time.Now().Year()+1 || month < 1 || month > 12 || day < 1 || day > 31 {
		return false
	}
	// Unset camera clocks commonly default to the epoch.
	if (year == 1970 || year == 1980) && month == 1 && day == 1 {
		return false
	}
	return true
}

// utcToLocalWall converts a UTC date, such as the QuickTime CreateDate that
// the specification defines as UTC, into the server's local wall clock time.
func utcToLocalWall(d parsedDate) parsedDate {
	local := d.wall.In(time.Local)
	_, off := local.Zone()
	wall := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(),
		local.Second(), 0, time.UTC)
	return parsedDate{wall: wall, offset: formatOffset(off)}
}

func formatOffset(seconds int) string {
	sign := "+"
	if seconds < 0 {
		sign = "-"
		seconds = -seconds
	}
	return sign + twoDigits(seconds/3600) + ":" + twoDigits((seconds%3600)/60)
}

func twoDigits(v int) string {
	if v < 10 {
		return "0" + strconv.Itoa(v)
	}
	return strconv.Itoa(v)
}

// filenameDateRe matches dates embedded in file names by phones and cameras,
// for example IMG_20230614_102233.jpg, PXL_20230614_102233123.jpg,
// "2023-06-14 10.22.33.jpg", Screenshot_2023-06-14-10-22-33.png or
// IMG-20230614-WA0001.jpg (date only).
var filenameDateRe = regexp.MustCompile(`(?:^|[^0-9])((?:19|20)\d{2})[-_.]?(0[1-9]|1[0-2])[-_.]?(0[1-9]|[12]\d|3[01])(?:[-_. T]?([01]\d|2[0-3])[-_.:h]?([0-5]\d)[-_.:m]?([0-5]\d)\d{0,3})?(?:[^0-9]|$)`)

func dateFromFilename(name string) (parsedDate, bool) {
	m := filenameDateRe.FindStringSubmatch(filepath.Base(name))
	if m == nil {
		return parsedDate{}, false
	}
	n := func(v string) int {
		i, _ := strconv.Atoi(v)
		return i
	}
	year, month, day := n(m[1]), n(m[2]), n(m[3])
	if !plausibleDate(year, month, day) {
		return parsedDate{}, false
	}
	wall := time.Date(year, time.Month(month), day, n(m[4]), n(m[5]), n(m[6]), 0, time.UTC)
	return parsedDate{wall: wall}, true
}

// metadata is the subset of the file metadata stored in the index.
type metadata struct {
	taken    parsedDate
	takenSrc string
	width    int
	height   int
	lat      *float64
	lon      *float64
	camera   string
}

// exifFields are the keys, as reported by "exiftool -j -n -G0", that we read.
type exifFields map[string]any

func (f exifFields) str(key string) string {
	v, ok := f[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

func (f exifFields) num(key string) (float64, bool) {
	v, ok := f[key]
	if !ok || v == nil {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return n, err == nil
	}
	return 0, false
}

// exiftoolTags are the tags requested from exiftool.
var exiftoolTags = []string{
	"-DateTimeOriginal", "-CreateDate", "-OffsetTimeOriginal", "-OffsetTime",
	"-CreationDate", "-DateCreated", "-CreationTime", "-GPSLatitude", "-GPSLongitude",
	"-ImageWidth", "-ImageHeight", "-Orientation", "-Make", "-Model",
}

// dateCandidate is a metadata tag that may hold the date taken.
type dateCandidate struct {
	key       string
	src       string
	offsetKey string // tag holding the UTC offset, if the date has none
	utc       bool   // the tag is UTC by specification
}

// dateCandidates are the date tags, from the most to the least reliable.
var dateCandidates = []dateCandidate{
	// The moment the shutter was pressed.
	{key: "EXIF:DateTimeOriginal", src: TakenSrcExif, offsetKey: "EXIF:OffsetTimeOriginal"},
	// Apple's QuickTime "Keys" CreationDate is local time with offset.
	{key: "QuickTime:CreationDate", src: TakenSrcVideo},
	// Written by some editors.
	{key: "XMP:DateTimeOriginal", src: TakenSrcXMP},
	// Digitized date.
	{key: "EXIF:CreateDate", src: TakenSrcExif, offsetKey: "EXIF:OffsetTime"},
	{key: "QuickTime:CreateDate", src: TakenSrcVideo, utc: true},
	{key: "XMP:DateCreated", src: TakenSrcXMP},
	{key: "XMP:CreateDate", src: TakenSrcXMP},
	{key: "PNG:CreationTime", src: TakenSrcXMP},
}

func dateFromFields(fields exifFields) (parsedDate, string, bool) {
	for _, c := range dateCandidates {
		d, ok := parseExifDate(fields.str(c.key))
		if !ok {
			continue
		}
		if c.utc && (d.offset == "" || d.offset == "Z") {
			d = utcToLocalWall(d)
		}
		if d.offset == "" && c.offsetKey != "" {
			d.offset = normalizeOffset(fields.str(c.offsetKey))
		}
		return d, c.src, true
	}
	return parsedDate{}, "", false
}

func gpsFromFields(fields exifFields) (*float64, *float64) {
	lat, ok := fields.num("Composite:GPSLatitude")
	if !ok {
		return nil, nil
	}
	lon, ok := fields.num("Composite:GPSLongitude")
	if !ok || (lat == 0 && lon == 0) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return nil, nil
	}
	return &lat, &lon
}

// resolveMetadata derives the stored metadata from the exiftool output (which
// may be nil if exiftool is unavailable or failed), the file name and the file
// modification time, picking the most reliable available date.
func resolveMetadata(fields exifFields, name string, modTime time.Time) metadata {
	var md metadata
	var ok bool
	if fields != nil {
		md.taken, md.takenSrc, _ = dateFromFields(fields)
		md.width, md.height = exifDimensions(fields)
		md.lat, md.lon = gpsFromFields(fields)
		md.camera = cameraName(fields.str("EXIF:Make"), fields.str("EXIF:Model"))
	}
	if md.takenSrc == "" {
		if md.taken, ok = dateFromFilename(name); ok {
			md.takenSrc = TakenSrcFilename
		}
	}
	if md.takenSrc == "" {
		md.taken = utcToLocalWall(parsedDate{wall: modTime.UTC()})
		md.takenSrc = TakenSrcModTime
	}
	return md
}

func normalizeOffset(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 5 && (s[0] == '+' || s[0] == '-') {
		return s[:3] + ":" + s[3:]
	}
	if len(s) == 6 && (s[0] == '+' || s[0] == '-') && s[3] == ':' {
		return s
	}
	return ""
}

func exifDimensions(fields exifFields) (int, int) {
	var w, h float64
	for _, group := range []string{"File", "EXIF", "PNG", "QuickTime", "RIFF", "Matroska"} {
		if v, ok := fields.num(group + ":ImageWidth"); ok && v > 0 {
			w = v
			if v, ok := fields.num(group + ":ImageHeight"); ok && v > 0 {
				h = v
			}
			break
		}
	}
	// EXIF orientations 5-8 are rotated by 90 degrees.
	if o, ok := fields.num("EXIF:Orientation"); ok && o >= 5 && o <= 8 {
		w, h = h, w
	}
	return int(w), int(h)
}

func cameraName(make, model string) string {
	make = strings.TrimSpace(make)
	model = strings.TrimSpace(model)
	if make == "" {
		return model
	}
	// Many models already start with the make, e.g. "Canon EOS 80D".
	if strings.HasPrefix(strings.ToLower(model), strings.ToLower(strings.Fields(make)[0])) {
		return model
	}
	if model == "" {
		return make
	}
	return make + " " + model
}
