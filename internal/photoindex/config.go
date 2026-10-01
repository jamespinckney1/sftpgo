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

// Package photoindex builds and queries a background index of the photos and
// videos stored on local filesystems (fork feature, see FORK_FEATURES.md).
//
// The index is keyed by the real filesystem path of each file, not by any
// user's virtual path, so a photo inside a folder shared by several users is
// processed only once. Access control is applied at query time by the caller,
// using the same per-user permission checks as a directory listing.
package photoindex

import (
	"path/filepath"
)

const logSender = "photoindex"

// Config defines the configuration for the photo index. All keys are optional;
// the index is disabled by default.
type Config struct {
	// Enabled allows to enable the photo index. Default false.
	Enabled bool `json:"enabled" mapstructure:"enabled"`
	// DataDir is where the index database and the photo previews are stored.
	// It can be an absolute path or a path relative to the config dir. Put it
	// on fast storage (an SSD), the index is read on every photo search.
	// Default "photoindex".
	DataDir string `json:"data_dir" mapstructure:"data_dir"`
	// PreviewSize is the size, in pixels, of the longest edge of the JPEG
	// preview generated for each photo. Previews are used for HEIC/RAW
	// thumbnails and for the viewer, and they will feed the later ML stages.
	// 0 disables preview generation. Default 1024.
	PreviewSize int `json:"preview_size" mapstructure:"preview_size"`
	// RescanInterval is the interval, in hours, between full rescans of all
	// the indexed folders. Uploads, renames and deletes done through SFTPGo are
	// picked up immediately; the rescan finds changes made outside of SFTPGo.
	// 0 means scan only at startup. Default 24.
	RescanInterval int `json:"rescan_interval" mapstructure:"rescan_interval"`
	// StartupDelay is the delay, in seconds, before the first scan after
	// startup. Default 60.
	StartupDelay int `json:"startup_delay" mapstructure:"startup_delay"`
	// Workers is the number of files processed in parallel. On a small board
	// such as a Raspberry Pi 1 keeps the system responsive. Default 1.
	Workers int `json:"workers" mapstructure:"workers"`
	// PauseBetweenFiles is an optional pause, in milliseconds, after each
	// processed file to further reduce the load. Default 0.
	PauseBetweenFiles int `json:"pause_between_files" mapstructure:"pause_between_files"`
	// MaxSourceSize is the maximum size, in MB, of a photo for which a preview
	// is generated. Metadata is still extracted for larger files. Default 200.
	MaxSourceSize int `json:"max_source_size" mapstructure:"max_source_size"`
	// ExiftoolPath is the path to the exiftool binary, used to read the date
	// taken and other metadata. If empty it is looked up in the PATH; if it is
	// not found dates are derived from file names and modification times only.
	ExiftoolPath string `json:"exiftool_path" mapstructure:"exiftool_path"`
	// VipsPath is the path to the vipsthumbnail binary (from libvips), used to
	// generate previews. If empty it is looked up in the PATH; if it is not
	// found no previews are generated.
	VipsPath string `json:"vips_path" mapstructure:"vips_path"`
	// MLURL is the URL of the face recognition service, the Immich
	// machine-learning container, e.g. "http://immich-ml:3003". Empty
	// disables face recognition.
	MLURL string `json:"ml_url" mapstructure:"ml_url"`
	// FaceModel is the face model the service uses. Default "buffalo_l", the
	// most accurate; "buffalo_s" is faster and lighter.
	FaceModel string `json:"face_model" mapstructure:"face_model"`
	// FaceMinScore is the minimum detection confidence, 0..1, for a face to be
	// kept. Default 0.7.
	FaceMinScore float64 `json:"face_min_score" mapstructure:"face_min_score"`
	// FaceMatchThreshold is the minimum similarity, 0..1, between a face and
	// a person for the face to be added to that person automatically. Higher
	// is stricter: fewer mistakes, more groups to merge by hand. Default 0.5.
	FaceMatchThreshold float64 `json:"face_match_threshold" mapstructure:"face_match_threshold"`
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{
		Enabled:            false,
		DataDir:            "photoindex",
		PreviewSize:        1024,
		RescanInterval:     24,
		StartupDelay:       60,
		Workers:            1,
		PauseBetweenFiles:  0,
		MaxSourceSize:      200,
		ExiftoolPath:       "",
		VipsPath:           "",
		MLURL:              "",
		FaceModel:          "buffalo_l",
		FaceMinScore:       0.7,
		FaceMatchThreshold: 0.5,
	}
}

func (c *Config) resolveDataDir(configDir string) string {
	dir := c.DataDir
	if dir == "" {
		dir = "photoindex"
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(configDir, dir)
	}
	return dir
}
