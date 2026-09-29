package verify

import (
	"fmt"
	"math"
	"os"

	"streamcore/internal/fftool"
	"streamcore/internal/source"
)

func catFiles(dst string, parts ...string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	for _, p := range parts {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if _, err := out.Write(data); err != nil {
			return err
		}
	}
	return nil
}

func dims(path string) (int, int, error) {
	streams, _, err := fftool.Probe(path)
	if err != nil {
		return 0, 0, err
	}
	for _, s := range streams {
		if s.CodecType == "video" {
			return s.Width, s.Height, nil
		}
	}
	return 0, 0, fmt.Errorf("no video stream in %s", path)
}

func analyze(frameLuma []float64, x []float64, reads int) (*Result, error) {
	res := &Result{}
	// ---- Video: each frame is a uniform gray encoding source time ----
	frames := len(frameLuma)
	res.DecodedVideoFrames = frames
	for f, got := range frameLuma {
		t := float64(f) / source.VideoFPS
		want := source.LumaAt(t)
		if d := math.Abs(got - want); d > res.MaxLumaError {
			res.MaxLumaError = d
		}
	}
	if res.MaxLumaError > lumaTolerance {
		return nil, fmt.Errorf("video content mismatch: max luma error %.1f > %.1f", res.MaxLumaError, lumaTolerance)
	}
	if frames < source.VideoFPS*source.Duration {
		return nil, fmt.Errorf("missing video: decoded %d frames, want %d", frames, source.VideoFPS*source.Duration)
	}
	if frames > source.VideoFPS*source.Duration {
		return nil, fmt.Errorf("repeated video: decoded %d frames, want %d", frames, source.VideoFPS*source.Duration)
	}

	// ---- Audio: chirp instantaneous frequency per 100ms window ----
	n := len(x)
	res.DecodedAudioSamples = n
	minSamples := audioRate * source.Duration
	// AAC encoder priming adds a small lead; allow a bounded slack only.
	if n < minSamples-int(float64(audioRate)*0.05) {
		return nil, fmt.Errorf("missing audio: %d samples, want >= %d", n, minSamples-int(float64(audioRate)*0.05))
	}
	if n > minSamples+int(float64(audioRate)*0.05) {
		return nil, fmt.Errorf("repeated audio: %d samples, want <= %d", n, minSamples+int(float64(audioRate)*0.05))
	}
	// Drop the final partial/padded window: AAC encoder padding at stream end
	// is silence and would distort the zero-crossing estimate.
	wins := n / windowSamples
	var freqs []float64
	for wn := 1; wn < wins-1; wn++ {
		w := x[wn*windowSamples : (wn+1)*windowSamples]
		f := estimateFreq(w, audioRate)
		freqs = append(freqs, f)
		t := (float64(wn) + 0.5) * float64(windowSamples) / audioRate
		want := chirpFreqAt(t)
		if d := math.Abs(f - want); d > res.MaxFreqError {
			res.MaxFreqError = d
		}
	}
	if res.MaxFreqError > freqToleranceHz {
		return nil, fmt.Errorf("audio content discontinuity: max chirp freq error %.1f Hz > %.1f", res.MaxFreqError, freqToleranceHz)
	}
	// Boundary checks at each 2s read join: compare adjacent windows across it.
	for b := 1; b < reads; b++ {
		sampleAt := b * 2 * audioRate / windowSamples // window index at 2s joins
		if sampleAt < 1 || sampleAt >= len(freqs) {
			continue
		}
		tAt := float64(b) * 2
		res.Boundaries = append(res.Boundaries, BoundaryCheck{
			At:             tAt,
			FreqBefore:     freqs[sampleAt-1],
			FreqAfter:      freqs[sampleAt],
			ExpectedBefore: chirpFreqAt(tAt - 0.05),
			ExpectedAfter:  chirpFreqAt(tAt + 0.05),
		})
	}
	return res, nil
}

// estimateFreq estimates a single sinusoid frequency by zero-crossing rate,
// adequate for the strong 440-range chirp.
func estimateFreq(x []float64, rate int) float64 {
	crossings := 0
	for i := 1; i < len(x); i++ {
		if (x[i-1] < 0 && x[i] >= 0) || (x[i-1] >= 0 && x[i] < 0) {
			crossings++
		}
	}
	dur := float64(len(x)) / float64(rate)
	return float64(crossings) / 2 / dur
}

func chirpFreqAt(t float64) float64 {
	if t < 0 {
		t = 0
	}
	if t > source.Duration {
		t = source.Duration
	}
	return source.ChirpStart + (source.ChirpEnd-source.ChirpStart)*t/float64(source.Duration)
}
