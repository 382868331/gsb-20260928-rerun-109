package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"localstream/internal/media"
)

const LayoutVersion = 1

type Segment struct {
	Index  int    `json:"index"`
	Start  uint64 `json:"start_pts"`
	End    uint64 `json:"end_pts"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Variant struct {
	Name     string    `json:"name"`
	Width    uint16    `json:"width"`
	Height   uint16    `json:"height"`
	Segments []Segment `json:"segments"`
}

type Index struct {
	Version     int       `json:"version"`
	ID          string    `json:"id"`
	WindowStart uint64    `json:"window_start_pts"`
	Timeline    uint64    `json:"timeline_duration"`
	Variants    []Variant `json:"variants"`
}

type ReleaseKind string

const (
	ReleasePublish     ReleaseKind = "publish"
	ReleaseCorrect     ReleaseKind = "correct"
	ReleaseWithdraw    ReleaseKind = "withdraw"
	ReleaseNoteName                = "release.json"
	ReleaseDecisionDir             = "decisions"
)

// ReleaseContent records one variant/segment content value in a release
// decision. BeforeSHA256 is empty for newly published segments. AdoptedFrom
// identifies the release that supplied AfterSHA256; for newly generated
// content it is the release itself.
type ReleaseContent struct {
	Variant      string `json:"variant"`
	Segment      int    `json:"segment"`
	StartPTS     uint64 `json:"start_pts"`
	EndPTS       uint64 `json:"end_pts"`
	BeforeSHA256 string `json:"before_sha256,omitempty"`
	AfterSHA256  string `json:"after_sha256"`
	AdoptedFrom  string `json:"adopted_from,omitempty"`
}

type ReleaseNote struct {
	ID          string           `json:"id"`
	ParentID    string           `json:"parent_id,omitempty"`
	Kind        ReleaseKind      `json:"kind"`
	Reason      string           `json:"reason,omitempty"`
	Correction  int              `json:"correction,omitempty"`
	WithdrawnID string           `json:"withdrawn_id,omitempty"`
	WindowStart uint64           `json:"window_start_pts"`
	Timeline    uint64           `json:"timeline_duration"`
	Changes     []ReleaseContent `json:"changes"`
	Adoptions   []ReleaseContent `json:"adoptions"`
}

type ReleaseVariantComparison struct {
	Name       string `json:"name"`
	FromSHA256 string `json:"from_sha256,omitempty"`
	ToSHA256   string `json:"to_sha256,omitempty"`
	Changed    bool   `json:"changed"`
}

type ReleaseSegmentComparison struct {
	Segment  int                        `json:"segment"`
	StartPTS uint64                     `json:"start_pts"`
	EndPTS   uint64                     `json:"end_pts"`
	Variants []ReleaseVariantComparison `json:"variants"`
}

type ReleaseComparison struct {
	FromID   string                     `json:"from_id"`
	ToID     string                     `json:"to_id"`
	Ranges   []Range                    `json:"ranges"`
	Segments []ReleaseSegmentComparison `json:"segments"`
}

// Range is a half-open PTS interval [Start,End) on the shared timeline.
type Range struct {
	Start uint64 `json:"start_pts"`
	End   uint64 `json:"end_pts"`
}

// DiffRanges returns the merged timeline ranges whose content differs
// between two published indexes, across every variant. It is used to tell
// a player which intervals changed when it adopts a corrected version.
func DiffRanges(before, after *Index) []Range {
	checksums := func(index *Index) map[string]map[int]string {
		byVariant := map[string]map[int]string{}
		for _, variant := range index.Variants {
			bySegment := map[int]string{}
			for _, segment := range variant.Segments {
				bySegment[segment.Index] = segment.SHA256
			}
			byVariant[variant.Name] = bySegment
		}
		return byVariant
	}
	oldSums, newSums := checksums(before), checksums(after)
	changed := map[int]bool{}
	for name, segments := range newSums {
		for segmentIndex, sum := range segments {
			if oldSums[name][segmentIndex] != sum {
				changed[segmentIndex] = true
			}
		}
	}
	for name, segments := range oldSums {
		for segmentIndex, sum := range segments {
			if newSums[name][segmentIndex] != sum {
				changed[segmentIndex] = true
			}
		}
	}
	indices := make([]int, 0, len(changed))
	for segmentIndex := range changed {
		indices = append(indices, segmentIndex)
	}
	sort.Ints(indices)
	ranges := make([]Range, 0, len(indices))
	for _, segmentIndex := range indices {
		start := uint64(segmentIndex) * media.SegmentDuration
		end := start + media.SegmentDuration
		if len(ranges) > 0 && ranges[len(ranges)-1].End == start {
			ranges[len(ranges)-1].End = end
			continue
		}
		ranges = append(ranges, Range{Start: start, End: end})
	}
	return ranges
}

// CompareReleases compares two immutable release decisions. Media versions may
// have been reclaimed, but decision records retain every per-segment checksum
// needed to explain historical differences.
func (s *Store) CompareReleases(fromID, toID string) (*ReleaseComparison, error) {
	from, err := s.ReadReleaseNote(fromID)
	if err != nil {
		return nil, err
	}
	to, err := s.ReadReleaseNote(toID)
	if err != nil {
		return nil, err
	}
	fromByVariant := map[string]map[int]Segment{}
	toByVariant := map[string]map[int]Segment{}
	segmentSet := map[int]bool{}
	variantSet := map[string]bool{}
	bounds := map[int]Range{}
	indexMaps := func(note *ReleaseNote) map[string]map[int]Segment {
		result := map[string]map[int]Segment{}
		for _, content := range append(append([]ReleaseContent{}, note.Changes...), note.Adoptions...) {
			variantSet[content.Variant] = true
			segmentSet[content.Segment] = true
			bounds[content.Segment] = Range{Start: content.StartPTS, End: content.EndPTS}
			if _, ok := result[content.Variant]; !ok {
				result[content.Variant] = map[int]Segment{}
			}
			result[content.Variant][content.Segment] = Segment{
				Index:  content.Segment,
				Start:  content.StartPTS,
				End:    content.EndPTS,
				SHA256: content.AfterSHA256,
			}
		}
		return result
	}
	fromByVariant = indexMaps(from)
	toByVariant = indexMaps(to)
	variants := make([]string, 0, len(variantSet))
	for name := range variantSet {
		variants = append(variants, name)
	}
	sort.Strings(variants)
	segments := make([]int, 0, len(segmentSet))
	for segment := range segmentSet {
		segments = append(segments, segment)
	}
	sort.Ints(segments)
	changedSegments := map[int]bool{}
	comparison := &ReleaseComparison{FromID: fromID, ToID: toID}
	for _, segmentIndex := range segments {
		item := ReleaseSegmentComparison{
			Segment:  segmentIndex,
			StartPTS: bounds[segmentIndex].Start,
			EndPTS:   bounds[segmentIndex].End,
		}
		for _, name := range variants {
			fromSegment := fromByVariant[name][segmentIndex]
			toSegment := toByVariant[name][segmentIndex]
			entry := ReleaseVariantComparison{Name: name, FromSHA256: fromSegment.SHA256, ToSHA256: toSegment.SHA256, Changed: fromSegment.SHA256 != toSegment.SHA256}
			if entry.Changed {
				changedSegments[segmentIndex] = true
			}
			item.Variants = append(item.Variants, entry)
		}
		comparison.Segments = append(comparison.Segments, item)
	}
	comparison = &ReleaseComparison{FromID: fromID, ToID: toID, Ranges: comparisonRanges(changedSegments, bounds), Segments: comparison.Segments}
	return comparison, nil
}

func comparisonRanges(changed map[int]bool, bounds map[int]Range) []Range {
	indices := make([]int, 0, len(changed))
	for segmentIndex := range changed {
		indices = append(indices, segmentIndex)
	}
	sort.Ints(indices)
	ranges := make([]Range, 0, len(indices))
	for _, segmentIndex := range indices {
		segmentRange := bounds[segmentIndex]
		if segmentRange.End == 0 {
			start := uint64(segmentIndex) * media.SegmentDuration
			segmentRange = Range{Start: start, End: start + media.SegmentDuration}
		}
		if len(ranges) > 0 && ranges[len(ranges)-1].End == segmentRange.Start {
			ranges[len(ranges)-1].End = segmentRange.End
			continue
		}
		ranges = append(ranges, segmentRange)
	}
	return ranges
}

type Retention struct {
	store *Store
	id    string
	token string
}

type Store struct{ Root string }

func Open(root string) *Store { return &Store{Root: root} }

func (s *Store) path(parts ...string) string {
	return filepath.Join(append([]string{s.Root}, parts...)...)
}

func (s *Store) Prepare(id string, variants []Variant, copySegment func(variantName string, segmentIndex int, destination string) error) (string, error) {
	id = filepath.Clean(id)
	if id == "." || strings.HasPrefix(id, "..") || filepath.IsAbs(id) || strings.Contains(id, string(os.PathSeparator)) {
		return "", errors.New("publication id must be a single safe path component")
	}
	if len(variants) < 2 {
		return "", errors.New("publication requires at least two variants")
	}
	staging := s.path("publish", "staging-"+id)
	if err := os.RemoveAll(staging); err != nil {
		return "", err
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}
	var windowStart, timeline uint64
	var haveBounds bool
	for _, variant := range variants {
		if err := validateVariant(variant); err != nil {
			return "", err
		}
		variantStart := variant.Segments[0].Start
		variantEnd := variant.Segments[len(variant.Segments)-1].End
		if !haveBounds {
			windowStart = variantStart
			timeline = variantEnd
			haveBounds = true
		} else if variantStart != windowStart || variantEnd != timeline {
			return "", errors.New("variants do not share the same timeline")
		}
		variantDir := filepath.Join(staging, variant.Name)
		if err := os.MkdirAll(variantDir, 0o755); err != nil {
			return "", err
		}
		for _, segment := range variant.Segments {
			name := filepath.Base(segment.Path)
			if name == "." || name == string(filepath.Separator) || filepath.Ext(name) != ".sm" {
				return "", fmt.Errorf("invalid segment path %q", segment.Path)
			}
			destination := filepath.Join(variantDir, name)
			if err := copySegment(variant.Name, segment.Index, destination); err != nil {
				return "", err
			}
			checksum, err := hashFile(destination)
			if err != nil {
				return "", err
			}
			if checksum != segment.SHA256 {
				return "", fmt.Errorf("checksum mismatch for %s/%d", variant.Name, segment.Index)
			}
		}
	}
	return staging, nil
}

func (s *Store) Commit(id string, index Index) error {
	staging := s.path("publish", "staging-"+id)
	versions := s.path("publish", "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		return err
	}
	index.Version = LayoutVersion
	index.ID = id
	if len(index.Variants) > 0 {
		index.WindowStart = index.Variants[0].Segments[0].Start
		index.Timeline = index.Variants[0].Segments[len(index.Variants[0].Segments)-1].End
	}
	indexBytes, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	indexPath := filepath.Join(staging, "index.json")
	if err := os.WriteFile(indexPath, append(indexBytes, '\n'), 0o644); err != nil {
		return err
	}
	next := filepath.Join(versions, id+"-next")
	if err := os.RemoveAll(next); err != nil {
		return err
	}
	if err := os.Rename(staging, next); err != nil {
		return err
	}
	committed := filepath.Join(versions, id)
	backup := filepath.Join(versions, id+"-old")
	_ = os.RemoveAll(backup)
	if _, err := os.Stat(committed); err == nil {
		if err := os.Rename(committed, backup); err != nil {
			return err
		}
	}
	if err := os.Rename(next, committed); err != nil {
		_ = os.Rename(backup, committed)
		return err
	}
	return os.RemoveAll(backup)
}

func (s *Store) Cancel(id string) error {
	return os.RemoveAll(s.path("publish", "staging-"+id))
}

func (s *Store) Current() (*Index, string, error) {
	if id, err := s.currentFromPointer(""); err == nil {
		index, err := s.ReadIndex(id)
		return index, id, err
	}
	publishDir := s.path("publish", "versions")
	entries, err := os.ReadDir(publishDir)
	if err != nil {
		return nil, "", err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasSuffix(entry.Name(), "-next") && !strings.HasSuffix(entry.Name(), "-old") {
			ids = append(ids, entry.Name())
		}
	}
	if len(ids) == 0 {
		return nil, "", errors.New("no published version")
	}
	sort.Strings(ids)
	id := ids[len(ids)-1]
	index, err := s.ReadIndex(id)
	return index, id, err
}

func (s *Store) CurrentForStream(stream string) (*Index, string, error) {
	if id, err := s.currentFromPointer(stream); err == nil {
		index, err := s.ReadIndex(id)
		return index, id, err
	} else if !os.IsNotExist(err) {
		return nil, "", err
	}
	return s.Current()
}

func (s *Store) SetCurrent(stream, id string) error {
	if stream != "" && (stream == "." || stream == ".." || filepath.Clean(stream) != stream || filepath.IsAbs(stream) || strings.Contains(stream, string(filepath.Separator))) {
		return errors.New("stream name must be a single safe path component")
	}
	dir := s.path("publish", "streams")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, stream)
	temporary, err := os.CreateTemp(dir, ".current-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	if _, err := temporary.WriteString(id); err != nil {
		temporary.Close()
		os.Remove(temporaryPath)
		return err
	}
	if err := temporary.Close(); err != nil {
		os.Remove(temporaryPath)
		return err
	}
	return os.Rename(temporaryPath, path)
}

func (s *Store) currentFromPointer(stream string) (string, error) {
	data, err := os.ReadFile(s.path("publish", "streams", stream))
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", errors.New("empty current publication pointer")
	}
	return id, nil
}

func (s *Store) ReadIndex(id string) (*Index, error) {
	data, err := os.ReadFile(s.path("publish", "versions", id, "index.json"))
	if err != nil {
		return nil, err
	}
	var index Index
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, err
	}
	return &index, nil
}

func (s *Store) WriteReleaseNote(note ReleaseNote) error {
	dir := s.path("publish", ReleaseDecisionDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(note, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, ".release-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		os.Remove(temporaryPath)
		return err
	}
	if err := temporary.Close(); err != nil {
		os.Remove(temporaryPath)
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(dir, note.ID+".json"))
}

func (s *Store) DeleteReleaseNote(id string) error {
	err := os.Remove(s.releaseNotePath(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Store) ReadReleaseNote(id string) (*ReleaseNote, error) {
	data, err := os.ReadFile(s.releaseNotePath(id))
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		data, err = os.ReadFile(s.path("publish", "versions", id, ReleaseNoteName))
		if err != nil {
			return nil, err
		}
	}
	var note ReleaseNote
	if err := json.Unmarshal(data, &note); err != nil {
		return nil, err
	}
	return &note, nil
}

func (s *Store) releaseNotePath(id string) string {
	return s.path("publish", ReleaseDecisionDir, id+".json")
}

func (s *Store) ListReleaseNoteIDs() ([]string, error) {
	entries, err := os.ReadDir(s.path("publish", ReleaseDecisionDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || strings.HasPrefix(entry.Name(), ".release-") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(entry.Name(), ".json"))
	}
	return ids, nil
}

// ContentOrigin follows AdoptedFrom links until it reaches the release that
// generated the requested variant/segment checksum.
func (s *Store) ContentOrigin(id, variantName string, segmentIndex int) (*ReleaseNote, ReleaseContent, error) {
	for {
		note, err := s.ReadReleaseNote(id)
		if err != nil {
			return nil, ReleaseContent{}, err
		}
		content, ok := releaseContent(note, variantName, segmentIndex)
		if !ok {
			return nil, ReleaseContent{}, fmt.Errorf("segment %d for variant %q is not in release %s", segmentIndex, variantName, id)
		}
		if content.AdoptedFrom == "" || content.AdoptedFrom == id {
			return note, content, nil
		}
		id = content.AdoptedFrom
	}
}

func releaseContent(note *ReleaseNote, variantName string, segmentIndex int) (ReleaseContent, bool) {
	for _, content := range note.Changes {
		if content.Variant == variantName && content.Segment == segmentIndex {
			return content, true
		}
	}
	for _, content := range note.Adoptions {
		if content.Variant == variantName && content.Segment == segmentIndex {
			return content, true
		}
	}
	return ReleaseContent{}, false
}

func (s *Store) SegmentPath(id, variant string, segment Segment) string {
	return s.path("publish", "versions", id, variant, filepath.Base(segment.Path))
}

func (s *Store) Retain(id string) (*Retention, error) {
	var tokenBytes [16]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(tokenBytes[:])
	dir := s.path("publish", "refs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, token), nil, 0o600); err != nil {
		return nil, err
	}
	return &Retention{store: s, id: id, token: token}, nil
}

func (r *Retention) Release() error {
	if r == nil || r.store == nil {
		return nil
	}
	err := os.Remove(r.store.path("publish", "refs", r.id, r.token))
	r.store = nil
	return err
}

func (s *Store) IsRetained(id string) (bool, error) {
	entries, err := os.ReadDir(s.path("publish", "refs", id))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

func (s *Store) RemoveVersion(id string) error {
	retained, err := s.IsRetained(id)
	if err != nil {
		return err
	}
	if retained {
		return nil
	}
	if err := os.RemoveAll(s.path("publish", "versions", id)); err != nil {
		return err
	}
	return os.RemoveAll(s.path("publish", "refs", id))
}

func (s *Store) VersionExists(id string) bool {
	_, err := os.Stat(s.path("publish", "versions", id))
	return err == nil
}

func SegmentFileName(index int) string { return fmt.Sprintf("%012d.sm", index) }

func validateVariant(v Variant) error {
	if v.Name == "" || len(v.Segments) == 0 {
		return errors.New("variant and complete segments are required")
	}
	var previousEnd uint64
	previousIndex := -1
	for i, segment := range v.Segments {
		if segment.Index < 0 || segment.Index <= previousIndex || (i > 0 && segment.Start != previousEnd) || segment.End <= segment.Start {
			return fmt.Errorf("variant %s is not contiguous at segment %d", v.Name, i)
		}
		if i > 0 && segment.Index != previousIndex+1 {
			return fmt.Errorf("variant %s is not contiguous at segment %d", v.Name, i)
		}
		if segment.SHA256 == "" {
			return errors.New("published segment requires a checksum")
		}
		wantStart := uint64(segment.Index) * media.SegmentDuration
		if segment.Start != wantStart || segment.End != segment.Start+media.SegmentDuration {
			return fmt.Errorf("variant %s segment %d has incorrect pts bounds", v.Name, segment.Index)
		}
		previousEnd = segment.End
		previousIndex = segment.Index
	}
	return nil
}

func hashFile(path string) (string, error) {
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
