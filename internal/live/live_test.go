package live

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"localstream/internal/media"
)

func TestLiveWindowSwitchRewindAndExactContent(t *testing.T) {
	root := t.TempDir()
	publisher, err := NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := publisher.PublishNext(media.Variants); err != nil {
			t.Fatal(err)
		}
	}
	player, err := OpenPlayer(publisher, "base")
	if err != nil {
		t.Fatal(err)
	}
	defer player.Close()

	assertRead(t, publisher, player, 0)
	assertRead(t, publisher, player, 1)
	if _, err := player.Jump(2); err != nil {
		t.Fatal(err)
	}
	if _, err := player.Jump(1); err != nil {
		t.Fatal(err)
	}
	if err := player.Switch("sharp"); err != nil {
		t.Fatal(err)
	}
	if _, err := player.Jump(2); err != nil {
		t.Fatal(err)
	}
	assertRead(t, publisher, player, 2)

	if _, err := publisher.PublishNext(media.Variants); err != nil {
		t.Fatal(err)
	}
	assertRead(t, publisher, player, 3)
	if player.Position() != 4*media.SegmentDuration {
		t.Fatalf("position=%d, want %d", player.Position(), 4*media.SegmentDuration)
	}
}

func TestExpiredPlaybackRequiresExplicitJumpOrStop(t *testing.T) {
	root := t.TempDir()
	publisher, err := NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if _, err := publisher.PublishNext(media.Variants); err != nil {
			t.Fatal(err)
		}
	}
	slow, err := OpenPlayer(publisher, "base")
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	if _, _, err := slow.ReadSegment(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := publisher.PublishNext(media.Variants); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := slow.ReadSegment(); !errors.Is(err, ErrWindowExpired) {
		t.Fatalf("read after falling outside window returned %v, want ErrWindowExpired", err)
	}
	if _, err := slow.JumpToLive(); err != nil {
		t.Fatal(err)
	}
	if slow.Sequence() != 5 {
		t.Fatalf("jumped to %d, want live edge segment 5", slow.Sequence())
	}
	slow.Stop()
	if _, _, err := slow.ReadSegment(); !errors.Is(err, ErrStopped) {
		t.Fatalf("stopped read returned %v, want ErrStopped", err)
	}
}

func TestOldIndexKeepsFilesUntilPlayerReleases(t *testing.T) {
	root := t.TempDir()
	publisher, err := NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := publisher.PublishNext(media.Variants); err != nil {
			t.Fatal(err)
		}
	}
	player, err := OpenPlayer(publisher, "base")
	if err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(root, "live", "news", "base", "000000.sm")
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := publisher.PublishNext(media.Variants); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(oldFile); err != nil {
		t.Fatalf("file needed by an old player was removed early: %v", err)
	}
	player.Close()
	if _, err := os.Stat(oldFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired file after release: err=%v, want os.ErrNotExist", err)
	}
}

func TestInterruptedStagingIsHiddenAndResumeUsesNextTimestamp(t *testing.T) {
	root := t.TempDir()
	publisher, err := NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := publisher.PublishNext(media.Variants); err != nil {
			t.Fatal(err)
		}
	}
	part := filepath.Join(root, "live", "news", "base", "000002.sm.part")
	if err := os.WriteFile(part, []byte("incomplete"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publisher.RecoverInterruption(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(part); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging file remained after recovery: %v", err)
	}
	resumed, err := NewPublisher(root, "news", 3)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := resumed.PublishNext(media.Variants)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.NextSequence != 3 {
		t.Fatalf("next sequence=%d, want 3", manifest.NextSequence)
	}
	segment := manifest.Variants[0].Segments[2]
	if segment.Sequence != 2 || segment.Start != 2*media.SegmentDuration {
		t.Fatalf("resumed segment reused a timestamp: %+v", segment)
	}
	player, err := OpenPlayer(resumed, "base")
	if err != nil {
		t.Fatal(err)
	}
	defer player.Close()
	if _, err := player.Jump(2); err != nil {
		t.Fatal(err)
	}
	assertRead(t, resumed, player, 2)
}

func assertRead(t *testing.T, publisher *Publisher, player *Player, sequence int) {
	t.Helper()
	header, packets, err := player.ReadSegment()
	if err != nil {
		t.Fatal(err)
	}
	if header.StartPTS != uint64(sequence)*media.SegmentDuration {
		t.Fatalf("start=%d, want segment %d", header.StartPTS, sequence)
	}
	spec := variantSpec(player.Variant())
	if len(packets) != 2*media.FramesPerSegment {
		t.Fatalf("packet count=%d, want %d", len(packets), 2*media.FramesPerSegment)
	}
	for _, packet := range packets {
		frame := int(packet.PTS / media.FrameDuration)
		switch packet.Type {
		case media.VideoPacket:
			for y := range int(header.Height) {
				for x := range int(header.Width) {
					offset := (y*int(header.Width) + x) * 3
					r, g, b := media.SourceFrame(frame, int(header.Width), int(header.Height), x, y)
					if packet.Video[offset] != r || packet.Video[offset+1] != g || packet.Video[offset+2] != b {
						t.Fatalf("wrong pixel frame=%d x=%d y=%d", frame, x, y)
					}
				}
			}
		case media.AudioPacket:
			for offsetSamples := range media.FrameDuration {
				for channel := 0; channel < 2; channel++ {
					offset := (int(offsetSamples)*2 + channel) * 2
					want := media.AudioSample(spec, packet.PTS+offsetSamples, frame, channel)
					got := media.ReadSample(packet.Audio[offset : offset+2])
					if got != want {
						t.Fatalf("wrong sample frame=%d offset=%d channel=%d", frame, offsetSamples, channel)
					}
				}
			}
		}
	}
}

func variantSpec(name string) media.VariantSpec {
	for _, spec := range media.Variants {
		if spec.Name == name {
			return spec
		}
	}
	return media.VariantSpec{}
}
