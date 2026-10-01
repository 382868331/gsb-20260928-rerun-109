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

func TestLiveWindowSwitchRewindExpiryAndRetention(t *testing.T) {
	root := t.TempDir()
	publisher, err := live.NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	var latest *store.Index
	for range 6 {
		latest, err = publisher.PublishNext()
		if err != nil {
			t.Fatal(err)
		}
	}
	if publisher.NextPTS() != 6*media.SegmentDuration {
		t.Fatalf("next pts=%d", publisher.NextPTS())
	}
	if latest.WindowStart != 3*media.SegmentDuration || latest.Timeline != 6*media.SegmentDuration {
		t.Fatalf("window=[%d,%d)", latest.WindowStart, latest.Timeline)
	}

	slow, current, err := player.OpenStream(store.Open(root), "news", "base")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != latest.ID {
		t.Fatalf("current=%q want %q", current.ID, latest.ID)
	}
	header, packets, err := slow.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	if header.StartPTS != 3*media.SegmentDuration {
		t.Fatalf("start=%d", header.StartPTS)
	}
	assertPackets(t, "base", header, packets)
	if err := slow.Switch("sharp"); err != nil {
		t.Fatal(err)
	}
	header, packets, err = slow.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	if header.StartPTS != 4*media.SegmentDuration || header.Width != media.Variants[1].Width {
		t.Fatalf("switched header=%+v", header)
	}
	assertPackets(t, "sharp", header, packets)

	newPublisher, err := live.NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	if newPublisher.NextPTS() != 6*media.SegmentDuration {
		t.Fatalf("resumed next pts=%d", newPublisher.NextPTS())
	}
	var advanced *store.Index
	for range 3 {
		advanced, err = newPublisher.PublishNext()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if advanced.WindowStart != 6*media.SegmentDuration || advanced.Timeline != 9*media.SegmentDuration {
		t.Fatalf("advanced window=[%d,%d)", advanced.WindowStart, advanced.Timeline)
	}
	oldID := current.ID
	if !store.Open(root).VersionExists(oldID) {
		t.Fatal("retained old publication was removed while player is open")
	}
	refreshed, refreshErr := slow.Refresh()
	if refreshErr == nil {
		t.Fatalf("unexpected refresh to %s window=[%d,%d) position=%d publication=%s", refreshed.ID, refreshed.WindowStart, refreshed.Timeline, slow.Position(), slow.Publication())
	}
	err = refreshErr
	if !errors.Is(err, player.ErrWindowExpired) {
		t.Fatalf("refresh error=%v, want ErrWindowExpired", err)
	}
	if slow.Publication() != oldID {
		t.Fatalf("failed refresh changed publication from %s to %s", oldID, slow.Publication())
	}
	header, packets, err = slow.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	if header.StartPTS != 5*media.SegmentDuration {
		t.Fatalf("old fixed publication jumped to pts %d", header.StartPTS)
	}
	assertPackets(t, "sharp", header, packets)
	if _, _, err := slow.ReadSegment(); !errors.Is(err, player.ErrPlaybackEnded) {
		t.Fatalf("old window end error=%v, want ErrPlaybackEnded", err)
	}
	if err := slow.JumpToLive(*advanced); err != nil {
		t.Fatal(err)
	}
	if err := slow.JumpTo(7 * media.SegmentDuration); err != nil {
		t.Fatal(err)
	}
	header, packets, err = slow.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	if header.StartPTS != 7*media.SegmentDuration {
		t.Fatalf("window rewind pts=%d", header.StartPTS)
	}
	assertPackets(t, "sharp", header, packets)
	if err := slow.JumpToLive(*advanced); err != nil {
		t.Fatal(err)
	}
	header, packets, err = slow.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	if header.StartPTS != 8*media.SegmentDuration {
		t.Fatalf("after jump pts=%d", header.StartPTS)
	}
	assertPackets(t, "sharp", header, packets)
	if err := slow.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = newPublisher.PublishNext(); err != nil {
		t.Fatal(err)
	}
	if store.Open(root).VersionExists(oldID) {
		t.Fatal("old publication remains after all players released it")
	}
}

func TestStagingFragmentsAreNotPublished(t *testing.T) {
	root := t.TempDir()
	publisher, err := live.NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishNext(); err != nil {
		t.Fatal(err)
	}
	files := []string{}
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && (filepath.Ext(path) == ".part" || filepath.Base(path) == "sharp.sm") && filepath.Dir(path) != root {
			files = append(files, path)
		}
		return nil
	})
	if len(files) != 0 {
		t.Fatalf("temporary fragments are visible: %v", files)
	}
}

func TestInterruptedPublicationDoesNotAdvanceTimeline(t *testing.T) {
	root := t.TempDir()
	publisher, err := live.NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.PublishNext(); err != nil {
		t.Fatal(err)
	}
	futureID := publisher.VersionID(1)
	if err := os.MkdirAll(filepath.Join(root, "publish", "staging-"+futureID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "publish", "staging-"+futureID, "index.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	recovered, err := live.NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.NextPTS() != media.SegmentDuration {
		t.Fatalf("recovered next pts=%d, want %d", recovered.NextPTS(), media.SegmentDuration)
	}
	index, err := recovered.PublishNext()
	if err != nil {
		t.Fatal(err)
	}
	if index.Timeline != 2*media.SegmentDuration {
		t.Fatalf("timeline=%d after interrupted publish", index.Timeline)
	}
}

func assertPackets(t *testing.T, variantName string, header media.Header, packets []media.Packet) {
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
		if packet.Type == media.VideoPacket {
			frame := int(packet.PTS / media.FrameDuration)
			for y := range int(header.Height) {
				for x := range int(header.Width) {
					offset := (y*int(header.Width) + x) * 3
					r, g, b := media.SourceFrame(frame, int(header.Width), int(header.Height), x, y)
					if packet.Video[offset] != r || packet.Video[offset+1] != g || packet.Video[offset+2] != b {
						t.Fatalf("wrong pixel at frame %d %d,%d", frame, x, y)
					}
				}
			}
		}
		if packet.Type == media.AudioPacket {
			frame := int(packet.PTS / media.FrameDuration)
			for sample := range packet.Duration {
				for channel := 0; channel < 2; channel++ {
					offset := (int(sample)*2 + channel) * 2
					want := media.AudioSample(spec, packet.PTS+uint64(sample), frame, channel)
					got := media.ReadSample(packet.Audio[offset : offset+2])
					if got != want {
						t.Fatalf("wrong sample %d channel %d", sample, channel)
					}
				}
			}
		}
	}
}
