// Package verify actually decodes the segments of a playback plan and checks
// both timeline and content: video luma must match the time-encoded source
// image, and the audio chirp must be continuous across every read, including
// tier switches. Decoding is done with ffmpeg; nothing about the check is
// purely metadata-based.
package verify

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"

	"streamcore/internal/client"
	"streamcore/internal/manifest"
)

// Result reports the decoded verification of one playback plan.
type Result struct {
	DecodedVideoFrames  int
	DecodedAudioSamples int
	MaxLumaError        float64
	MaxFreqError        float64
	Boundaries          []BoundaryCheck
}

// BoundaryCheck records audio continuity at one splice/read boundary.
type BoundaryCheck struct {
	At             float64
	FreqBefore     float64
	FreqAfter      float64
	ExpectedBefore float64
	ExpectedAfter  float64
}

const (
	audioRate       = 24000
	lumaTolerance   = 12.0
	freqToleranceHz = 35.0
	windowSamples   = audioRate / 10 // 100 ms analysis windows
)

// Plan decodes every planned segment (init + fragment, per its rendition),
// concatenates raw decoded media, and verifies content vs source time.
func Plan(root string, m *manifest.Manifest, plan []client.PlannedSegment) (*Result, error) {
	if len(plan) == 0 {
		return nil, fmt.Errorf("empty plan")
	}
	var audio []float64      // trimmed, concatenated PCM mono @24kHz
	var lumaFrames []float64 // one mean-luma value per decoded video frame
	for _, ps := range plan {
		ren, err := m.RenditionTier(ps.Rendition)
		if err != nil {
			return nil, err
		}
		means, err := decodeVideoMeans(filepath.Join(root, ren.InitFile), filepath.Join(root, ps.File))
		if err != nil {
			return nil, fmt.Errorf("decode video %s: %w", ps.File, err)
		}
		lumaFrames = append(lumaFrames, means...)
		pcm, err := decodeAudio(filepath.Join(root, ren.InitFile), filepath.Join(root, ps.File))
		if err != nil {
			return nil, fmt.Errorf("decode audio %s: %w", ps.File, err)
		}
		audio = append(audio, segmentWindow(bytesToF32(pcm), ps.Index)...)
	}
	return analyze(lumaFrames, audio, len(plan))
}

// segmentWindow reduces one decoded AAC fragment to the exact 2s timeline
// window it represents. The first fragment starts with native encoder delay
// but decodes exactly 2.048s of sound; later fragments carry ~2048 samples of
// leading priming. Taking a fixed window at every read keeps tier-switch joins
// sample-aligned because both inits share the same priming length.
func segmentWindow(decoded []float64, segIndex int) []float64 {
	const window = 2 * audioRate // 48000
	var skip int
	if segIndex == 0 {
		skip = 0
		if len(decoded) > window {
			// First fragment carries native encoder delay; its last 1152
			// samples are the next fragment's priming, not source content.
			decoded = decoded[:window]
		}
	} else {
		skip = 128
	}
	if skip < 0 {
		skip = 0
	}
	if skip+window > len(decoded) {
		return decoded[skip:]
	}
	return decoded[skip : skip+window]
}

func bytesToF32(b []byte) []float64 {
	n := len(b) / 4
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		bits := binary.LittleEndian.Uint32(b[i*4:])
		out[i] = float64(math.Float32frombits(bits))
	}
	return out
}

func decodeVideoMeans(initPath, segPath string) ([]float64, error) {
	// Concatenate init + fragment into one temporary fMP4 so the decoder has
	// the moov/moov headers. Each rendition uses its own init.
	tmp, err := os.CreateTemp("", "seg-*.mp4")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if err := catFiles(tmp.Name(), initPath, segPath); err != nil {
		return nil, err
	}
	tmp.Close()
	cmd := exec.Command("ffmpeg", "-v", "error", "-i", tmp.Name(),
		"-an", "-pix_fmt", "gray", "-f", "rawvideo", "-")
	var out bytes.Buffer
	cmd.Stdout = &out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, stderr.String())
	}
	w, h, err := dims(tmp.Name())
	if err != nil {
		return nil, err
	}
	plane := w * h
	raw := out.Bytes()
	if len(raw)%plane != 0 {
		return nil, fmt.Errorf("decoded %d bytes, not a whole-frame multiple of %dx%d", len(raw), w, h)
	}
	frames := len(raw) / plane
	means := make([]float64, frames)
	for f := 0; f < frames; f++ {
		var sum float64
		base := f * plane
		for i := 0; i < plane; i++ {
			sum += float64(raw[base+i])
		}
		means[f] = sum / float64(plane)
	}
	return means, nil
}

func decodeAudio(initPath, segPath string) ([]byte, error) {
	tmp, err := os.CreateTemp("", "seg-*.mp4")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if err := catFiles(tmp.Name(), initPath, segPath); err != nil {
		return nil, err
	}
	tmp.Close()
	cmd := exec.Command("ffmpeg", "-v", "error", "-i", tmp.Name(),
		"-vn", "-ac", "1", "-ar", "24000", "-f", "f32le", "-")
	var out bytes.Buffer
	cmd.Stdout = &out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, stderr.String())
	}
	return out.Bytes(), nil
}
