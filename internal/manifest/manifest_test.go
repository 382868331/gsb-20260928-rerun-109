package manifest

import "testing"

func TestTimelineDetectsGap(t *testing.T) {
	segs := []Segment{
		{Index: 0, StartPTS: 0, Duration: 2},
		{Index: 1, StartPTS: 4, Duration: 2}, // gap at 2..4
	}
	m := &Manifest{Renditions: []Rendition{{Name: "low", Segments: segs}}}
	if _, err := m.Timeline(); err == nil {
		t.Fatal("expected gap error")
	}
}

func TestTimelineDetectsTierMisalignment(t *testing.T) {
	seg := func(shift float64) []Segment {
		return []Segment{
			{Index: 0, StartPTS: shift, Duration: 2},
			{Index: 1, StartPTS: 2 + shift, Duration: 2},
		}
	}
	m := &Manifest{Renditions: []Rendition{
		{Name: "low", Segments: seg(0)},
		{Name: "high", Segments: seg(0.5)},
	}}
	if _, err := m.Timeline(); err == nil {
		t.Fatal("expected cross-tier misalignment error")
	}
}
