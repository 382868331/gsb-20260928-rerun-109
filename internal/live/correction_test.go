package live_test

import (
	"errors"
	"testing"

	"localstream/internal/live"
	"localstream/internal/media"
	"localstream/internal/player"
	"localstream/internal/store"
)

func correctSpec(segment int) media.Correction {
	return media.Correction{Segment: segment, Revision: 1, AudioGain: 0.5, VideoTint: 201}
}

func TestCorrectionNewOpenerAndResumeAtSameMoment(t *testing.T) {
	root := t.TempDir()
	publisher, err := live.NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	var latest *store.Index
	for range 5 {
		latest, err = publisher.PublishNext()
		if err != nil {
			t.Fatal(err)
		}
	}
	correctionPTS := uint64(3) * media.SegmentDuration

	// A player already playing the old version reads segment 3 before the fix.
	old, current, err := player.OpenStream(store.Open(root), "news", "base")
	if err != nil {
		t.Fatal(err)
	}
	oldID := current.ID
	if err := old.JumpTo(correctionPTS); err != nil {
		t.Fatal(err)
	}
	header, packets, err := old.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	assertPackets(t, "base", header, packets)
	if old.Position() != 4*media.SegmentDuration {
		t.Fatalf("position=%d", old.Position())
	}

	corrected, err := publisher.PublishCorrection(correctSpec(3))
	if err != nil {
		t.Fatal(err)
	}
	if corrected.ID == oldID {
		t.Fatal("correction did not produce a new version id")
	}
	if corrected.WindowStart != latest.WindowStart || corrected.Timeline != latest.Timeline {
		t.Fatalf("correction moved the window: [%d,%d) vs [%d,%d)", corrected.WindowStart, corrected.Timeline, latest.WindowStart, latest.Timeline)
	}
	// Every quality variant is part of the same decision.
	for _, variant := range corrected.Variants {
		var segment store.Segment
		found := false
		for _, candidate := range variant.Segments {
			if candidate.Index == 3 {
				segment = candidate
				found = true
			}
		}
		if !found || segment.Revision != 1 {
			t.Fatalf("variant %s corrected segment=%+v found=%v", variant.Name, segment, found)
		}
		if checksum := fileChecksum(t, root, corrected.ID, variant.Name, 3); checksum != segment.SHA256 {
			t.Fatalf("variant %s on-disk checksum mismatch", variant.Name)
		}
	}

	// A brand new player gets the explicit latest (corrected) version.
	fresh, freshIndex, err := player.OpenStream(store.Open(root), "news", "sharp")
	if err != nil {
		t.Fatal(err)
	}
	if freshIndex.ID != corrected.ID {
		t.Fatalf("new opener got %s, want %s", freshIndex.ID, corrected.ID)
	}
	if err := fresh.JumpTo(correctionPTS); err != nil {
		t.Fatal(err)
	}
	header, packets, err = fresh.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	assertCorrectedPackets(t, correctSpec(3), "sharp", header, packets)

	// The old index and files remain readable while the old player holds them.
	if !store.Open(root).VersionExists(oldID) {
		t.Fatal("old version removed while held by a player")
	}
	oldIndex, err := store.Open(root).ReadIndex(oldID)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range oldIndex.Variants {
		for _, segment := range variant.Segments {
			if segment.Index == 3 && segment.Revision != 0 {
				t.Fatalf("held old index was rewritten for %s", variant.Name)
			}
		}
	}

	// Adopting the correction resumes at the same content moment, never re-reads
	// the half-finished segment, and reports the differing interval.
	resumed, err := old.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID != corrected.ID || old.Position() != 4*media.SegmentDuration {
		t.Fatalf("refresh moved player: id=%s position=%d", resumed.ID, old.Position())
	}
	changed := old.ChangedSegments()
	if len(changed) != 1 || changed[0].Index != 3 || changed[0].Revision != 1 {
		t.Fatalf("changed segments=%+v", changed)
	}
	if err := old.Switch("sharp"); err != nil {
		t.Fatal(err)
	}
	header, packets, err = old.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	if header.StartPTS != 4*media.SegmentDuration {
		t.Fatalf("resume skipped or repeated time: %d", header.StartPTS)
	}
	assertPackets(t, "sharp", header, packets)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCorrectionExpiryRulesAndAtomicFailure(t *testing.T) {
	root := t.TempDir()
	publisher, err := live.NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := publisher.PublishNext(); err != nil {
			t.Fatal(err)
		}
	}
	correction := correctSpec(2)
	fixed, err := publisher.PublishCorrection(correction)
	if err != nil {
		t.Fatal(err)
	}
	// Hold the first corrected version before the window scrolls away from it.
	held, _, err := player.OpenStreamAt(store.Open(root), "news", "base", fixed.WindowStart)
	if err != nil {
		t.Fatal(err)
	}
	if held.Publication() != fixed.ID {
		t.Fatalf("holder got %s, want %s", held.Publication(), fixed.ID)
	}

	// Same segment corrected again bumps the revision for all variants.
	again, err := publisher.PublishCorrection(media.Correction{Segment: 2, AudioGain: 0.25, VideoTint: 33})
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range again.Variants {
		for _, segment := range variant.Segments {
			if segment.Index == 2 && segment.Revision != 2 {
				t.Fatalf("revision=%d for %s", segment.Revision, variant.Name)
			}
		}
	}

	// New segments roll segment 2 out of the window.
	for range 3 {
		if _, err := publisher.PublishNext(); err != nil {
			t.Fatal(err)
		}
	}
	current, currentID, err := store.Open(root).CurrentForStream("news")
	if err != nil {
		t.Fatal(err)
	}
	if current.WindowStart != 5*media.SegmentDuration {
		t.Fatalf("window start=%d", current.WindowStart)
	}
	if _, err := publisher.PublishCorrection(correctSpec(2)); !errors.Is(err, live.ErrCorrectionExpired) {
		t.Fatalf("expired correction error=%v", err)
	}
	after, afterID, err := store.Open(root).CurrentForStream("news")
	if err != nil || afterID != currentID || after.Timeline != current.Timeline {
		t.Fatalf("failed correction changed current version: %s vs %s", afterID, currentID)
	}

	// The player holding the first corrected version keeps its files.
	if _, _, err := held.ReadSegment(); err != nil {
		t.Fatal(err)
	}
	if _, err := held.Refresh(); !errors.Is(err, player.ErrWindowExpired) {
		t.Fatalf("refresh error=%v", err)
	}
	if !store.Open(root).VersionExists(fixed.ID) {
		t.Fatal("held corrected version was cleaned up")
	}
	if err := held.JumpToLive(); err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCorrectionSurvivesPublisherRestart(t *testing.T) {
	root := t.TempDir()
	publisher, err := live.NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := publisher.PublishNext(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := publisher.PublishCorrection(correctSpec(3)); err != nil {
		t.Fatal(err)
	}
	restarted, err := live.NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.NextPTS() != 5*media.SegmentDuration {
		t.Fatalf("next pts after restart=%d", restarted.NextPTS())
	}
	advanced, err := restarted.PublishNext()
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range advanced.Variants {
		byIndex := map[int]store.Segment{}
		for _, segment := range variant.Segments {
			byIndex[segment.Index] = segment
		}
		if byIndex[3].Revision != 1 {
			t.Fatalf("correction lost after restart for %s: %+v", variant.Name, byIndex[3])
		}
		if byIndex[5].Revision != 0 {
			t.Fatalf("new segment inherited revision %d", byIndex[5].Revision)
		}
	}
}

func fileChecksum(t *testing.T, root, id, variantName string, segmentIndex int) string {
	t.Helper()
	index, err := store.Open(root).ReadIndex(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range index.Variants {
		if variant.Name != variantName {
			continue
		}
		for _, segment := range variant.Segments {
			if segment.Index == segmentIndex {
				return segment.SHA256
			}
		}
	}
	t.Fatalf("segment %d missing in %s", segmentIndex, id)
	return ""
}

func assertCorrectedPackets(t *testing.T, correction media.Correction, variantName string, header media.Header, packets []media.Packet) {
	t.Helper()
	spec := media.VariantSpec{}
	for _, candidate := range media.Variants {
		if candidate.Name == variantName {
			spec = candidate
		}
	}
	for _, packet := range packets {
		if packet.Type == media.VideoPacket {
			frame := int(packet.PTS / media.FrameDuration)
			for y := range int(header.Height) {
				for x := range int(header.Width) {
					offset := (y*int(header.Width) + x) * 3
					r, g, b := correction.VideoPixel(frame, int(header.Width), int(header.Height), x, y)
					if packet.Video[offset] != r || packet.Video[offset+1] != g || packet.Video[offset+2] != b {
						t.Fatalf("wrong corrected pixel at frame %d %d,%d", frame, x, y)
					}
				}
			}
		}
		if packet.Type == media.AudioPacket {
			frame := int(packet.PTS / media.FrameDuration)
			for sample := range packet.Duration {
				for channel := 0; channel < 2; channel++ {
					offset := (int(sample)*2 + channel) * 2
					want := correction.AudioSample(spec, packet.PTS+uint64(sample), frame, channel)
					if got := media.ReadSample(packet.Audio[offset : offset+2]); got != want {
						t.Fatalf("wrong corrected sample %d channel %d", sample, channel)
					}
				}
			}
		}
	}
}
