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
	nextSequence   int
	latestID       string
	corrections    int
	notes          []ReleaseNote
	notesByID      map[string]int
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
	p.notesByID = map[string]int{}
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
		p.nextSequence = latest + 1
		p.latestID = p.VersionID(latest)
	}
	if err := p.loadReleaseNotes(); err != nil {
		return nil, err
	}
	for _, note := range p.notes {
		if note.Kind == ReleaseCorrection {
			p.corrections++
		}
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
	id := p.VersionID(p.nextSequence)
	parentID := p.latestID
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
	note := ReleaseNote{
		ID:       id,
		ParentID: parentID,
		Kind:     ReleaseAppend,
		Reason:   "append next live segment",
		Segments: []int{segment},
		Variants: affectedChecksums(nil, &index, map[int]bool{segment: true}, "added"),
		Contents: contentSnapshot(&index),
	}
	if err := p.appendReleaseNote(note); err != nil {
		return nil, err
	}
	if err := p.store.SetCurrent(p.stream, id); err != nil {
		return nil, err
	}
	p.latestID = id
	p.nextSequence++
	if err := p.cleanup(&index); err != nil {
		return nil, err
	}
	p.nextSegment++
	return p.store.ReadIndex(id)
}

// Correct republishes a single in-window segment with replacement content
// across every variant as one atomic decision. The corrected segments are
// generated, copied and verified for all variants before the new immutable
// version replaces the current pointer; on any failure the previous version
// stays fully intact and current.
func (p *Publisher) Correct(segmentIndex int, reason ...string) (*store.Index, error) {
	if p.latestID == "" {
		return nil, fmt.Errorf("stream %s has no published version to correct", p.stream)
	}
	latest, err := p.store.ReadIndex(p.latestID)
	if err != nil {
		return nil, err
	}
	first := int(latest.WindowStart / media.SegmentDuration)
	last := int(latest.Timeline/media.SegmentDuration) - 1
	if segmentIndex < first || segmentIndex > last {
		return nil, fmt.Errorf("segment %d is outside the correctable window [%d,%d]", segmentIndex, first, last)
	}
	id := p.VersionID(p.nextSequence)
	parentID := p.latestID
	if p.store.VersionExists(id) {
		return nil, fmt.Errorf("publication %s already exists", id)
	}
	correction := p.corrections + 1
	stagingSources := filepath.Join(p.store.Root, "live", p.stream, ".staging-correct-"+strconv.Itoa(segmentIndex))
	if err := os.MkdirAll(stagingSources, 0o755); err != nil {
		return nil, err
	}
	defer os.RemoveAll(stagingSources)
	checksums := map[string]string{}
	for _, spec := range media.Variants {
		sourcePath := filepath.Join(stagingSources, spec.Name+".sm")
		if err := media.WriteCorrectedSegment(sourcePath, spec, segmentIndex, correction); err != nil {
			return nil, err
		}
		checksum, err := fileSHA256(sourcePath)
		if err != nil {
			return nil, err
		}
		checksums[spec.Name] = checksum
	}
	variants := make([]store.Variant, 0, len(latest.Variants))
	for _, oldVariant := range latest.Variants {
		segments := make([]store.Segment, 0, len(oldVariant.Segments))
		for _, segment := range oldVariant.Segments {
			if segment.Index == segmentIndex {
				segment.SHA256 = checksums[oldVariant.Name]
			}
			segments = append(segments, segment)
		}
		variants = append(variants, store.Variant{Name: oldVariant.Name, Width: oldVariant.Width, Height: oldVariant.Height, Segments: segments})
	}
	index := store.Index{WindowStart: latest.WindowStart, Timeline: latest.Timeline, Variants: variants}
	if _, err := p.store.Prepare(id, variants, p.correctCopyCallback(segmentIndex, stagingSources)); err != nil {
		_ = p.store.Cancel(id)
		return nil, err
	}
	if err := p.store.Commit(id, index); err != nil {
		_ = p.store.Cancel(id)
		return nil, err
	}
	note := ReleaseNote{
		ID:       id,
		ParentID: parentID,
		Kind:     ReleaseCorrection,
		Reason:   releaseReason(reason),
		Segments: []int{segmentIndex},
		Variants: affectedChecksums(latest, &index, map[int]bool{segmentIndex: true}, "corrected"),
		Contents: contentSnapshot(&index),
	}
	if note.Reason == "" {
		note.Reason = "correct live content"
	}
	if err := p.appendReleaseNote(note); err != nil {
		return nil, err
	}
	if err := p.store.SetCurrent(p.stream, id); err != nil {
		return nil, err
	}
	p.latestID = id
	p.nextSequence++
	p.corrections = correction
	if err := p.cleanup(&index); err != nil {
		return nil, err
	}
	return p.store.ReadIndex(id)
}

