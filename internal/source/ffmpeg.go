package source

import "streamcore/internal/fftool"

func mux(m *Media) error {
	// Video: y4m (10 fps, luma-encoded time). Audio: 24 kHz mono PCM.
	// Keyframes every 1 s so the two-tier cut points at 2 s align.
	args := []string{"-y",
		"-i", m.VideoY4M,
		"-i", m.AudioWAV,
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-g", "10", "-keyint_min", "10", "-bf", "0",
		"-c:a", "aac", "-b:a", "64k", "-ar", "24000", "-ac", "1",
		"-movflags", "+faststart",
		m.MuxedMP4,
	}
	_, err := fftool.Run("ffmpeg", args...)
	return err
}
