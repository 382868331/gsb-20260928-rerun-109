package media

import (
	"encoding/binary"
	"math"
)

type VariantSpec struct {
	Name      string  `json:"name"`
	Width     uint16  `json:"width"`
	Height    uint16  `json:"height"`
	Frequency float64 `json:"frequency"`
}

var Variants = []VariantSpec{
	{Name: "base", Width: 16, Height: 12, Frequency: 330},
	{Name: "sharp", Width: 32, Height: 24, Frequency: 440},
}

func SourceFrame(frame int, width, height int, x, y int) (byte, byte, byte) {
	progress := byte((frame * 255) / 47)
	return byte((x * 255) / max(width-1, 1)), progress, byte((y * 255) / max(height-1, 1))
}

func AudioSample(variant VariantSpec, ptsInTimescale uint64, frame int, channel int) int16 {
	t := float64(ptsInTimescale) / float64(TimeScale)
	value := math.Sin(2*math.Pi*variant.Frequency*t) * 22000
	if channel == 1 {
		value *= 0.75
	}
	value += float64(frame%7) - 3
	if value > 32767 {
		value = 32767
	}
	if value < -32768 {
		value = -32768
	}
	return int16(value)
}

func AppendSample(b []byte, sample int16) []byte {
	return binary.LittleEndian.AppendUint16(b, uint16(uint32(uint16(sample))))
}

func ReadSample(b []byte) int16 {
	return int16(binary.LittleEndian.Uint16(b))
}
