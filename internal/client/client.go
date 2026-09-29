// Package client is the local playback client. It reads segments in PTS order
// and only switches quality tiers at spliceable boundaries, so audio and video
// are neither repeated nor skipped across a switch.
package client

import (
	"fmt"
	"math"

	"streamcore/internal/manifest"
)

const boundaryEps = 1e-6

// PlannedSegment is one read in a continuous playback plan.
type PlannedSegment struct {
	Rendition string
	manifest.Segment
}

// Client holds playback position against one published version.
type Client struct {
	manifest *manifest.Manifest
	tier     string
	segIndex int
}

// New starts a client on the named rendition at t=0.
func New(m *manifest.Manifest, tier string) (*Client, error) {
	if _, err := m.RenditionTier(tier); err != nil {
		return nil, err
	}
	return &Client{manifest: m, tier: tier}, nil
}

// CurrentTier returns the active tier name.
func (c *Client) CurrentTier() string { return c.tier }

// Position returns the PTS at which the next read begins.
func (c *Client) Position() float64 {
	r := c.manifest.Renditions[0]
	if c.segIndex >= len(r.Segments) {
		last := r.Segments[len(r.Segments)-1]
		return last.StartPTS + last.Duration
	}
	return r.Segments[c.segIndex].StartPTS
}

// Switch changes tier at PTS at. at must be a shared segment boundary; a
// mid-segment switch is rejected because it would duplicate or drop media.
func (c *Client) Switch(tier string, at float64) error {
	if tier == c.tier {
		return nil
	}
	if _, err := c.manifest.RenditionTier(tier); err != nil {
		return err
	}
	if !c.isBoundary(at) {
		return fmt.Errorf("switch at %.3fs is not a spliceable boundary %v", at, c.manifest.Boundaries())
	}
	newTier, err := c.manifest.RenditionTier(tier)
	if err != nil {
		return err
	}
	seg, err := newTier.SegmentAt(at)
	if err != nil {
		return err
	}
	if math.Abs(seg.StartPTS-at) > boundaryEps {
		return fmt.Errorf("rendition %q has no segment start at %.3f", tier, at)
	}
	c.tier = tier
	c.segIndex = seg.Index
	return nil
}

func (c *Client) isBoundary(t float64) bool {
	for _, b := range c.manifest.Boundaries() {
		if math.Abs(b-t) < boundaryEps {
			return true
		}
	}
	return false
}

// Read returns the next segment in timeline order and advances the position.
func (c *Client) Read() (PlannedSegment, error) {
	r, err := c.manifest.RenditionTier(c.tier)
	if err != nil {
		return PlannedSegment{}, err
	}
	if c.segIndex >= len(r.Segments) {
		return PlannedSegment{}, fmt.Errorf("end of stream")
	}
	ps := PlannedSegment{Rendition: r.Name, Segment: r.Segments[c.segIndex]}
	c.segIndex++
	return ps, nil
}

// PlaybackPlan reads from t=0 to the end, applying switches at the given
// boundary points ("tier@boundaryPTS" pairs, chronologically).
type Switch struct {
	Tier string
	At   float64
}

// Plan builds the full continuous read sequence with the requested switches.
func Plan(m *manifest.Manifest, startTier string, switches []Switch) ([]PlannedSegment, error) {
	c, err := New(m, startTier)
	if err != nil {
		return nil, err
	}
	var plan []PlannedSegment
	si := 0
	for {
		for si < len(switches) && math.Abs(c.Position()-switches[si].At) < boundaryEps {
			if err := c.Switch(switches[si].Tier, switches[si].At); err != nil {
				return nil, err
			}
			si++
		}
		ps, err := c.Read()
		if err != nil {
			break
		}
		plan = append(plan, ps)
	}
	if si != len(switches) {
		return nil, fmt.Errorf("switch at %.3fs is not a boundary reached during playback", switches[si].At)
	}
	if err := ValidatePlan(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// ValidatePlan checks the read sequence covers the timeline once: contiguous,
// no repeated PTS and no gap.
func ValidatePlan(plan []PlannedSegment) error {
	if len(plan) == 0 {
		return fmt.Errorf("empty playback plan")
	}
	var cursor float64
	seen := map[string]int{}
	for i, ps := range plan {
		if math.Abs(ps.StartPTS-cursor) > boundaryEps {
			return fmt.Errorf("read #%d starts at %.3f, expected %.3f (gap or repeat)", i, ps.StartPTS, cursor)
		}
		key := fmt.Sprintf("%s@%.3f", ps.Rendition, ps.StartPTS)
		if seen[key] > 0 {
			return fmt.Errorf("segment %s read twice", key)
		}
		seen[key]++
		cursor = ps.StartPTS + ps.Duration
	}
	return nil
}
