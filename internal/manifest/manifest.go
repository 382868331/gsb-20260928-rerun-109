// Package manifest defines the published segment index: renditions, segments
// with exact media timestamps, and splice boundaries where tier switches are
// continuous.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// SchemaVersion is the manifest format version.
const SchemaVersion = 1

// Segment is one completed, publishable media fragment.
type Segment struct {
	Index        int     `json:"index"`
	File         string  `json:"file"`
	StartPTS     float64 `json:"start_pts"`
	Duration     float64 `json:"duration"`
	VideoFrames  int     `json:"video_frames"`
	AudioSamples int     `json:"audio_samples"`
}

// Rendition is one quality tier in the index.
type Rendition struct {
	Name     string    `json:"name"`
	Width    int       `json:"width"`
	Height   int       `json:"height"`
	InitFile string    `json:"init_file"`
	Segments []Segment `json:"segments"`
}

// Manifest is one immutable publication version of the segment index.
type Manifest struct {
	Schema     int         `json:"schema"`
	Version    int         `json:"version"`
	Source     string      `json:"source"`
	CreatedAt  string      `json:"created_at"`
	SegmentDur float64     `json:"segment_duration"`
	Renditions []Rendition `json:"renditions"`
}

// Timeline validates that every rendition covers [0, end) with its segments
// and that boundaries match across tiers. It returns the segment count.
func (m *Manifest) Timeline() (int, error) {
	if len(m.Renditions) == 0 {
		return 0, fmt.Errorf("manifest has no renditions")
	}
	n := len(m.Renditions[0].Segments)
	if n == 0 {
		return 0, fmt.Errorf("rendition %q has no segments", m.Renditions[0].Name)
	}
	for _, r := range m.Renditions {
		if len(r.Segments) != n {
			return 0, fmt.Errorf("rendition %q has %d segments, want %d", r.Name, len(r.Segments), n)
		}
		var cursor float64
		for _, s := range r.Segments {
			if d := s.StartPTS - cursor; d > 1e-6 || d < -1e-6 {
				return 0, fmt.Errorf("rendition %q segment %d gap/overlap: cursor %.3f start %.3f", r.Name, s.Index, cursor, s.StartPTS)
			}
			cursor += s.Duration
		}
	}
	for i := 0; i < n; i++ {
		first := m.Renditions[0].Segments[i].StartPTS
		for _, r := range m.Renditions[1:] {
			if d := r.Segments[i].StartPTS - first; d > 1e-6 || d < -1e-6 {
				return 0, fmt.Errorf("segment %d starts differ across renditions", i)
			}
		}
	}
	return n, nil
}

// Boundaries returns the splice points (segment start PTS), including t=0.
func (m *Manifest) Boundaries() []float64 {
	if len(m.Renditions) == 0 {
		return nil
	}
	out := make([]float64, 0, len(m.Renditions[0].Segments))
	for _, s := range m.Renditions[0].Segments {
		out = append(out, s.StartPTS)
	}
	return out
}

// RenditionTier finds a tier by name.
func (m *Manifest) RenditionTier(name string) (*Rendition, error) {
	for i := range m.Renditions {
		if m.Renditions[i].Name == name {
			return &m.Renditions[i], nil
		}
	}
	return nil, fmt.Errorf("unknown rendition %q", name)
}

// SegmentAt returns the segment covering time t for a rendition.
func (r *Rendition) SegmentAt(t float64) (*Segment, error) {
	idx := sort.Search(len(r.Segments), func(i int) bool {
		return r.Segments[i].StartPTS+r.Segments[i].Duration > t+1e-9
	})
	if idx >= len(r.Segments) {
		return nil, fmt.Errorf("no segment at t=%.3f in rendition %q", t, r.Name)
	}
	return &r.Segments[idx], nil
}

// Save writes the manifest as indented JSON.
func Save(path string, m *Manifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// Load reads a manifest JSON file.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
