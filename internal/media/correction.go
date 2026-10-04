package media

import (
	"math"
	"os"
	"path/filepath"
)

// Correction describes a replacement for one published segment. The video is
// replaced by a distinct correction pattern and the audio gain is applied to
// every sample, which models volume fixes.
type Correction struct {
	Segment   int
	Revision  int
	AudioGain float64
	VideoTint byte
}

func (c Correction) AudioSample(variant VariantSpec, ptsInTimescale uint64, frame, channel int) int16 {
	t := float64(ptsInTimescale) / float64(TimeScale)
	value := math.Sin(2*math.Pi*variant.Frequency*t) * 22000
	if channel == 1 {
		value *= 0.75
	}
	value += float64(frame%7) - 3
	value *= c.AudioGain
	if value > 32767 {
		value = 32767
	}
	if value < -32768 {
		value = -32768
	}
	return int16(value)
}

func (c Correction) VideoPixel(frame, width, height, x, y int) (byte, byte, byte) {
	progress := byte((frame * 255) / 47)
	return c.VideoTint, progress, byte(((x + y) * 255) / max(width+height-2, 1))
}

// WriteCorrectedSegment renders a corrected 1 second segment. It reuses the
// same header geometry and PTS so it can replace an already published segment
// without disturbing the shared timeline.
func WriteCorrectedSegment(path string, variant VariantSpec, correction Correction) error {
	dir, name := filepath.Split(path)
	temporary, err := os.CreateTemp(dir, "."+name+"-*.part")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := writeCorrectedSegment(temporary, variant, correction); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func writeCorrectedSegment(f *os.File, variant VariantSpec, correction Correction) error {
	segment := correction.Segment
	start := uint64(segment) * SegmentDuration
	header := Header{
		Width:     variant.Width,
		Height:    variant.Height,
		AudioRate: TimeScale,
		Channels:  2,
		StartPTS:  start,
		Duration:  SegmentDuration,
	}
	w := NewWriter(f, header)
	if err := w.WriteHeader(FramesPerSegment, FramesPerSegment); err != nil {
		return err
	}
	for frameIndex := range FramesPerSegment {
		globalFrame := segment*FramesPerSegment + frameIndex
		pts := start + uint64(frameIndex)*FrameDuration
		video := make([]byte, 0, int(variant.Width)*int(variant.Height)*3)
		for y := range int(variant.Height) {
			for x := range int(variant.Width) {
				r, g, b := correction.VideoPixel(globalFrame, int(variant.Width), int(variant.Height), x, y)
				video = append(video, r, g, b)
			}
		}
		if err := w.WritePacket(Packet{Type: VideoPacket, KeyFrame: true, PTS: pts, Duration: uint32(FrameDuration), Video: video}); err != nil {
			return err
		}
		audio := make([]byte, 0, int(FrameDuration)*2*2)
		for sampleOffset := range FrameDuration {
			for channel := 0; channel < 2; channel++ {
				audio = AppendSample(audio, correction.AudioSample(variant, pts+uint64(sampleOffset), globalFrame, channel))
			}
		}
		if err := w.WritePacket(Packet{Type: AudioPacket, KeyFrame: false, PTS: pts, Duration: uint32(FrameDuration), Audio: audio}); err != nil {
			return err
		}
	}
	_, err := f.Write([]byte{0})
	return err
}
