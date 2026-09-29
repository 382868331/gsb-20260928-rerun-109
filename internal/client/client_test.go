package client

import (
	"strings"
	"testing"

	"streamcore/internal/manifest"
)

func testManifest() *manifest.Manifest {
	ren := func(name string) manifest.Rendition {
		var segs []manifest.Segment
		for i := 0; i < 4; i++ {
			segs = append(segs, manifest.Segment{Index: i, StartPTS: float64(i * 2), Duration: 2})
		}
		return manifest.Rendition{Name: name, Segments: segs}
	}
	return &manifest.Manifest{Renditions: []manifest.Rendition{ren("low"), ren("high")}}
}

func TestBoundarySwitchIsContinuous(t *testing.T) {
	m := testManifest()
	plan, err := Plan(m, "low", []Switch{
		{Tier: "high", At: 2},
		{Tier: "low", At: 4},
		{Tier: "high", At: 6},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	wantTier := []string{"low", "high", "low", "high"}
	var cursor float64
	for i, ps := range plan {
		if ps.Rendition != wantTier[i] {
			t.Errorf("read %d tier = %s, want %s", i, ps.Rendition, wantTier[i])
		}
		if ps.StartPTS != cursor {
			t.Errorf("read %d starts %.1f, want %.1f", i, ps.StartPTS, cursor)
		}
		cursor += ps.Duration
	}
}

func TestMidSegmentSwitchRejected(t *testing.T) {
	m := testManifest()
	c, err := New(m, "low")
	if err != nil {
		t.Fatal(err)
	}
	err = c.Switch("high", 3.5) // mid-segment would duplicate/drop media
	if err == nil || !strings.Contains(err.Error(), "not a spliceable boundary") {
		t.Fatalf("want boundary error, got %v", err)
	}
	if c.CurrentTier() != "low" {
		t.Fatalf("tier changed after rejected switch: %s", c.CurrentTier())
	}
}

func TestPlanRejectsRepeatedPTS(t *testing.T) {
	m := testManifest()
	if _, err := Plan(m, "low", []Switch{{Tier: "high", At: 1}}); err == nil {
		t.Fatal("switch at t=1 must be rejected")
	}
}
