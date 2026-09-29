package stream_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"localstream/internal/indexer"
	"localstream/internal/media"
	"localstream/internal/player"
	"localstream/internal/store"
)

func TestPublishedSwitchIsGaplessAndContentMatchesVariant(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	publishID := "first"
	writeSource(t, sourceDir)
	index := scanAndPublish(t, root, sourceDir, publishID)

	p, _, err := player.Open(store.Open(root), "base")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for !p.AtEnd() {
		if p.Position() == 2*media.SegmentDuration {
			if err := p.Switch("sharp"); err != nil {
				t.Fatal(err)
			}
		}
		header, packets, err := p.ReadSegment()
		if err != nil {
			t.Fatal(err)
		}
		spec := variantSpec(t, p.Variant())
		for _, packet := range packets {
			key := fmt.Sprintf("%d:%d", packet.Type, packet.PTS)
			if previous, exists := seen[key]; exists {
				t.Fatalf("duplicated frame at pts %d from variants %s and %s", packet.PTS, previous, p.Variant())
			}
			seen[key] = p.Variant()
			switch packet.Type {
			case media.VideoPacket:
				assertVideo(t, spec, header, packet)
			case media.AudioPacket:
				assertAudio(t, spec, packet)
			}
		}
	}

	expectedFrames := media.SegmentCount * media.FramesPerSegment
	if len(seen) != expectedFrames*2 {
		t.Fatalf("got %d unique timestamped audio/video packets, want %d", len(seen), expectedFrames*2)
	}
	for frame := range expectedFrames {
		pts := uint64(frame) * media.FrameDuration
		if _, ok := seen[fmt.Sprintf("%d:%d", media.VideoPacket, pts)]; !ok {
			t.Fatalf("missing frame at pts %d", pts)
		}
		if _, ok := seen[fmt.Sprintf("%d:%d", media.AudioPacket, pts)]; !ok {
			t.Fatalf("missing audio at pts %d", pts)
		}
	}
	if position := p.Position(); position != index.Timeline {
		t.Fatalf("ended at %d, want %d", position, index.Timeline)
	}
}

