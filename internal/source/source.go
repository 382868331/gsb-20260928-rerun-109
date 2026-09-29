// Package source generates the bundled short test media entirely from Go
// code: a mono WAV tone plus a grayscale y4m video whose luma encodes the
// presentation time. ffmpeg only encodes/muxes the generated raw material.
package source

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
)

const (
	Width      = 320
	Height     = 240
	VideoFPS   = 10
	AudioRate  = 24000
	Duration   = 8 // seconds, divisible by the segment duration
	LumaStart  = 24.0
	LumaEnd    = 232.0
	ToneAmp    = 0.6
	ChirpStart = 220.0 // Hz at t=0
	ChirpEnd   = 880.0 // Hz at t=Duration
)

// Media is a generated source asset bundle.
type Media struct {
	Dir      string
	VideoY4M string
	AudioWAV string
	MuxedMP4 string
	Frames   int
	Samples  int
}

// Generate writes raw audio/video assets and a muxed source MP4 into dir.
func Generate(dir string) (*Media, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m := &Media{
		Dir:      dir,
		VideoY4M: filepath.Join(dir, "video.y4m"),
		AudioWAV: filepath.Join(dir, "audio.wav"),
		MuxedMP4: filepath.Join(dir, "source.mp4"),
		Frames:   VideoFPS * Duration,
		Samples:  AudioRate * Duration,
	}
	if err := writeY4M(m.VideoY4M, m.Frames); err != nil {
		return nil, err
	}
	if err := writeWAV(m.AudioWAV, m.Samples); err != nil {
		return nil, err
	}
	if err := mux(m); err != nil {
		return nil, err
	}
	return m, nil
}

// LumaAt returns the generated whole-frame luma for presentation time t.
func LumaAt(tSeconds float64) float64 {
	return LumaStart + (LumaEnd-LumaStart)*tSeconds/float64(Duration)
}

func writeY4M(path string, frames int) error {
	var buf bytes.Buffer
	buf.WriteString("YUV4MPEG2 W")
	buf.WriteString(itoa(Width))
	buf.WriteString(" H")
	buf.WriteString(itoa(Height))
	buf.WriteString(" F10:1 Ip A0:0 Cmono\n")
	plane := make([]byte, Width*Height)
	for f := 0; f < frames; f++ {
		t := float64(f) / VideoFPS
		luma := byte(math.Round(LumaAt(t)))
		for i := range plane {
			plane[i] = luma
		}
		buf.WriteString("FRAME\n")
		buf.Write(plane)
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func writeWAV(path string, samples int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dataBytes := samples * 2
	var hdr bytes.Buffer
	hdr.WriteString("RIFF")
	binary.Write(&hdr, binary.LittleEndian, uint32(36+dataBytes))
	hdr.WriteString("WAVEfmt ")
	binary.Write(&hdr, binary.LittleEndian, uint32(16))
	binary.Write(&hdr, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&hdr, binary.LittleEndian, uint16(1)) // mono
	binary.Write(&hdr, binary.LittleEndian, uint32(AudioRate))
	binary.Write(&hdr, binary.LittleEndian, uint32(AudioRate*2))
	binary.Write(&hdr, binary.LittleEndian, uint16(2))
	binary.Write(&hdr, binary.LittleEndian, uint16(16))
	hdr.WriteString("data")
	binary.Write(&hdr, binary.LittleEndian, uint32(dataBytes))
	if _, err := f.Write(hdr.Bytes()); err != nil {
		return err
	}
	// Write in chunks to bound memory.
	chunk := make([]byte, 0, AudioRate) // 0.5s of int16 samples
	for n := 0; n < samples; n++ {
		t := float64(n) / AudioRate
		// Linear chirp: a repeated or missing block makes the estimated
		// instantaneous frequency jump backwards or forwards, which the
		// verifier detects at splice boundaries.
		phase := 2 * math.Pi * (ChirpStart*t + (ChirpEnd-ChirpStart)*t*t/(2*Duration))
		v := math.Sin(phase)
		s := int16(math.Round(ToneAmp * v * math.MaxInt16))
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], uint16(s))
		chunk = append(chunk, b[:]...)
		if len(chunk) == cap(chunk) {
			if _, err := f.Write(chunk); err != nil {
				return err
			}
			chunk = chunk[:0]
		}
	}
	if len(chunk) > 0 {
		if _, err := f.Write(chunk); err != nil {
			return err
		}
	}
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