// WithdrawCorrection republishes the content used immediately before a
// specific correction publication. The withdrawn correction remains an
// immutable publication and players holding it keep reading that version.
func (p *Publisher) WithdrawCorrection(correctionID string, reason ...string) (*store.Index, error) {
	target, err := p.correctionNote(correctionID)
	if err != nil {
		return nil, err
	}
	if len(target.Segments) != 1 {
		return nil, fmt.Errorf("release %s is not a single-segment correction", correctionID)
	}
	segmentIndex := target.Segments[0]
	if err := p.ensureCorrectionIsCurrent(target, segmentIndex); err != nil {
		return nil, err
	}
	current, err := p.store.ReadIndex(p.latestID)
	if err != nil {
		return nil, err
	}
	if target.ParentID == "" {
		return nil, fmt.Errorf("correction %s has no parent publication to restore", target.ID)
	}
	parent, err := p.store.ReadIndex(target.ParentID)
	if err != nil {
		return nil, err
	}
	first := int(current.WindowStart / media.SegmentDuration)
	last := int(current.Timeline/media.SegmentDuration) - 1
	if segmentIndex < first || segmentIndex > last {
		return nil, fmt.Errorf("segment %d is outside the current window [%d,%d]", segmentIndex, first, last)
	}
	id := p.VersionID(p.nextSequence)
	if p.store.VersionExists(id) {
		return nil, fmt.Errorf("publication %s already exists", id)
	}
	variants := make([]store.Variant, 0, len(current.Variants))
	restored := map[string]store.Segment{}
	for _, oldVariant := range parent.Variants {
		for _, segment := range oldVariant.Segments {
			if segment.Index == segmentIndex {
				restored[oldVariant.Name] = segment
			}
		}
	}
	for _, currentVariant := range current.Variants {
		segment, ok := restored[currentVariant.Name]
		if !ok {
			return nil, fmt.Errorf("segment %d/%s unavailable in %s", segmentIndex, currentVariant.Name, target.ParentID)
		}
		segments := make([]store.Segment, 0, len(currentVariant.Segments))
		for _, candidate := range currentVariant.Segments {
			if candidate.Index == segmentIndex {
				candidate.SHA256 = segment.SHA256
			}
			segments = append(segments, candidate)
		}
		variants = append(variants, store.Variant{Name: currentVariant.Name, Width: currentVariant.Width, Height: currentVariant.Height, Segments: segments})
	}
	index := store.Index{WindowStart: current.WindowStart, Timeline: current.Timeline, Variants: variants}
	if _, err := p.store.Prepare(id, variants, p.restoreCopyCallback(segmentIndex, target.ParentID)); err != nil {
		_ = p.store.Cancel(id)
		return nil, err
	}
	if err := p.store.Commit(id, index); err != nil {
		_ = p.store.Cancel(id)
		return nil, err
	}
	noteReason := releaseReason(reason)
	if noteReason == "" {
		noteReason = "withdraw live correction"
	}
	note := ReleaseNote{
		ID:          id,
		ParentID:    p.latestID,
		Kind:        ReleaseWithdrawal,
		Reason:      noteReason,
		Segments:    []int{segmentIndex},
		WithdrawsID: target.ID,
		Variants:    affectedChecksums(current, &index, map[int]bool{segmentIndex: true}, "restored"),
		Contents:    contentSnapshot(&index),
	}
	if err := p.appendReleaseNote(note); err != nil {
		return nil, err
	}
	if err := p.store.SetCurrent(p.stream, id); err != nil {
		return nil, err
	}
	p.latestID = id
	p.nextSequence++
	if err := p.cleanup(&index); err != nil {
		return nil, err
	}
	return p.store.ReadIndex(id)
}

func (p *Publisher) WithdrawSegmentCorrection(segmentIndex int, reason ...string) (*store.Index, error) {
	for i := len(p.notes) - 1; i >= 0; i-- {
		note := p.notes[i]
		if note.Kind != ReleaseCorrection {
			continue
		}
		for _, corrected := range note.Segments {
			if corrected == segmentIndex {
				return p.WithdrawCorrection(note.ID, reason...)
			}
		}
	}
	return nil, fmt.Errorf("no correction found for segment %d", segmentIndex)
}

