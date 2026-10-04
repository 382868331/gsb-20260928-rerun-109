package live_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"localstream/internal/live"
	"localstream/internal/media"
	"localstream/internal/player"
	"localstream/internal/store"
)

func TestCorrectionAdoptDiffAndContinuity(t *testing.T) {
	root := t.TempDir()
	publisher, err := live.NewPublisher(root, "news", 4)
	if err != nil {
		t.Fatal(err)
	}
	var latest *store.Index
	for range 6 {
		if latest, err = publisher.PublishNext(); err != nil {
			t.Fatal(err)
		}
	}
	oldID := latest.ID

	viewer, opened, err := player.OpenStream(store.Open(root), "news", "base")
	if err != nil {
		t.Fatal(err)
	}
	if opened.ID != oldID {
		t.Fatalf("opened=%s want %s", opened.ID, oldID)
	}
	for want := 2; want <= 3; want++ {
		header, packets, err := viewer.ReadSegment()
		if err != nil {
			t.Fatal(err)
		}
		if header.StartPTS != uint64(want)*media.SegmentDuration {
			t.Fatalf("pts=%d want %d", header.StartPTS, want)
		}
		assertPackets(t, "base", header, packets)
	}
	if err := viewer.Switch("sharp"); err != nil {
		t.Fatal(err)
	}

	corrected, err := publisher.Correct(3)
	if err != nil {
		t.Fatal(err)
	}
	if corrected.ID == oldID {
		t.Fatal("correction reused the old publication id")
	}
	if corrected.WindowStart != latest.WindowStart || corrected.Timeline != latest.Timeline {
		t.Fatalf("correction changed window to [%d,%d)", corrected.WindowStart, corrected.Timeline)
	}
	ranges := store.DiffRanges(latest, corrected)
	if len(ranges) != 1 || ranges[0].Start != 3*media.SegmentDuration || ranges[0].End != 4*media.SegmentDuration {
		t.Fatalf("diff ranges=%+v", ranges)
	}

	fresh, current, err := player.OpenStream(store.Open(root), "news", "base")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != corrected.ID {
		t.Fatalf("new player got %s, want corrected %s", current.ID, corrected.ID)
	}
	if err := fresh.JumpTo(3 * media.SegmentDuration); err != nil {
		t.Fatal(err)
	}
	header, packets, err := fresh.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	assertCorrectedPackets(t, "base", 1, header, packets)
	header, packets, err = fresh.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	if header.StartPTS != 4*media.SegmentDuration {
		t.Fatalf("pts=%d", header.StartPTS)
	}
	assertPackets(t, "base", header, packets)
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}

	if viewer.Publication() != oldID {
		t.Fatalf("existing player moved to %s without adopting", viewer.Publication())
	}
	adopted, adoptRanges, err := viewer.AdoptLatest()
	if err != nil {
		t.Fatal(err)
	}
	if adopted.ID != corrected.ID || viewer.Publication() != corrected.ID {
		t.Fatalf("adopted=%s publication=%s", adopted.ID, viewer.Publication())
	}
	if len(adoptRanges) != 1 || adoptRanges[0].Start != 3*media.SegmentDuration || adoptRanges[0].End != 4*media.SegmentDuration {
		t.Fatalf("adopt ranges=%+v", adoptRanges)
	}
	if viewer.Position() != 4*media.SegmentDuration {
		t.Fatalf("position moved to %d on adopt", viewer.Position())
	}
	header, packets, err = viewer.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	if header.StartPTS != 4*media.SegmentDuration || header.Width != media.Variants[1].Width {
		t.Fatalf("continuity broken at %+v", header)
	}
	assertPackets(t, "sharp", header, packets)
	if err := viewer.JumpTo(3 * media.SegmentDuration); err != nil {
		t.Fatal(err)
	}
	header, packets, err = viewer.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	if header.StartPTS != 3*media.SegmentDuration {
		t.Fatalf("rewind pts=%d", header.StartPTS)
	}
	assertCorrectedPackets(t, "sharp", 1, header, packets)

	for range 4 {
		if _, err := publisher.PublishNext(); err != nil {
			t.Fatal(err)
		}
	}
	s := store.Open(root)
	if s.VersionExists(oldID) {
		t.Fatal("superseded version remains after the last reader adopted the correction")
	}
	if !s.VersionExists(corrected.ID) {
		t.Fatal("corrected version removed while readers still hold it")
	}
	if err := viewer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishNext(); err != nil {
		t.Fatal(err)
	}
	if s.VersionExists(corrected.ID) {
		t.Fatal("corrected version remains after all readers released it")
	}
}

