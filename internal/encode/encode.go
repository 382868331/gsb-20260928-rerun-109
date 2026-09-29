// Package encode produces two fMP4/HLS renditions of the source with shared,
// splittable time information. Both renditions are cut by one ffmpeg run so
// their boundaries coincide exactly.
package encode

import (
	"fmt"
	"path/filepath"

	"streamcore/internal/fftool"
)

const SegmentDuration = 2 // seconds; source is 8s -> 4 segments

// RenditionSpec describes one quality tier.
type RenditionSpec struct {
	Name      string
	Width     int
	Height    int
	VideoKbps int
}

// Renditions are the two content tiers. Lower index is the "low" tier.
var Renditions = []RenditionSpec{
	{Name: "low", Width: 160, Height: 120, VideoKbps: 80},
	{Name: "high", Width: 320, Height: 240, VideoKbps: 240},
}

// Rendition is one encoded tier output directory.
type Rendition struct {
	Spec     RenditionSpec
	Dir      string
	Playlist string
	Segments []string // .m4s segment file names in order
}

// All encodes the two renditions of sourceMP4 into outDir using fMP4
// fragmented segments. Output layout:
//
//	<outDir>/<tier>/init.mp4
//	<outDir>/<tier>/seg000.m4s ...
//	<outDir>/<tier>/index.m3u8
func All(sourceMP4, outDir string) ([]Rendition, error) {
	args := []string{
		"-y", "-i", sourceMP4,
		"-map", "0:v:0", "-map", "0:v:0",
		"-map", "0:a:0", "-map", "0:a:0",
	}
	rend := make([]Rendition, len(Renditions))
	for i, spec := range Renditions { // output video streams are v:0 and v:1
		rend[i] = Rendition{Spec: spec, Dir: filepath.Join(outDir, spec.Name), Playlist: filepath.Join(outDir, spec.Name, "index.m3u8")}
		args = append(args,
			fmt.Sprintf("-filter:v:%d", i),
			fmt.Sprintf("scale=%d:%d", spec.Width, spec.Height),
			fmt.Sprintf("-c:v:%d", i), "libx264",
			fmt.Sprintf("-b:v:%d", i), fmt.Sprintf("%dk", spec.VideoKbps),
			fmt.Sprintf("-pix_fmt:v:%d", i), "yuv420p",
			fmt.Sprintf("-g:v:%d", i), fmt.Sprintf("%d", segmentFrames()),
			fmt.Sprintf("-keyint_min:v:%d", i), fmt.Sprintf("%d", segmentFrames()),
			fmt.Sprintf("-sc_threshold:v:%d", i), "0",
			fmt.Sprintf("-bf:v:%d", i), "0", // no B-frame reorder across cuts
			fmt.Sprintf("-force_key_frames:v:%d", i), "expr:gte(t,n_forced*"+fmt.Sprintf("%d", SegmentDuration)+")",
		)
	}
	for i := range Renditions { // output audio streams are a:0 and a:1
		args = append(args,
			fmt.Sprintf("-c:a:%d", i), "aac",
			fmt.Sprintf("-b:a:%d", i), "64k",
			fmt.Sprintf("-ar:a:%d", i), "24000",
		)
	}
	args = append(args,
		"-f", "hls",
		"-hls_time", fmt.Sprintf("%d", SegmentDuration),
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_flags", "independent_segments",
		"-hls_segment_type", "fmp4",
		"-hls_segment_filename", filepath.Join(outDir, "%v", "seg%d.m4s"),
		"-var_stream_map", "v:0,a:0 v:1,a:1",
		filepath.Join(outDir, "%v", "index.m3u8"),
	)
	if _, err := fftool.Run("ffmpeg", args...); err != nil {
		return nil, err
	}
	if err := renameVariantDirs(outDir); err != nil {
		return nil, err
	}
	for i := range rend {
		dir := rend[i].Dir
		segs, err := ListSegments(dir)
		if err != nil {
			return nil, err
		}
		rend[i].Segments = segs
	}
	return rend, nil
}

func segmentFrames() int { return SegmentDuration * 10 }