func (p *Publisher) correctionNote(correctionID string) (ReleaseNote, error) {
	index, ok := p.notesByID[correctionID]
	if !ok {
		return ReleaseNote{}, fmt.Errorf("correction release %s not found", correctionID)
	}
	target := p.notes[index]
	if target.Kind != ReleaseCorrection {
		return ReleaseNote{}, fmt.Errorf("release %s is not a correction", correctionID)
	}
	return target, nil
}

func (p *Publisher) ensureCorrectionIsCurrent(target ReleaseNote, segmentIndex int) error {
	current, err := p.store.ReadIndex(p.latestID)
	if err != nil {
		return err
	}
	for _, currentVariant := range current.Variants {
		for _, currentSegment := range currentVariant.Segments {
			if currentSegment.Index != segmentIndex {
				continue
			}
			for _, targetVariant := range target.Variants {
				if targetVariant.Variant == currentVariant.Name && targetVariant.AfterSHA256 != currentSegment.SHA256 {
					return fmt.Errorf("correction %s is not the current content of segment %d", target.ID, segmentIndex)
				}
			}
		}
	}
	for _, note := range p.notes {
		if note.Kind == ReleaseWithdrawal && note.WithdrawsID == target.ID {
			return fmt.Errorf("correction %s was already withdrawn by %s", target.ID, note.ID)
		}
	}
	return nil
}

func (p *Publisher) restoreCopyCallback(restoredSegment int, sourceID string) func(string, int, string) error {
	return func(variantName string, segmentIndex int, destination string) error {
		index, err := p.store.ReadIndex(sourceID)
		if err != nil {
			return err
		}
		for _, variant := range index.Variants {
			if variant.Name != variantName {
				continue
			}
			for _, segment := range variant.Segments {
				if segment.Index == segmentIndex {
					return copyFile(p.store.SegmentPath(sourceID, variantName, segment), destination)
				}
			}
		}
		return fmt.Errorf("segment %d/%s unavailable in %s", segmentIndex, variantName, sourceID)
	}
}

func (p *Publisher) correctCopyCallback(correctedSegment int, stagingSources string) func(string, int, string) error {
	return func(variantName string, segmentIndex int, destination string) error {
		if segmentIndex == correctedSegment {
			return copyFile(filepath.Join(stagingSources, variantName+".sm"), destination)
		}
		index, err := p.store.ReadIndex(p.latestID)
		if err != nil {
			return err
		}
		for _, variant := range index.Variants {
			if variant.Name != variantName {
				continue
			}
			for _, segment := range variant.Segments {
				if segment.Index == segmentIndex {
					return copyFile(p.store.SegmentPath(p.latestID, variantName, segment), destination)
				}
			}
		}
		return fmt.Errorf("segment %d/%s unavailable", segmentIndex, variantName)
	}
}

func checksumForNewWindow(index int, currentChecksum string, currentSegment int) string {
	if index == currentSegment {
		return currentChecksum
	}
	return ""
}

func (p *Publisher) fillChecksums(variants []store.Variant, currentSegment int) error {
	var previous *store.Index
	if p.latestID != "" {
		var err error
		previous, err = p.store.ReadIndex(p.latestID)
		if err != nil {
			return err
		}
	} else if currentSegment > 0 {
		return fmt.Errorf("missing previous publication before segment %d", currentSegment)
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
		index, err := p.store.ReadIndex(p.latestID)
		if err != nil {
			return err
		}
		for _, variant := range index.Variants {
			if variant.Name != variantName {
				continue
			}
			for _, segment := range variant.Segments {
				if segment.Index == segmentIndex {
					return copyFile(p.store.SegmentPath(p.latestID, variantName, segment), destination)
				}
			}
		}
		return fmt.Errorf("segment %d/%s unavailable", segmentIndex, variantName)
	}
}

// cleanup removes versions that lie fully before the current window and are
// no longer referenced by any player. Versions still overlapping the window
// or held open by a player stay readable.
func (p *Publisher) cleanup(current *store.Index) error {
	entries, err := os.ReadDir(filepath.Join(p.store.Root, "publish", "versions"))
	if err != nil {
		return err
	}
	ids := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, ok := parseVersionID(entry.Name(), p.stream); ok && entry.Name() != current.ID {
			ids = append(ids, entry.Name())
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		index, err := p.store.ReadIndex(id)
		if err != nil {
			return err
		}
		if index.Timeline > current.WindowStart {
			continue
		}
		if err := p.store.RemoveVersion(id); err != nil {
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
