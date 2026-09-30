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

package cmd

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/drakkan/sftpgo/v2/internal/photoindex"
)

var (
	photoBenchDir         string
	photoBenchSamples     int
	photoBenchPreviewSize int
	photoBenchExiftool    string
	photoBenchVips        string

	photoIndexBenchCmd = &cobra.Command{
		Use:   "photoindex-bench",
		Short: "Measure how long the photo index takes per file on this machine",
		Long: `Runs the photo index pipeline (date/metadata extraction and preview
generation) on a sample of the photos and videos found under a directory and
prints the time per file by format, plus an estimate for a first full pass.
Nothing is written to the index and the source files are only read.

Usage example:

sftpgo photoindex-bench --dir /srv/sftpgo/data --samples 100`,
		Run: func(_ *cobra.Command, _ []string) {
			err := photoindex.Benchmark(photoindex.BenchmarkOptions{
				Dir:          photoBenchDir,
				Samples:      photoBenchSamples,
				PreviewSize:  photoBenchPreviewSize,
				ExiftoolPath: photoBenchExiftool,
				VipsPath:     photoBenchVips,
			}, os.Stdout)
			if err != nil {
				os.Stderr.WriteString("benchmark failed: " + err.Error() + "\n") //nolint:errcheck
				os.Exit(1)
			}
		},
	}
)

func init() {
	photoIndexBenchCmd.Flags().StringVar(&photoBenchDir, "dir", "", "Directory containing photos (required)")
	photoIndexBenchCmd.Flags().IntVar(&photoBenchSamples, "samples", 50, "Number of files to process")
	photoIndexBenchCmd.Flags().IntVar(&photoBenchPreviewSize, "preview-size", 1024, "Preview size in pixels")
	photoIndexBenchCmd.Flags().StringVar(&photoBenchExiftool, "exiftool-path", "", "Path to exiftool, looked up in PATH if empty")
	photoIndexBenchCmd.Flags().StringVar(&photoBenchVips, "vips-path", "", "Path to vipsthumbnail, looked up in PATH if empty")
	photoIndexBenchCmd.MarkFlagRequired("dir") //nolint:errcheck
	rootCmd.AddCommand(photoIndexBenchCmd)
}
