package indexer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"localstream/internal/media"
)

type SegmentInfo struct {
	Index  int    `json:"index"`
	Start  uint64 `json:"start_pts"`
	End    uint64 `json:"end_pts"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type VariantIndex struct {
	Name     string        `json:"name"`
	Width    uint16        `json:"width"`
	Height   uint16        `json:"height"`
	Segments []SegmentInfo `json:"segments"`
}

type SourceIndex struct {
	Variants []VariantIndex `json:"variants"`
}

func Scan(root string) (SourceIndex, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return SourceIndex{}, err
	}
	result := SourceIndex{}
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) > 0 && entry.Name()[0] == '.' {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, entry.Name()))
		if err != nil {
			return SourceIndex{}, err
		}
		var segments []SegmentInfo
		for _, file := range files {
			name := file.Name()
			if filepath.Ext(name) != ".sm" || !file.Type().IsRegular() {
				continue
			}
			var index int
			if _, err := fmt.Sscanf(name, "%06d.sm", &index); err != nil {
				continue
			}
			path := filepath.Join(root, entry.Name(), name)
			f, err := os.Open(path)
			if err != nil {
				return SourceIndex{}, err
			}
			header, packets, decodeErr := media.Decode(f)
			f.Close()
			if decodeErr != nil {
				return SourceIndex{}, fmt.Errorf("decode %s: %w", path, decodeErr)
			}
			if err := verifyContent(entry.Name(), header, packets); err != nil {
				return SourceIndex{}, fmt.Errorf("content mismatch in %s: %w", path, err)
			}
			checksum, err := checksum(path)
			if err != nil {
				return SourceIndex{}, err
			}
			segments = append(segments, SegmentInfo{Index: index, Start: header.StartPTS, End: header.StartPTS + header.Duration, Path: path, SHA256: checksum})
		}
		sort.Slice(segments, func(i, j int) bool { return segments[i].Index < segments[j].Index })
		for i := range segments {
			if segments[i].Index != i {
				return SourceIndex{}, fmt.Errorf("variant %s has missing or unfinished segment %d", entry.Name(), i)
			}
		}
		if len(segments) == 0 {
			continue
		}
		result.Variants = append(result.Variants, VariantIndex{Name: entry.Name(), Width: 0, Height: 0, Segments: segments})
	}
	sort.Slice(result.Variants, func(i, j int) bool { return result.Variants[i].Name < result.Variants[j].Name })
	return result, nil
}

func verifyContent(variantName string, header media.Header, packets []media.Packet) error {
	spec := media.VariantSpec{}
	found := false
	for _, candidate := range media.Variants {
		if candidate.Name == variantName {
			spec, found = candidate, true
		}
	}
	if !found {
		return fmt.Errorf("unknown variant %s", variantName)
	}
	if header.Width != spec.Width || header.Height != spec.Height {
		return fmt.Errorf("dimension %dx%d does not match %s", header.Width, header.Height, spec.Name)
	}
	for _, packet := range packets {
		if packet.Type != media.VideoPacket {
			continue
		}
		frame := int(packet.PTS / media.FrameDuration)
		for y := range int(header.Height) {
			for x := range int(header.Width) {
				offset := (y*int(header.Width) + x) * 3
				r, g, b := media.SourceFrame(frame, int(header.Width), int(header.Height), x, y)
				if packet.Video[offset] != r || packet.Video[offset+1] != g || packet.Video[offset+2] != b {
					return fmt.Errorf("video pixel %d,%d differs at frame %d", x, y, frame)
				}
			}
		}
	}
	return nil
}

func Write(path string, index SourceIndex) error {
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func checksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := newSHA256()
	if _, err := copyHash(h, f); err != nil {
		return "", err
	}
	return sumHex(h), nil
}
