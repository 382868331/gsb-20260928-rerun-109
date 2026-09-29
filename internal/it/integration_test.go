package streamcore_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"streamcore/internal/client"
	"streamcore/internal/encode"
	"streamcore/internal/manifest"
	"streamcore/internal/publish"
	"streamcore/internal/source"
	"streamcore/internal/verify"
)

func ffmpegAvailable() bool {
	_, err1 := exec.LookPath("ffmpeg")
	_, err2 := exec.LookPath("ffprobe")
	return err1 == nil && err2 == nil
}

// TestFullPipeline is the real end-to-end check: bundled source, two tiers,
// versioned publish, boundary switches, and actual decode verification.
func TestFullPipeline(t *testing.T) {
	if !ffmpegAvailable() {
		t.Skip("ffmpeg/ffprobe not available")
	}
	wd := t.TempDir()
	src, err := source.Generate(filepath.Join(wd, "source"))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if src.Frames != 80 || src.Samples != 24000*8 {
		t.Fatalf("unexpected source size %d frames %d samples", src.Frames, src.Samples)
	}
	trend, err := encode.All(src.MuxedMP4, filepath.Join(wd, "encoded"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, r := range trend {
		if len(r.Segments) != 4 {
			t.Fatalf("rendition %s has %d segments, want 4", r.Spec.Name, len(r.Segments))
		}
	}
	pub := publish.New(filepath.Join(wd, "publication"))
	m1, err := pub.Publish(trend, "source.mp4")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if m1.Version != 1 {
		t.Fatalf("first version = %d, want 1", m1.Version)
	}

	// Tier switching at every boundary with actual decode verification.
	plan, err := client.Plan(m1, "low", []client.Switch{
		{Tier: "high", At: 2},
		{Tier: "low", At: 4},
		{Tier: "high", At: 6},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	res, err := verify.Plan(pub.Root, m1, plan)
	if err != nil {
		t.Fatalf("decode verification: %v", err)
	}
	if res.DecodedVideoFrames != 80 {
		t.Errorf("decoded %d video frames, want 80 (no repeat/gap)", res.DecodedVideoFrames)
	}
	if res.DecodedAudioSamples != 192000 {
		t.Errorf("decoded %d audio samples, want 192000", res.DecodedAudioSamples)
	}
}

// TestUnfinishedSegmentRejectedAndVersionKept corrupts one fragment (a
// truncated, unfinished segment). Publish must fail and current.json must
// still describe the previous complete version.
func TestUnfinishedSegmentRejectedAndVersionKept(t *testing.T) {
	if !ffmpegAvailable() {
		t.Skip("ffmpeg/ffprobe not available")
	}
	wd := t.TempDir()
	src, err := source.Generate(filepath.Join(wd, "source"))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	trend, err := encode.All(src.MuxedMP4, filepath.Join(wd, "encoded"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	pub := publish.New(filepath.Join(wd, "publication"))
	if _, err := pub.Publish(trend, "source.mp4"); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	// Truncate the last segment of the low tier to simulate an unfinished
	// fragment, then attempt a second publish.
	bad := trend[0].Segments[3]
	data, err := os.ReadFile(bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, data[:len(data)/3], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := pub.Publish(trend, "source.mp4"); err == nil ||
		!strings.Contains(err.Error(), "unfinished fragment") {
		t.Fatalf("want unfinished fragment error, got %v", err)
	}
	cur, err := pub.Current()
	if err != nil {
		t.Fatalf("current manifest lost: %v", err)
	}
	if cur.Version != 1 {
		t.Fatalf("current version = %d, want 1 preserved after failed publish", cur.Version)
	}
	if n, err := cur.Timeline(); err != nil || n != 4 {
		t.Fatalf("preserved version timeline invalid: n=%d err=%v", n, err)
	}
}

var _ = manifest.SchemaVersion
