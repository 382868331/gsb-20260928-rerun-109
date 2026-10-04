package media

import (
	"os"
	"path/filepath"
)

const (
	FrameDuration    = uint64(12000)
	FramesPerSegment = 4
	SegmentDuration  = FrameDuration * FramesPerSegment
	SegmentCount     = 6
)

func WriteCompleteSegment(path string, variant VariantSpec, segment int) error {
	return writeSegmentFile(path, variant, segment, 0)
}

// WriteCorrectedSegment writes a replacement segment for an already
// published window position. correction must be > 0 and identifies the
// correction generation.
func WriteCorrectedSegment(path string, variant VariantSpec, segment int, correction int) error {
	return writeSegmentFile(path, variant, segment, correction)
}

func writeSegmentFile(path string, variant VariantSpec, segment int, correction int) error {
	dir, name := filepath.Split(path)
	temporary, err := os.CreateTemp(dir, "."+name+"-*.part")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := writeSegment(temporary, variant, segment, correction); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func writeSegment(f *os.File, variant VariantSpec, segment int, correction int) error {
	start := uint64(segment) * SegmentDuration
	h := Header{
		Width:     variant.Width,
		Height:    variant.Height,
		AudioRate: TimeScale,
		Channels:  2,
		StartPTS:  start,
		Duration:  SegmentDuration,
	}
	w := NewWriter(f, h)
	if err := w.WriteHeader(FramesPerSegment, FramesPerSegment); err != nil {
		return err
	}
	for frameIndex := range FramesPerSegment {
		globalFrame := segment*FramesPerSegment + frameIndex
		pts := start + uint64(frameIndex)*FrameDuration
		video := make([]byte, 0, int(variant.Width)*int(variant.Height)*3)
		for y := range int(variant.Height) {
			for x := range int(variant.Width) {
				var r, g, b byte
				if correction == 0 {
					r, g, b = SourceFrame(globalFrame, int(variant.Width), int(variant.Height), x, y)
				} else {
					r, g, b = CorrectedFrame(globalFrame, correction, int(variant.Width), int(variant.Height), x, y)
				}
				video = append(video, r, g, b)
			}
		}
		if err := w.WritePacket(Packet{Type: VideoPacket, KeyFrame: true, PTS: pts, Duration: uint32(FrameDuration), Video: video}); err != nil {
			return err
		}
		audio := make([]byte, 0, int(FrameDuration)*2*2)
		for sampleOffset := range FrameDuration {
			for channel := 0; channel < 2; channel++ {
				sample := AudioSample(variant, pts+sampleOffset, globalFrame, channel)
				if correction != 0 {
					sample = CorrectedAudioSample(variant, pts+sampleOffset, globalFrame, channel, correction)
				}
				audio = AppendSample(audio, sample)
			}
		}
		if err := w.WritePacket(Packet{Type: AudioPacket, KeyFrame: false, PTS: pts, Duration: uint32(FrameDuration), Audio: audio}); err != nil {
			return err
		}
	}
	if _, err := f.Write([]byte{0}); err != nil {
		return err
	}
	return nil
}