func TestUnfinishedSegmentIsNotIndexed(t *testing.T) {
	root := t.TempDir()
	writeSource(t, root)
	if err := os.WriteFile(filepath.Join(root, "base", ".000006.sm.part"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	index, err := indexer.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Variants) != 2 {
		t.Fatalf("got %d variants, want 2", len(index.Variants))
	}
	for _, variant := range index.Variants {
		if len(variant.Segments) != media.SegmentCount {
			t.Fatalf("%s indexed %d complete segments, want %d", variant.Name, len(variant.Segments), media.SegmentCount)
		}
	}
}

func TestCanceledPublicationKeepsPreviousVersion(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	writeSource(t, first)
	writeSource(t, second)
	writeSource(t, first)
	scanAndPublish(t, root, first, "release")

	secondIndex := scan(t, second)
	variants := publishedVariants(t, secondIndex)
	s := store.Open(root)
	if _, err := s.Prepare("release-next", variants, copyFrom(second)); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel("release-next"); err != nil {
		t.Fatal(err)
	}
	current, id, err := s.Current()
	if err != nil {
		t.Fatal(err)
	}
	if id != "release" {
		t.Fatalf("current publication %q, want release", id)
	}
	checksum := current.Variants[0].Segments[0].SHA256
	firstSource := scan(t, first)
	if checksum != firstSource.Variants[0].Segments[0].SHA256 {
		t.Fatal("canceled publication replaced segment content")
	}
}

func TestSwitchRejectsUnknownVariant(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	writeSource(t, source)
	scanAndPublish(t, root, source, "release")
	p, _, err := player.Open(store.Open(root), "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.ReadSegment(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.ReadSegment(); err != nil {
		t.Fatal(err)
	}
	if err := p.Switch("hd"); err == nil {
		t.Fatal("unknown variant switch was accepted")
	}
}

func TestClientCanContinueAfterBoundarySwitch(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	writeSource(t, source)
	scanAndPublish(t, root, source, "release")
	p, _, err := player.Open(store.Open(root), "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.ReadSegment(); err != nil {
		t.Fatal(err)
	}
	if p.Position() != media.SegmentDuration {
		t.Fatalf("position=%d, want boundary %d", p.Position(), media.SegmentDuration)
	}
	if err := p.Switch("sharp"); err != nil {
		t.Fatal(err)
	}
	header, packets, err := p.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	if header.StartPTS != media.SegmentDuration {
		t.Fatalf("switched segment starts at %d, want %d", header.StartPTS, media.SegmentDuration)
	}
	if len(packets) != 8 {
		t.Fatalf("segment contains %d packets, want 8 audio/video packets", len(packets))
	}
	if got := p.Position(); got != 2*media.SegmentDuration {
		t.Fatalf("position=%d after switch, want %d", got, 2*media.SegmentDuration)
	}
}

func writeSource(t *testing.T, root string) {
	t.Helper()
	for _, spec := range media.Variants {
		if err := os.MkdirAll(filepath.Join(root, spec.Name), 0o755); err != nil {
			t.Fatal(err)
		}
		for segment := range media.SegmentCount {
			path := filepath.Join(root, spec.Name, segmentName(segment))
			if err := media.WriteCompleteSegment(path, spec, segment); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func scan(t *testing.T, root string) indexer.SourceIndex {
	t.Helper()
	index, err := indexer.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	return index
}

func scanAndPublish(t *testing.T, repositoryRoot, sourceDir, id string) *store.Index {
	t.Helper()
	sourceIndex := scan(t, sourceDir)
	variants := publishedVariants(t, sourceIndex)
	s := store.Open(repositoryRoot)
	if _, err := s.Prepare(id, variants, copyFrom(sourceDir)); err != nil {
		t.Fatal(err)
	}
	timeline := sourceIndex.Variants[0].Segments[len(sourceIndex.Variants[0].Segments)-1].End
	published := store.Index{Timeline: timeline, Variants: variants}
	if err := s.Commit(id, published); err != nil {
		t.Fatal(err)
	}
	return &published
}

func publishedVariants(t *testing.T, source indexer.SourceIndex) []store.Variant {
	t.Helper()
	variants := make([]store.Variant, 0, len(source.Variants))
	for _, sourceVariant := range source.Variants {
		spec := variantSpec(t, sourceVariant.Name)
		segments := make([]store.Segment, 0, len(sourceVariant.Segments))
		for _, segment := range sourceVariant.Segments {
			segments = append(segments, store.Segment{Index: segment.Index, Start: segment.Start, End: segment.End, Path: filepath.Base(segment.Path), SHA256: segment.SHA256})
		}
		variants = append(variants, store.Variant{Name: sourceVariant.Name, Width: spec.Width, Height: spec.Height, Segments: segments})
	}
	return variants
}

func copyFrom(sourceDir string) func(string, int, string) error {
	return func(variantName string, segmentIndex int, destination string) error {
		input, err := os.Open(filepath.Join(sourceDir, variantName, segmentName(segmentIndex)))
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.Create(destination)
		if err != nil {
			return err
		}
		defer output.Close()
		_, err = io.Copy(output, input)
		return err
	}
}

func assertVideo(t *testing.T, spec media.VariantSpec, header media.Header, packet media.Packet) {
	t.Helper()
	frame := int(packet.PTS / media.FrameDuration)
	for y := range int(header.Height) {
		for x := range int(header.Width) {
			offset := (y*int(header.Width) + x) * 3
			r, g, b := media.SourceFrame(frame, int(header.Width), int(header.Height), x, y)
			if packet.Video[offset] != r || packet.Video[offset+1] != g || packet.Video[offset+2] != b {
				t.Fatalf("wrong pixel for %s frame %d at %d,%d", spec.Name, frame, x, y)
			}
		}
	}
}

func assertAudio(t *testing.T, spec media.VariantSpec, packet media.Packet) {
	t.Helper()
	frame := int(packet.PTS / media.FrameDuration)
	for sampleOffset := range media.FrameDuration {
		for channel := 0; channel < 2; channel++ {
			offset := (int(sampleOffset)*2 + channel) * 2
			want := media.AudioSample(spec, packet.PTS+sampleOffset, frame, channel)
			got := media.ReadSample(packet.Audio[offset : offset+2])
			if got != want {
				t.Fatalf("wrong audio for %s frame %d sample %d channel %d", spec.Name, frame, sampleOffset, channel)
			}
		}
	}
}

func variantSpec(t *testing.T, name string) media.VariantSpec {
	t.Helper()
	for _, spec := range media.Variants {
		if spec.Name == name {
			return spec
		}
	}
	t.Fatalf("unknown variant %s", name)
	return media.VariantSpec{}
}

func segmentName(index int) string {
	return filepath.Join(fmt.Sprintf("%06d.sm", index))
}
