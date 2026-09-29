// Package publish builds a segment index from completed encoded renditions and
// publishes it as an immutable version. Unfinished/corrupt segments are
// rejected; a failed publish leaves the previous version untouched.
package publish

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"streamcore/internal/encode"
	"streamcore/internal/fftool"
	"streamcore/internal/manifest"
)

const maxDurationError = 0.12 // seconds tolerance for AAC priming/encoder lag

// Publisher manages a publication root:
//
//	<root>/media/<tier>/init.mp4,seg*.m4s
//	<root>/versions/manifest.vN.json
//	<root>/current.json -> (content copy of current version)
type Publisher struct {
	Root     string
	MediaDir string
	VerDir   string
}

// New prepares a publisher rooted at root.
func New(root string) *Publisher {
	return &Publisher{
		Root:     root,
		MediaDir: filepath.Join(root, "media"),
		VerDir:   filepath.Join(root, "versions"),
	}
}

// CurrentPath is the stable path clients read the active index from.
func (p *Publisher) CurrentPath() string { return filepath.Join(p.Root, "current.json") }

// VersionPath returns the immutable file for version v.
func (p *Publisher) VersionPath(v int) string {
	return filepath.Join(p.VerDir, fmt.Sprintf("manifest.v%d.json", v))
}

// Current returns the active manifest, or an error if none is published.
func (p *Publisher) Current() (*manifest.Manifest, error) {
	return manifest.Load(p.CurrentPath())
}

// Publish stages encoded renditions into the media directory, probes every
// segment to verify completion and timing, and only then atomically swaps in
// a new manifest version. Any failure aborts before current.json changes.
func (p *Publisher) Publish(renditions []encode.Rendition, sourceName string) (*manifest.Manifest, error) {
	stage := filepath.Join(p.Root, ".stage-"+time.Now().Format("20060102-150405.000000000"))
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return nil, err
	}
	abort := func() {
		// Staging is disposable; its failure does not affect live state.
		_ = os.RemoveAll(stage)
	}

	built, err := p.build(stage, renditions)
	if err != nil {
		abort()
		return nil, err
	}
	m := &manifest.Manifest{
		Schema:     manifest.SchemaVersion,
		Source:     sourceName,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		SegmentDur: float64(encode.SegmentDuration),
		Renditions: built,
	}
	if _, err := m.Timeline(); err != nil {
		abort()
		return nil, fmt.Errorf("timeline verification failed: %w", err)
	}

	next := 1
	if cur, err := p.Current(); err == nil {
		next = cur.Version + 1
	}
	m.Version = next

	// Commit media first (new versioned dirs), then the manifest, finally swap.
	commitMedia := filepath.Join(p.MediaDir, fmt.Sprintf("v%d", next))
	if err := os.MkdirAll(filepath.Dir(commitMedia), 0o755); err != nil {
		abort()
		return nil, err
	}
	if err := os.Rename(filepath.Join(stage, "media"), commitMedia); err != nil {
		abort()
		return nil, err
	}
	verFile := p.VersionPath(next)
	if err := manifest.Save(verFile, m); err != nil {
		abort()
		return nil, err
	}
	// Re-point file paths at the committed location, then write current atomically.
	for ri := range m.Renditions {
		m.Renditions[ri].InitFile = filepath.ToSlash(filepath.Join("media", fmt.Sprintf("v%d", next), m.Renditions[ri].Name, "init.mp4"))
		for si := range m.Renditions[ri].Segments {
			m.Renditions[ri].Segments[si].File = filepath.ToSlash(filepath.Join("media", fmt.Sprintf("v%d", next), m.Renditions[ri].Name, fmt.Sprintf("seg%d.m4s", si)))
		}
	}
	if err := manifest.Save(verFile, m); err != nil {
		abort()
		return nil, err
	}
	if err := atomicWriteJSON(p.CurrentPath(), m); err != nil {
		abort()
		return nil, err
	}
	_ = os.RemoveAll(stage)
	return m, nil
}

