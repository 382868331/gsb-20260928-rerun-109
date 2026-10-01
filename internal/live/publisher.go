package live

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"localstream/internal/media"
	"localstream/internal/store"
)

const DefaultWindowSegments = 4

type Publisher struct {
	store          *store.Store
	stream         string
	windowSegments int
	nextSegment    int
}

func NewPublisher(root, stream string, windowSegments int) (*Publisher, error) {
	if !safeComponent(stream) {
		return nil, fmt.Errorf("invalid stream name %q", stream)
	}
	if windowSegments <= 0 {
		windowSegments = DefaultWindowSegments
	}
	s := store.Open(root)
	p := &Publisher{store: s, stream: stream, windowSegments: windowSegments}
	if entries, err := os.ReadDir(filepath.Join(root, "publish")); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				if versionID, ok := strings.CutPrefix(entry.Name(), "staging-"); ok {
					if _, ok := parseVersionID(versionID, stream); ok {
						if err := os.RemoveAll(filepath.Join(root, "publish", entry.Name())); err != nil {
							return nil, err
						}
					}
				}
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "publish", "versions"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	latest := -1
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if number, ok := parseVersionID(entry.Name(), stream); ok && number > latest {
			latest = number
		}
	}
	if latest >= 0 {
		index, err := s.ReadIndex(p.VersionID(latest))
		if err != nil {
			return nil, err
		}
		p.nextSegment = int(index.Timeline / media.SegmentDuration)
	}
	return p, nil
}

func parseVersionID(versionID, stream string) (int, bool) {
	prefix := stream + "-"
	suffix, ok := strings.CutPrefix(versionID, prefix)
	if !ok || len(suffix) != 20 {
		return 0, false
	}
	number, err := strconv.Atoi(suffix)
	return number, err == nil
}

func safeComponent(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Clean(value) == value && !filepath.IsAbs(value) && !strings.Contains(value, string(filepath.Separator))
}

func (p *Publisher) Stream() string   { return p.stream }
func (p *Publisher) NextSegment() int { return p.nextSegment }
func (p *Publisher) NextPTS() uint64  { return uint64(p.nextSegment) * media.SegmentDuration }
func (p *Publisher) VersionID(sequence int) string {
	return fmt.Sprintf("%s-%020d", p.stream, sequence)
}

func (p *Publisher) PublishNext() (*store.Index, error) {
	segment := p.nextSegment
	sequence := segment
	id := p.VersionID(sequence)
	if p.store.VersionExists(id) {
		return nil, fmt.Errorf("publication %s already exists", id)
	}
	variants := make([]store.Variant, 0, len(media.Variants))
	stagingSources := filepath.Join(p.store.Root, "live", p.stream, ".staging-"+strconv.Itoa(segment))
	if err := os.MkdirAll(stagingSources, 0o755); err != nil {
		return nil, err
	}
	defer os.RemoveAll(stagingSources)
	for _, spec := range media.Variants {
		sourcePath := filepath.Join(stagingSources, spec.Name+".sm")
		if err := media.WriteCompleteSegment(sourcePath, spec, segment); err != nil {
			return nil, err
		}
		checksum, err := fileSHA256(sourcePath)
		if err != nil {
			return nil, err
		}
		windowStartIndex := segment - p.windowSegments + 1
		if windowStartIndex < 0 {
			windowStartIndex = 0
		}
		segments := make([]store.Segment, 0, segment-windowStartIndex+1)
		for i := windowStartIndex; i <= segment; i++ {
			start := uint64(i) * media.SegmentDuration
			segments = append(segments, store.Segment{Index: i, Start: start, End: start + media.SegmentDuration, Path: store.SegmentFileName(i), SHA256: checksumForNewWindow(i, checksum, segment)})
		}
		variants = append(variants, store.Variant{Name: spec.Name, Width: spec.Width, Height: spec.Height, Segments: segments})
	}
	if err := p.fillChecksums(variants, segment); err != nil {
		_ = p.store.Cancel(id)
		return nil, err
	}
	index := store.Index{WindowStart: variants[0].Segments[0].Start, Timeline: uint64(segment+1) * media.SegmentDuration, Variants: variants}
	if _, err := p.store.Prepare(id, variants, p.copyCallback(segment, stagingSources)); err != nil {
		_ = p.store.Cancel(id)
		return nil, err
	}
	if err := p.store.Commit(id, index); err != nil {
		_ = p.store.Cancel(id)
		return nil, err
	}
	if err := p.store.SetCurrent(p.stream, id); err != nil {
		return nil, err
	}
	if err := p.cleanup(segment); err != nil {
		return nil, err
	}
	p.nextSegment++
	return p.store.ReadIndex(id)
}

func checksumForNewWindow(index int, currentChecksum string, currentSegment int) string {
	if index == currentSegment {
		return currentChecksum
	}
	return ""
}

func (p *Publisher) fillChecksums(variants []store.Variant, currentSegment int) error {
	previousID := p.VersionID(currentSegment - 1)
	previous, err := p.store.ReadIndex(previousID)
	if currentSegment > 0 && err != nil {
		return err
	}
	for variantIndex := range variants {
		for segmentIndex := range variants[variantIndex].Segments {
			segment := &variants[variantIndex].Segments[segmentIndex]
			if segment.SHA256 != "" {
				continue
			}
			found := false
			if previous != nil {
				for _, oldVariant := range previous.Variants {
					if oldVariant.Name != variants[variantIndex].Name {
						continue
					}
					for _, oldSegment := range oldVariant.Segments {
						if oldSegment.Index == segment.Index {
							segment.SHA256 = oldSegment.SHA256
							found = true
						}
					}
				}
			}
			if !found {
				return fmt.Errorf("missing retained segment %d for %s", segment.Index, variants[variantIndex].Name)
			}
		}
	}
	return nil
}

func (p *Publisher) copyCallback(currentSegment int, stagingSources string) func(string, int, string) error {
	return func(variantName string, segmentIndex int, destination string) error {
		if segmentIndex == currentSegment {
			return copyFile(filepath.Join(stagingSources, variantName+".sm"), destination)
		}
		id := p.VersionID(currentSegment - 1)
		index, err := p.store.ReadIndex(id)
		if err != nil {
			return err
		}
		for _, variant := range index.Variants {
			if variant.Name != variantName {
				continue
			}
			for _, segment := range variant.Segments {
				if segment.Index == segmentIndex {
					return copyFile(p.store.SegmentPath(id, variantName, segment), destination)
				}
			}
		}
		return fmt.Errorf("segment %d/%s unavailable", segmentIndex, variantName)
	}
}

func (p *Publisher) cleanup(currentSegment int) error {
	minKeep := currentSegment - p.windowSegments + 1
	entries, err := os.ReadDir(filepath.Join(p.store.Root, "publish", "versions"))
	if err != nil {
		return err
	}
	ids := make([]int, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if number, ok := parseVersionID(entry.Name(), p.stream); ok {
			ids = append(ids, number)
		}
	}
	sort.Ints(ids)
	for _, number := range ids {
		if number >= minKeep {
			break
		}
		if err := p.store.RemoveVersion(p.VersionID(number)); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
