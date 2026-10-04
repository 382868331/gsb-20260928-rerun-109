package live

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"localstream/internal/media"
	"localstream/internal/store"
)

const DefaultWindowSegments = 4

var ErrCorrectionExpired = errors.New("correction target has left the live window")

type Publisher struct {
	store          *store.Store
	stream         string
	windowSegments int
	nextSegment    int
	nextVersion    int
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
		name := strings.TrimSuffix(strings.TrimSuffix(entry.Name(), "-next"), "-old")
		if number, ok := parseVersionID(name, stream); ok && number > latest {
			latest = number
		}
	}
	if latest >= 0 {
		p.nextVersion = latest + 1
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
	id := p.VersionID(p.nextVersion)
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
		variant := store.Variant{Name: spec.Name, Width: spec.Width, Height: spec.Height}
		variant.Segments = p.baseSegments(spec.Name, segment)
		variant.Segments = append(variant.Segments, store.Segment{
			Index: segment, Revision: 0,
			Start: uint64(segment) * media.SegmentDuration,
			End:   uint64(segment+1) * media.SegmentDuration,
			Path:  store.SegmentFileName(segment), SHA256: checksum,
		})
		variants = append(variants, variant)
	}
	baseID, _ := p.currentID()
	if _, err := p.store.Prepare(id, variants, p.copyCallback(baseID, segment, stagingSources, nil)); err != nil {
		_ = p.store.Cancel(id)
		return nil, err
	}
	if err := p.store.Commit(id, store.Index{Variants: variants}); err != nil {
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
	p.nextVersion++
	return p.store.ReadIndex(id)
}

func (p *Publisher) currentID() (string, *store.Index) {
	index, id, err := p.store.CurrentForStream(p.stream)
	if err != nil {
		return "", nil
	}
	return id, index
}

// baseSegments copies the current window for a variant, retaining the stored
// revision and checksum for every already published segment.
func (p *Publisher) baseSegments(variantName string, appendSegment int) []store.Segment {
	_, current := p.currentID()
	if current == nil {
		return nil
	}
	windowStart := appendSegment - p.windowSegments + 1
	for _, variant := range current.Variants {
		if variant.Name != variantName {
			continue
		}
		segments := make([]store.Segment, 0, len(variant.Segments))
		for _, segment := range variant.Segments {
			if segment.Index < windowStart {
				continue
			}
			if appendSegment >= 0 && segment.Index > appendSegment {
				continue
			}
			segments = append(segments, store.Segment{
				Index: segment.Index, Revision: segment.Revision,
				Start: segment.Start, End: segment.End,
				Path: store.SegmentFileName(segment.Index), SHA256: segment.SHA256,
			})
		}
		return segments
	}
	return nil
}

// PublishCorrection replaces one segment that is still inside the live window.
// New content is produced for every quality variant from the same decision:
// the staging copy and checksum verification must succeed for all variants
// before the new version and stream pointer are committed, so a failure leaves
// the complete previous version playable.
func (p *Publisher) PublishCorrection(correction media.Correction) (*store.Index, error) {
	segment := correction.Segment
	if segment < 0 {
		return nil, fmt.Errorf("correction segment %d is invalid", segment)
	}
	baseID, current := p.currentID()
	if current == nil {
		return nil, fmt.Errorf("no published version available for correction")
	}
	if segment < int(current.WindowStart/media.SegmentDuration) || segment >= int(current.Timeline/media.SegmentDuration) {
		return nil, fmt.Errorf("correction segment %d is outside window [%d,%d): %w",
			segment, current.WindowStart, current.Timeline, ErrCorrectionExpired)
	}
	currentRevision := 0
	for _, variant := range current.Variants {
		for _, candidate := range variant.Segments {
			if candidate.Index == segment && candidate.Revision > currentRevision {
				currentRevision = candidate.Revision
			}
		}
	}
	revision := correction.Revision
	if revision <= currentRevision {
		revision = currentRevision + 1
	}
	id := p.VersionID(p.nextVersion)
	if p.store.VersionExists(id) {
		return nil, fmt.Errorf("publication %s already exists", id)
	}
	stagingSources := filepath.Join(p.store.Root, "live", p.stream, ".staging-correction-"+strconv.Itoa(segment)+"-r"+strconv.Itoa(revision))
	if err := os.MkdirAll(stagingSources, 0o755); err != nil {
		return nil, err
	}
	defer os.RemoveAll(stagingSources)
	replacement := media.Correction{Segment: segment, Revision: revision, AudioGain: correction.AudioGain, VideoTint: correction.VideoTint}
	variants := make([]store.Variant, 0, len(media.Variants))
	for _, spec := range media.Variants {
		sourcePath := filepath.Join(stagingSources, spec.Name+".sm")
		if err := media.WriteCorrectedSegment(sourcePath, spec, replacement); err != nil {
			_ = p.store.Cancel(id)
			return nil, err
		}
		checksum, err := fileSHA256(sourcePath)
		if err != nil {
			_ = p.store.Cancel(id)
			return nil, err
		}
		variant := store.Variant{Name: spec.Name, Width: spec.Width, Height: spec.Height, Segments: p.baseSegments(spec.Name, -1)}
		if len(variant.Segments) == 0 {
			_ = p.store.Cancel(id)
			return nil, fmt.Errorf("variant %s has no window segments", spec.Name)
		}
		for i := range variant.Segments {
			if variant.Segments[i].Index == segment {
				variant.Segments[i].Revision = revision
				variant.Segments[i].SHA256 = checksum
			}
		}
		variants = append(variants, variant)
	}
	if _, err := p.store.Prepare(id, variants, p.copyCallback(baseID, segment, stagingSources, map[int]bool{segment: true})); err != nil {
		_ = p.store.Cancel(id)
		return nil, err
	}
	if err := p.store.Commit(id, store.Index{Variants: variants}); err != nil {
		_ = p.store.Cancel(id)
		return nil, err
	}
	if err := p.store.SetCurrent(p.stream, id); err != nil {
		return nil, err
	}
	if err := p.cleanup(p.nextSegment - 1); err != nil {
		return nil, err
	}
	p.nextVersion++
	return p.store.ReadIndex(id)
}

func (p *Publisher) copyCallback(baseID string, liveSegment int, stagingSources string, corrected map[int]bool) func(string, int, string) error {
	return func(variantName string, segmentIndex int, destination string) error {
		source := filepath.Join(stagingSources, variantName+".sm")
		if (segmentIndex == liveSegment && liveSegment >= 0) || corrected[segmentIndex] {
			return copyFile(source, destination)
		}
		index, err := p.store.ReadIndex(baseID)
		if err != nil {
			return err
		}
		for _, variant := range index.Variants {
			if variant.Name != variantName {
				continue
			}
			for _, segment := range variant.Segments {
				if segment.Index == segmentIndex {
					return copyFile(p.store.SegmentPath(baseID, variantName, segment), destination)
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
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := strings.TrimSuffix(strings.TrimSuffix(entry.Name(), "-next"), "-old")
		if _, ok := parseVersionID(name, p.stream); !ok {
			continue
		}
		index, err := p.store.ReadIndex(name)
		if err != nil {
			return err
		}
		lastSegment := int(index.Timeline/media.SegmentDuration) - 1
		if lastSegment >= minKeep {
			continue
		}
		if err := p.store.RemoveVersion(name); err != nil {
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