// build copies encoded output into stage/media/v and probes each segment.
// Manifest File fields temporarily point into stage; they are rewritten on
// commit. The important work here is decoding verification.
func (p *Publisher) build(stage string, renditions []encode.Rendition) ([]manifest.Rendition, error) {
	out := make([]manifest.Rendition, 0, len(renditions))
	wantSegs := -1
	var firstStarts [][]float64
	for _, ren := range renditions {
		if wantSegs == -1 {
			wantSegs = len(ren.Segments)
		} else if len(ren.Segments) != wantSegs {
			return nil, fmt.Errorf("rendition %q has %d completed segments, want %d", ren.Spec.Name, len(ren.Segments), wantSegs)
		}
		if wantSegs == 0 {
			return nil, fmt.Errorf("rendition %q has no completed segments", ren.Spec.Name)
		}
		stageTier := filepath.Join(stage, "media", ren.Spec.Name)
		if err := os.MkdirAll(stageTier, 0o755); err != nil {
			return nil, err
		}
		initSrc := filepath.Join(ren.Dir, "init.mp4")
		if err := copyFile(initSrc, filepath.Join(stageTier, "init.mp4")); err != nil {
			return nil, err
		}
		mr := manifest.Rendition{
			Name:     ren.Spec.Name,
			Width:    ren.Spec.Width,
			Height:   ren.Spec.Height,
			InitFile: filepath.ToSlash(filepath.Join("media", ren.Spec.Name, "init.mp4")),
		}
		var cursor float64
		var starts []float64
		for i, seg := range ren.Segments {
			info, err := inspect(initSrc, seg, i)
			if err != nil {
				return nil, fmt.Errorf("segment %s/%s rejected: %w", ren.Spec.Name, filepath.Base(seg), err)
			}
			name := fmt.Sprintf("seg%d.m4s", i)
			if err := copyFile(seg, filepath.Join(stageTier, name)); err != nil {
				return nil, err
			}
			mr.Segments = append(mr.Segments, manifest.Segment{
				Index:        i,
				File:         filepath.ToSlash(filepath.Join("media", ren.Spec.Name, name)),
				StartPTS:     cursor,
				Duration:     float64(encode.SegmentDuration),
				VideoFrames:  info.VideoFrames,
				AudioSamples: info.AudioSamples,
			})
			starts = append(starts, info.VideoStart)
			cursor += float64(encode.SegmentDuration)
		}
		firstStarts = append(firstStarts, starts)
		out = append(out, mr)
	}
	for segI := 0; segI < wantSegs; segI++ {
		ref := firstStarts[0][segI]
		for ri := 1; ri < len(firstStarts); ri++ {
			if d := firstStarts[ri][segI] - ref; math.Abs(d) > 1e-3 {
				return nil, fmt.Errorf("segment %d first-frame PTS differs across tiers: %.6f vs %.6f", segI, ref, firstStarts[ri][segI])
			}
		}
	}
	return out, nil
}

type segInfo struct {
	VideoStart   float64
	VideoEnd     float64
	VideoFrames  int
	AudioSamples int
}

// inspect decodes/probes an init+segment pair. Both streams must be present
// and complete: ffprobe reads the whole fragment and reports frames/samples.
func inspect(initFile, segFile string, expectIndex int) (segInfo, error) {
	tmp, err := os.CreateTemp("", "probe-seg-*.mp4")
	if err != nil {
		return segInfo{}, err
	}
	defer os.Remove(tmp.Name())
	if err := concatInto(tmp, initFile, segFile); err != nil {
		return segInfo{}, err
	}
	tmp.Close()
	_, frames, err := fftool.Probe(tmp.Name())
	if err != nil {
		return segInfo{}, err
	}
	var info segInfo
	haveV, haveA := false, false
	var vFirst, vLast float64
	var vFrames int
	var aFirst, aLast float64
	aSeen := false
	for _, f := range frames {
		switch f.MediaType {
		case "video":
			if vFrames == 0 {
				vFirst = atof(f.PTSTime)
			}
			vLast = atof(f.PTSTime) + atof(f.PktDurationTime)
			vFrames++
			haveV = true
		case "audio":
			start := atof(f.PTSTime)
			end := start + atof(f.PktDurationTime)
			if !aSeen {
				aFirst = start
				aSeen = true
			}
			if end > aLast {
				aLast = end
			}
			info.AudioSamples += f.NBSamples
			haveA = true
		}
	}
	if !haveV || !haveA {
		return segInfo{}, fmt.Errorf("missing stream (video=%v audio=%v)", haveV, haveA)
	}
	wantFrames := encode.SegmentDuration * 10
	if vFrames != wantFrames {
		return segInfo{}, fmt.Errorf("unfinished fragment: %d video frames, want %d", vFrames, wantFrames)
	}
	// 20 frames on the 10fps grid span 1.9s from first to last PTS; the last
	// frame's display end reaches the next 2s boundary.
	if d := (vLast - vFirst) - float64(wantFrames-1)*0.1; math.Abs(d) > maxDurationError {
		return segInfo{}, fmt.Errorf("video frame span %.3f does not cover %d frames (d=%.3f, unfinished fragment)", vLast-vFirst, wantFrames, d)
	}
	firstOffset := vFirst - float64(expectIndex*encode.SegmentDuration)
	endOffset := vLast + 0.1 - float64((expectIndex+1)*encode.SegmentDuration)
	if math.Abs(endOffset-firstOffset) > maxDurationError {
		return segInfo{}, fmt.Errorf("video window end offset %.3f != start offset %.3f (boundary mismatch)", endOffset, firstOffset)
	}
	// Audio packets must reach the segment end boundary (AAC priming may make
	// the first packet start slightly early; that is shared by both tiers).
	wantEnd := float64((expectIndex + 1) * encode.SegmentDuration)
	if aLast < wantEnd-maxDurationError {
		return segInfo{}, fmt.Errorf("audio ends at %.3f before boundary %.3f (unfinished fragment)", aLast, wantEnd)
	}
	if aFirst > float64(expectIndex*encode.SegmentDuration)+maxDurationError {
		return segInfo{}, fmt.Errorf("audio starts at %.3f after boundary %d (gap)", aFirst, expectIndex*encode.SegmentDuration)
	}
	info.VideoFrames = vFrames
	info.VideoStart = vFirst
	info.VideoEnd = vLast
	return info, nil
}

func concatInto(out *os.File, parts ...string) error {
	for _, p := range parts {
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func atof(s string) float64 {
	var pts, dur float64
	_ = dur
	fmt.Sscanf(s, "%f", &pts)
	return pts
}
