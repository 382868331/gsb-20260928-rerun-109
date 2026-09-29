package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	Magic       = "LSM1"
	VideoPacket = 1
	AudioPacket = 2
	TimeScale   = uint32(48000)
)

type Header struct {
	Width       uint16
	Height      uint16
	AudioRate   uint32
	Channels    uint16
	TimeScale   uint32
	StartPTS    uint64
	Duration    uint64
	VideoFrames uint32
	AudioFrames uint32
}

type Packet struct {
	Type     byte
	KeyFrame bool
	PTS      uint64
	Duration uint32
	Video    []byte
	Audio    []byte
}

type Writer struct {
	w      io.Writer
	header Header
}

func NewWriter(w io.Writer, header Header) *Writer {
	header.TimeScale = TimeScale
	return &Writer{w: w, header: header}
}

func (w *Writer) Write(p []byte) (int, error) { return w.w.Write(p) }

func (w *Writer) WriteHeader(videoFrames, audioFrames uint32) error {
	w.header.VideoFrames = videoFrames
	w.header.AudioFrames = audioFrames
	buf := make([]byte, 0, 43)
	buf = append(buf, Magic...)
	buf = append(buf, 1)
	buf = binary.BigEndian.AppendUint16(buf, w.header.Width)
	buf = binary.BigEndian.AppendUint16(buf, w.header.Height)
	buf = binary.BigEndian.AppendUint32(buf, w.header.AudioRate)
	buf = binary.BigEndian.AppendUint16(buf, w.header.Channels)
	buf = binary.BigEndian.AppendUint32(buf, w.header.TimeScale)
	buf = binary.BigEndian.AppendUint64(buf, w.header.StartPTS)
	buf = binary.BigEndian.AppendUint64(buf, w.header.Duration)
	buf = binary.BigEndian.AppendUint32(buf, videoFrames)
	buf = binary.BigEndian.AppendUint32(buf, audioFrames)
	_, err := w.Write(buf)
	return err
}

func (w *Writer) WritePacket(p Packet) error {
	payload := p.Video
	if p.Type == AudioPacket {
		payload = p.Audio
	}
	if payload == nil {
		return errors.New("media packet payload is empty")
	}
	header := []byte{p.Type, 0}
	if p.KeyFrame {
		header[1] = 1
	}
	header = binary.BigEndian.AppendUint64(header, p.PTS)
	header = binary.BigEndian.AppendUint32(header, p.Duration)
	header = binary.BigEndian.AppendUint32(header, uint32(len(payload)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func Decode(r io.Reader) (Header, []Packet, error) {
	var fixed [43]byte
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
		return Header{}, nil, err
	}
	if got := string(fixed[0:4]); got != Magic {
		return Header{}, nil, fmt.Errorf("unexpected media magic %q", got)
	}
	if fixed[4] != 1 {
		return Header{}, nil, fmt.Errorf("unsupported media version %d", fixed[4])
	}
	h := Header{
		Width:       binary.BigEndian.Uint16(fixed[5:7]),
		Height:      binary.BigEndian.Uint16(fixed[7:9]),
		AudioRate:   binary.BigEndian.Uint32(fixed[9:13]),
		Channels:    binary.BigEndian.Uint16(fixed[13:15]),
		TimeScale:   binary.BigEndian.Uint32(fixed[15:19]),
		StartPTS:    binary.BigEndian.Uint64(fixed[19:27]),
		Duration:    binary.BigEndian.Uint64(fixed[27:35]),
		VideoFrames: binary.BigEndian.Uint32(fixed[35:39]),
		AudioFrames: binary.BigEndian.Uint32(fixed[39:43]),
	}
	packets := make([]Packet, 0, h.VideoFrames+h.AudioFrames)
	for range h.VideoFrames + h.AudioFrames {
		var packetHeader [18]byte
		if _, err := io.ReadFull(r, packetHeader[:]); err != nil {
			return h, nil, err
		}
		size := binary.BigEndian.Uint32(packetHeader[14:18])
		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return h, nil, err
		}
		p := Packet{
			Type:     packetHeader[0],
			KeyFrame: packetHeader[1] == 1,
			PTS:      binary.BigEndian.Uint64(packetHeader[2:10]),
			Duration: binary.BigEndian.Uint32(packetHeader[10:14]),
		}
		switch p.Type {
		case VideoPacket:
			p.Video = payload
		case AudioPacket:
			p.Audio = payload
		default:
			return h, nil, fmt.Errorf("unknown packet type %d", p.Type)
		}
		packets = append(packets, p)
	}
	var trailer [1]byte
	if _, err := io.ReadFull(r, trailer[:]); err != nil {
		return h, nil, err
	}
	if trailer[0] != 0 {
		return h, nil, errors.New("invalid segment end marker")
	}
	return h, packets, Validate(h, packets)
}

func Validate(h Header, packets []Packet) error {
	if h.TimeScale != TimeScale || h.AudioRate != TimeScale {
		return fmt.Errorf("unsupported timescale: header=%d audio=%d", h.TimeScale, h.AudioRate)
	}
	if len(packets) != int(h.VideoFrames+h.AudioFrames) {
		return fmt.Errorf("packet count %d does not match header %d", len(packets), h.VideoFrames+h.AudioFrames)
	}
	var videoSeen, audioSeen uint32
	var nextVideoPTS, nextAudioPTS = h.StartPTS, h.StartPTS
	for _, p := range packets {
		switch p.Type {
		case VideoPacket:
			if !p.KeyFrame {
				return errors.New("video packet is not independently decodable")
			}
			if p.PTS != nextVideoPTS {
				return fmt.Errorf("video gap or overlap at pts %d, expected %d", p.PTS, nextVideoPTS)
			}
			expected := int(h.Width) * int(h.Height) * 3
			if len(p.Video) != expected {
				return fmt.Errorf("video payload %d does not match %dx%d", len(p.Video), h.Width, h.Height)
			}
			nextVideoPTS += uint64(p.Duration)
			videoSeen++
		case AudioPacket:
			if p.PTS != nextAudioPTS {
				return fmt.Errorf("audio gap or overlap at pts %d, expected %d", p.PTS, nextAudioPTS)
			}
			expected := int(p.Duration) * int(h.Channels) * 2
			if len(p.Audio) != expected {
				return fmt.Errorf("audio payload %d does not match duration %d", len(p.Audio), p.Duration)
			}
			nextAudioPTS += uint64(p.Duration)
			audioSeen++
		}
	}
	if videoSeen != h.VideoFrames || audioSeen != h.AudioFrames {
		return errors.New("audio/video packet counts do not match header")
	}
	if nextVideoPTS != h.StartPTS+h.Duration || nextAudioPTS != h.StartPTS+h.Duration {
		return errors.New("audio and video timelines do not end together")
	}
	return nil
}
