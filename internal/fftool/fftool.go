// Package fftool wraps the ffmpeg/ffprobe CLIs used to encode the two
// renditions and to actually decode segments for verification.
package fftool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
)

func Run(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %v: %w\n%s", name, args, err, stderr.Bytes())
	}
	return out, nil
}

// Frame is one decoded video frame reported by ffprobe showframes.
type Frame struct {
	MediaType       string `json:"media_type"`
	KeyFrame        int    `json:"key_frame"`
	PTS             int64  `json:"pts"`
	PTSTime         string `json:"pts_time"`
	BestEffortTime  string `json:"best_effort_timestamp_time"`
	Width           int    `json:"width"`
	Height          int    `json:"height"`
	PixFmt          string `json:"pix_fmt"`
	PktDuration     int64  `json:"pkt_duration"`
	PktDurationTime string `json:"pkt_duration_time"`
	NBSamples       int    `json:"nb_samples"`
}

// Stream describes one stream from ffprobe.
type Stream struct {
	Index      int    `json:"index"`
	CodecType  string `json:"codec_type"`
	CodecName  string `json:"codec_name"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	SampleRate string `json:"sample_rate"`
	Channels   int    `json:"channels"`
	RFrameRate string `json:"r_frame_rate"`
	TimeBase   string `json:"time_base"`
	Duration   string `json:"duration"`
}

type probeResult struct {
	Streams []Stream `json:"streams"`
	Frames  []Frame  `json:"frames"`
}

// Probe runs ffprobe on a file and returns streams and per-frame records.
func Probe(path string) ([]Stream, []Frame, error) {
	out, err := Run("ffprobe", "-v", "error",
		"-show_streams", "-show_frames",
		"-print_format", "json", path)
	if err != nil {
		return nil, nil, err
	}
	var res probeResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, nil, fmt.Errorf("parse ffprobe json: %w", err)
	}
	return res.Streams, res.Frames, nil
}

// FrameTimes returns PTS times (seconds) for video or audio frames.
func FrameTimes(frames []Frame, mediaType string) []float64 {
	var times []float64
	for _, fr := range frames {
		if fr.MediaType != mediaType {
			continue
		}
		s := fr.PTSTime
		if s == "N/A" || s == "" {
			s = fr.BestEffortTime
		}
		var v float64
		if _, err := fmt.Sscanf(s, "%f", &v); err == nil {
			times = append(times, v)
		}
	}
	return times
}