func TestCorrectionWindowExpiryRules(t *testing.T) {
	root := t.TempDir()
	publisher, err := live.NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if _, err := publisher.PublishNext(); err != nil {
			t.Fatal(err)
		}
	}
	viewer, opened, err := player.OpenStream(store.Open(root), "news", "base")
	if err != nil {
		t.Fatal(err)
	}
	header, packets, err := viewer.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	if header.StartPTS != 3*media.SegmentDuration {
		t.Fatalf("pts=%d", header.StartPTS)
	}
	if _, err := publisher.Correct(4); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := publisher.PublishNext(); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := viewer.AdoptLatest(); !errors.Is(err, player.ErrWindowExpired) {
		t.Fatalf("adopt error=%v, want ErrWindowExpired", err)
	}
	if viewer.Publication() != opened.ID {
		t.Fatalf("failed adopt moved player to %s", viewer.Publication())
	}
	for want := 4; want <= 5; want++ {
		header, packets, err := viewer.ReadSegment()
		if err != nil {
			t.Fatal(err)
		}
		if header.StartPTS != uint64(want)*media.SegmentDuration {
			t.Fatalf("pts=%d want %d", header.StartPTS, want)
		}
		assertPackets(t, "base", header, packets)
	}
	if _, _, err := viewer.ReadSegment(); !errors.Is(err, player.ErrPlaybackEnded) {
		t.Fatalf("old version end error=%v, want ErrPlaybackEnded", err)
	}
	if err := viewer.JumpToLive(); err != nil {
		t.Fatal(err)
	}
	header, packets, err = viewer.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	if header.StartPTS != 8*media.SegmentDuration {
		t.Fatalf("live pts=%d", header.StartPTS)
	}
	assertPackets(t, "base", header, packets)
	if err := viewer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCorrectionFailureKeepsOldVersion(t *testing.T) {
	root := t.TempDir()
	publisher, err := live.NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	var latest *store.Index
	for range 3 {
		if latest, err = publisher.PublishNext(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := publisher.Correct(99); err == nil {
		t.Fatal("correction outside the window succeeded")
	}
	if err := os.MkdirAll(filepath.Join(root, "live", "news"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "live", "news", ".staging-correct-1"), []byte("blocked"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Correct(1); err == nil {
		t.Fatal("correction with blocked staging succeeded")
	}
	index, id, err := store.Open(root).CurrentForStream("news")
	if err != nil {
		t.Fatal(err)
	}
	if id != latest.ID || index.Timeline != latest.Timeline {
		t.Fatalf("current pointer moved to %s after failed correction", id)
	}
	viewer, _, err := player.OpenStream(store.Open(root), "news", "base")
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	for want := 0; want < 3; want++ {
		header, packets, err := viewer.ReadSegment()
		if err != nil {
			t.Fatal(err)
		}
		if header.StartPTS != uint64(want)*media.SegmentDuration {
			t.Fatalf("pts=%d want %d", header.StartPTS, want)
		}
		assertPackets(t, "base", header, packets)
	}
	empty, err := live.NewPublisher(root, "empty", 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Correct(0); err == nil {
		t.Fatal("correction without any published version succeeded")
	}
}

func TestCorrectionPersistsAcrossReopen(t *testing.T) {
	root := t.TempDir()
	publisher, err := live.NewPublisher(root, "news", 4)
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if _, err := publisher.PublishNext(); err != nil {
			t.Fatal(err)
		}
	}
	corrected, err := publisher.Correct(2)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := live.NewPublisher(root, "news", 4)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.NextSegment() != 4 {
		t.Fatalf("reopened next segment=%d", reopened.NextSegment())
	}
	next, err := reopened.PublishNext()
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == corrected.ID || next.Timeline != 5*media.SegmentDuration || next.WindowStart != media.SegmentDuration {
		t.Fatalf("next=%s window=[%d,%d)", next.ID, next.WindowStart, next.Timeline)
	}
	viewer, current, err := player.OpenStream(store.Open(root), "news", "base")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != next.ID {
		t.Fatalf("reopened player got %s, want %s", current.ID, next.ID)
	}
	if err := viewer.JumpTo(2 * media.SegmentDuration); err != nil {
		t.Fatal(err)
	}
	header, packets, err := viewer.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	assertCorrectedPackets(t, "base", 1, header, packets)
	if err := viewer.JumpTo(4 * media.SegmentDuration); err != nil {
		t.Fatal(err)
	}
	header, packets, err = viewer.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	assertPackets(t, "base", header, packets)
	if err := viewer.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := live.NewPublisher(root, "news", 4)
	if err != nil {
		t.Fatal(err)
	}
	if again.NextSegment() != 5 {
		t.Fatalf("next segment=%d after second reopen", again.NextSegment())
	}
}

func assertCorrectedPackets(t *testing.T, variantName string, correction int, header media.Header, packets []media.Packet) {
	t.Helper()
	spec := media.VariantSpec{}
	for _, candidate := range media.Variants {
		if candidate.Name == variantName {
			spec = candidate
		}
	}
	seen := map[string]bool{}
	for _, packet := range packets {
		key := fmt.Sprintf("%d:%d", packet.Type, packet.PTS)
		if seen[key] {
			t.Fatalf("duplicate packet %s", key)
		}
		seen[key] = true
		frame := int(packet.PTS / media.FrameDuration)
		if packet.Type == media.VideoPacket {
			for y := range int(header.Height) {
				for x := range int(header.Width) {
					offset := (y*int(header.Width) + x) * 3
					r, g, b := media.CorrectedFrame(frame, correction, int(header.Width), int(header.Height), x, y)
					if packet.Video[offset] != r || packet.Video[offset+1] != g || packet.Video[offset+2] != b {
						t.Fatalf("wrong corrected pixel at frame %d %d,%d", frame, x, y)
					}
				}
			}
		}
		if packet.Type == media.AudioPacket {
			for sample := range packet.Duration {
				for channel := 0; channel < 2; channel++ {
					offset := (int(sample)*2 + channel) * 2
					want := media.CorrectedAudioSample(spec, packet.PTS+uint64(sample), frame, channel, correction)
					got := media.ReadSample(packet.Audio[offset : offset+2])
					if got != want {
						t.Fatalf("wrong corrected sample %d channel %d", sample, channel)
					}
				}
			}
		}
	}
}
