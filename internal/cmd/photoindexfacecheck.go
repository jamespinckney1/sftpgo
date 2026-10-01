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
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/drakkan/sftpgo/v2/internal/photoindex"
)

var (
	faceCheckURL      string
	faceCheckFile     string
	faceCheckModel    string
	faceCheckMinScore float64

	photoIndexFaceCheckCmd = &cobra.Command{
		Use:   "photoindex-facecheck",
		Short: "Check the face recognition service with one photo",
		Long: `Sends one JPEG photo to the face recognition service (the Immich
machine-learning container) and prints the faces found, to verify the setup
before enabling face recognition. The first run downloads the models and can
take a few minutes.

Usage example:

sftpgo photoindex-facecheck --ml-url http://immich-ml:3003 --file /srv/fileshare/photo.jpg`,
		Run: func(_ *cobra.Command, _ []string) {
			data, err := os.ReadFile(faceCheckFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "unable to read %q: %v\n", faceCheckFile, err)
				os.Exit(1)
			}
			start := time.Now()
			res, err := photoindex.CheckFaces(context.Background(), faceCheckURL, faceCheckModel, faceCheckMinScore, data)
			if err != nil {
				fmt.Fprintf(os.Stderr, "face check failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("OK in %s: image %dx%d, %d faces\n", time.Since(start).Round(time.Millisecond),
				res.Width, res.Height, len(res.Faces))
			for i, f := range res.Faces {
				fmt.Printf("  face %d: box (%.0f,%.0f)-(%.0f,%.0f), score %.2f, embedding %d values\n",
					i+1, f.X1, f.Y1, f.X2, f.Y2, f.Score, len(f.Embedding))
			}
		},
	}
)

func init() {
	photoIndexFaceCheckCmd.Flags().StringVar(&faceCheckURL, "ml-url", "http://immich-ml:3003", "URL of the face recognition service")
	photoIndexFaceCheckCmd.Flags().StringVar(&faceCheckFile, "file", "", "JPEG photo to analyze (required)")
	photoIndexFaceCheckCmd.Flags().StringVar(&faceCheckModel, "model", "buffalo_l", "Face model")
	photoIndexFaceCheckCmd.Flags().Float64Var(&faceCheckMinScore, "min-score", 0.7, "Minimum detection score")
	photoIndexFaceCheckCmd.MarkFlagRequired("file") //nolint:errcheck
	rootCmd.AddCommand(photoIndexFaceCheckCmd)
}
