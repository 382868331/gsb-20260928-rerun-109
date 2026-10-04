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
		p.nextSequence = latest + 1
		p.latestID = p.VersionID(latest)
		if note, err := s.ReadReleaseNote(p.latestID); err == nil {
			p.corrections = note.Correction
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
	note := p.buildPublicationNote(id, p.latestID, variants, store.ReleaseContent{
		Variant:  "",
		Segment:  segment,
		StartPTS: uint64(segment) * media.SegmentDuration,
		EndPTS:   uint64(segment+1) * media.SegmentDuration,
	})
	note.Kind = store.ReleasePublish
	note.Correction = p.corrections
	for i := range note.Changes {
		note.Changes[i].AdoptedFrom = id
	}
	if err := p.store.WriteStagingReleaseNote(id, note); err != nil {
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
func (p *Publisher) Correct(segmentIndex int, reasons ...string) (*store.Index, error) {
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
	reason := ""
	if len(reasons) > 0 {
		reason = reasons[0]
	}
	note := p.buildPublicationNote(id, latest.ID, variants, store.ReleaseContent{
		Variant:      "",
		Segment:      segmentIndex,
		StartPTS:     uint64(segmentIndex) * media.SegmentDuration,
		EndPTS:       uint64(segmentIndex+1) * media.SegmentDuration,
		BeforeSHA256: "",
		AfterSHA256:  "",
	})
	note.Kind = store.ReleaseCorrect
	note.Reason = reason
	note.Correction = correction
	p.fillChangeChecksums(&note, latest, &index, segmentIndex, id)
	if err := p.store.WriteStagingReleaseNote(id, note); err != nil {
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
	p.latestID = id
	p.nextSequence++
	p.corrections = correction
	if err := p.cleanup(&index); err != nil {
		return nil, err
	}
	return p.store.ReadIndex(id)
}

// WithdrawCorrection reverses the most recent correction decision for a
// segment by publishing a new immutable release whose segment content is
// adopted from that correction's parent. Old releases remain untouched.
func (p *Publisher) WithdrawCorrection(segmentIndex int, reasons ...string) (*store.Index, error) {
	if p.latestID == "" {
		return nil, fmt.Errorf("stream %s has no published version to withdraw from", p.stream)
	}
	current, err := p.store.ReadIndex(p.latestID)
	if err != nil {
		return nil, err
	}
	targetID, err := p.latestCorrectionID(segmentIndex)
	if err != nil {
		return nil, err
	}
	return p.withdrawID(current, segmentIndex, targetID, firstReason(reasons))
}

// Withdraw reverses a specific correction release. It is accepted only when
// that release is still the latest correction decision for its segment.
func (p *Publisher) Withdraw(correctionID string, reasons ...string) (*store.Index, error) {
	if p.latestID == "" {
		return nil, fmt.Errorf("stream %s has no published version to withdraw from", p.stream)
	}
	target, err := p.store.ReadReleaseNote(correctionID)
	if err != nil {
		return nil, err
	}
	if target.Kind != store.ReleaseCorrect || len(target.Changes) == 0 {
		return nil, fmt.Errorf("release %s is not a correction", correctionID)
	}
	segmentIndex := target.Changes[0].Segment
	latestCorrection, err := p.latestCorrectionID(segmentIndex)
	if err != nil {
		return nil, err
	}
	if latestCorrection != correctionID {
		return nil, fmt.Errorf("correction %s for segment %d is superseded by %s", correctionID, segmentIndex, latestCorrection)
	}
	current, err := p.store.ReadIndex(p.latestID)
	if err != nil {
		return nil, err
	}
	return p.withdrawID(current, segmentIndex, correctionID, firstReason(reasons))
}

func (p *Publisher) withdrawID(current *store.Index, segmentIndex int, correctionID, reason string) (*store.Index, error) {
	target, err := p.store.ReadReleaseNote(correctionID)
	if err != nil {
		return nil, err
	}
	if target.ParentID == "" {
		return nil, fmt.Errorf("correction %s has no parent release", correctionID)
	}
	restored, err := p.store.ReadIndex(target.ParentID)
	if err != nil {
		return nil, err
	}
	id := p.VersionID(p.nextSequence)
	if p.store.VersionExists(id) {
		return nil, fmt.Errorf("publication %s already exists", id)
	}
	variants, changed := p.variantsWithReplacement(current, restored, segmentIndex)
	if !changed {
		return nil, fmt.Errorf("segment %d is unavailable in release %s", segmentIndex, correctionID)
	}
	if _, err := p.store.Prepare(id, variants, p.withdrawCopyCallback(segmentIndex, target.ParentID)); err != nil {
		_ = p.store.Cancel(id)
		return nil, err
	}
	note := p.buildPublicationNote(id, current.ID, variants, store.ReleaseContent{
		Variant:  "",
		Segment:  segmentIndex,
		StartPTS: uint64(segmentIndex) * media.SegmentDuration,
		EndPTS:   uint64(segmentIndex+1) * media.SegmentDuration,
	})
	note.Kind = store.ReleaseWithdraw
	note.Reason = reason
	note.Correction = p.corrections
	note.WithdrawnID = correctionID
	p.fillChangeChecksums(&note, current, restored, segmentIndex, target.ParentID)
	if err := p.store.WriteStagingReleaseNote(id, note); err != nil {
		_ = p.store.Cancel(id)
		return nil, err
	}
	index := store.Index{WindowStart: current.WindowStart, Timeline: current.Timeline, Variants: variants}
	if err := p.store.Commit(id, index); err != nil {
		_ = p.store.Cancel(id)
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

func (p *Publisher) CurrentReleaseNote() (*store.ReleaseNote, error) {
	if p.latestID == "" {
		return nil, fmt.Errorf("stream %s has no published release", p.stream)
	}
	return p.store.ReadReleaseNote(p.latestID)
}

func (p *Publisher) ReleaseNote(id string) (*store.ReleaseNote, error) {
	return p.store.ReadReleaseNote(id)
}

func (p *Publisher) CompareReleases(fromID, toID string) (*store.ReleaseComparison, error) {
	return p.store.CompareReleases(fromID, toID)
}

func (p *Publisher) ReleaseNotes() ([]store.ReleaseNote, error) {
	entries, err := os.ReadDir(filepath.Join(p.store.Root, "publish", "versions"))
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			if _, ok := parseVersionID(entry.Name(), p.stream); ok {
				ids = append(ids, entry.Name())
			}
		}
	}
	sort.Strings(ids)
	notes := make([]store.ReleaseNote, 0, len(ids))
	for _, id := range ids {
		note, err := p.store.ReadReleaseNote(id)
		if err != nil {
			return nil, err
		}
		notes = append(notes, *note)
	}
	return notes, nil
}

func (p *Publisher) latestCorrectionID(segmentIndex int) (string, error) {
	return p.decisionForSegment(p.latestID, segmentIndex)
}

func (p *Publisher) decisionForSegment(id string, segmentIndex int) (string, error) {
	for id != "" {
		note, err := p.store.ReadReleaseNote(id)
		if err != nil {
			return "", err
		}
		applies := false
		for _, change := range note.Changes {
			if change.Segment == segmentIndex {
				applies = true
				break
			}
		}
		if applies && note.Kind == store.ReleaseCorrect {
			return id, nil
		}
		if applies && note.Kind == store.ReleaseWithdraw {
			target, err := p.store.ReadReleaseNote(note.WithdrawnID)
			if err != nil {
				return "", err
			}
			return p.decisionForSegment(target.ParentID, segmentIndex)
		}
		id = note.ParentID
	}
	return "", fmt.Errorf("segment %d has no active correction to withdraw", segmentIndex)
}

func (p *Publisher) variantsWithReplacement(current, restored *store.Index, segmentIndex int) ([]store.Variant, bool) {
	variants := make([]store.Variant, 0, len(current.Variants))
	changed := false
	for _, currentVariant := range current.Variants {
		segments := make([]store.Segment, 0, len(currentVariant.Segments))
		for _, segment := range currentVariant.Segments {
			if segment.Index == segmentIndex {
				for _, restoredVariant := range restored.Variants {
					if restoredVariant.Name != currentVariant.Name {
						continue
					}
					for _, restoredSegment := range restoredVariant.Segments {
						if restoredSegment.Index == segmentIndex {
							segment.SHA256 = restoredSegment.SHA256
							changed = true
						}
					}
				}
			}
			segments = append(segments, segment)
		}
		variants = append(variants, store.Variant{Name: currentVariant.Name, Width: currentVariant.Width, Height: currentVariant.Height, Segments: segments})
	}
	return variants, changed
}

func (p *Publisher) buildPublicationNote(id, parentID string, variants []store.Variant, template store.ReleaseContent) store.ReleaseNote {
	note := store.ReleaseNote{ID: id, ParentID: parentID, Changes: []store.ReleaseContent{}, Adoptions: []store.ReleaseContent{}}
	if len(variants) > 0 && len(variants[0].Segments) > 0 {
		note.WindowStart = variants[0].Segments[0].Start
		note.Timeline = variants[0].Segments[len(variants[0].Segments)-1].End
	}
	for _, variant := range variants {
		for _, segment := range variant.Segments {
			content := store.ReleaseContent{
				Variant:     variant.Name,
				Segment:     segment.Index,
				StartPTS:    segment.Start,
				EndPTS:      segment.End,
				AfterSHA256: segment.SHA256,
				AdoptedFrom: parentID,
			}
			if segment.Index == template.Segment && template.Variant == "" || content.Variant == template.Variant && content.Segment == template.Segment {
				if parentID == "" {
					content.AdoptedFrom = id
				}
				note.Changes = append(note.Changes, content)
			} else {
				note.Adoptions = append(note.Adoptions, content)
			}
		}
	}
	return note
}

func (p *Publisher) fillChangeChecksums(note *store.ReleaseNote, before, after *store.Index, segmentIndex int, adoptedFrom string) {
	beforeSums := map[string]string{}
	afterSums := map[string]string{}
	collect := func(index *store.Index) map[string]string {
		sums := map[string]string{}
		for _, variant := range index.Variants {
			for _, segment := range variant.Segments {
				if segment.Index == segmentIndex {
					sums[variant.Name] = segment.SHA256
				}
			}
		}
		return sums
	}
	beforeSums, afterSums = collect(before), collect(after)
	for i := range note.Changes {
		name := note.Changes[i].Variant
		note.Changes[i].BeforeSHA256 = beforeSums[name]
		note.Changes[i].AfterSHA256 = afterSums[name]
		note.Changes[i].AdoptedFrom = adoptedFrom
	}
}

func (p *Publisher) withdrawCopyCallback(restoredSegment int, restoredID string) func(string, int, string) error {
	return func(variantName string, segmentIndex int, destination string) error {
		sourceID := p.latestID
		if segmentIndex == restoredSegment {
			sourceID = restoredID
		}
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
		return fmt.Errorf("segment %d/%s unavailable", segmentIndex, variantName)
	}
}

func firstReason(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	return reasons[0]
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
