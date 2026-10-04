package live

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"localstream/internal/media"
	"localstream/internal/store"
)

const (
	ReleaseAppend     = "append"
	ReleaseCorrection = "correction"
	ReleaseWithdrawal = "withdrawal"
)

type VariantChecksum struct {
	Variant      string `json:"variant"`
	BeforeSHA256 string `json:"before_sha256,omitempty"`
	AfterSHA256  string `json:"after_sha256"`
	Relation     string `json:"relation"`
}

type SegmentChecksum struct {
	Variant string `json:"variant"`
	Index   int    `json:"segment"`
	SHA256  string `json:"sha256"`
}

// ReleaseNote is the operator-visible, immutable decision for one
// publication. Contents retains a complete checksum snapshot so decisions
// remain traceable after ordinary window cleanup removes old media files.
type ReleaseNote struct {
	ID          string            `json:"id"`
	ParentID    string            `json:"parent_id,omitempty"`
	Kind        string            `json:"kind"`
	Reason      string            `json:"reason,omitempty"`
	Segments    []int             `json:"segments,omitempty"`
	WithdrawsID string            `json:"withdraws_id,omitempty"`
	Variants    []VariantChecksum `json:"variants,omitempty"`
	Contents    []SegmentChecksum `json:"contents"`
}

type SegmentComparison struct {
	Index      int    `json:"index"`
	Variant    string `json:"variant"`
	FromSHA256 string `json:"from_sha256"`
	ToSHA256   string `json:"to_sha256"`
	Changed    bool   `json:"changed"`
}

type ReleaseComparison struct {
	FromID   string              `json:"from_id"`
	ToID     string              `json:"to_id"`
	Ranges   []store.Range       `json:"ranges"`
	Segments []SegmentComparison `json:"segments"`
}

type releaseJournal struct {
	Stream   string        `json:"stream"`
	Releases []ReleaseNote `json:"releases"`
}

func (p *Publisher) releaseLogPath() string {
	return filepath.Join(p.store.Root, "publish", "streams", p.stream+"-releases.json")
}

func (p *Publisher) loadReleaseNotes() error {
	p.notes = nil
	p.notesByID = map[string]int{}
	data, err := os.ReadFile(p.releaseLogPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var journal releaseJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return err
	}
	if journal.Stream != p.stream {
		return fmt.Errorf("release log stream %q does not match %q", journal.Stream, p.stream)
	}
	for i := range journal.Releases {
		note := journal.Releases[i]
		p.notesByID[note.ID] = i
	}
	p.notes = journal.Releases
	return nil
}

func (p *Publisher) appendReleaseNote(note ReleaseNote) error {
	if _, exists := p.notesByID[note.ID]; exists {
		return fmt.Errorf("release note %s already exists", note.ID)
	}
	p.notes = append(p.notes, note)
	p.notesByID[note.ID] = len(p.notes) - 1
	return p.saveReleaseNotes()
}

func (p *Publisher) saveReleaseNotes() error {
	path := p.releaseLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".releases-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	encodeErr := json.NewEncoder(temporary).Encode(releaseJournal{Stream: p.stream, Releases: p.notes})
	closeErr := temporary.Close()
	if encodeErr != nil {
		os.Remove(temporaryPath)
		return encodeErr
	}
	if closeErr != nil {
		os.Remove(temporaryPath)
		return closeErr
	}
	return os.Rename(temporaryPath, path)
}

func (p *Publisher) ReleaseNotes() []ReleaseNote {
	notes := make([]ReleaseNote, len(p.notes))
	copy(notes, p.notes)
	return notes
}

func (p *Publisher) ReleaseNote(id string) (ReleaseNote, error) {
	index, ok := p.notesByID[id]
	if !ok {
		return ReleaseNote{}, fmt.Errorf("release %s not found", id)
	}
	return p.notes[index], nil
}

func (p *Publisher) CurrentReleaseNote() (ReleaseNote, error) {
	if p.latestID == "" {
		return ReleaseNote{}, fmt.Errorf("stream %s has no published release", p.stream)
	}
	return p.ReleaseNote(p.latestID)
}

func releaseReason(reason []string) string {
	if len(reason) == 0 {
		return ""
	}
	return reason[0]
}

func contentSnapshot(index *store.Index) []SegmentChecksum {
	contents := make([]SegmentChecksum, 0)
	for _, variant := range index.Variants {
		for _, segment := range variant.Segments {
			contents = append(contents, SegmentChecksum{Variant: variant.Name, Index: segment.Index, SHA256: segment.SHA256})
		}
	}
	sort.Slice(contents, func(i, j int) bool {
		if contents[i].Variant != contents[j].Variant {
			return contents[i].Variant < contents[j].Variant
		}
		return contents[i].Index < contents[j].Index
	})
	return contents
}

func affectedChecksums(before, after *store.Index, segments map[int]bool, relation string) []VariantChecksum {
	result := make([]VariantChecksum, 0, len(after.Variants))
	for _, newVariant := range after.Variants {
		item := VariantChecksum{Variant: newVariant.Name, Relation: relation}
		for _, segment := range newVariant.Segments {
			if segments[segment.Index] {
				item.AfterSHA256 = segment.SHA256
			}
		}
		if before != nil {
			for _, oldVariant := range before.Variants {
				if oldVariant.Name != newVariant.Name {
					continue
				}
				for _, segment := range oldVariant.Segments {
					if segments[segment.Index] {
						item.BeforeSHA256 = segment.SHA256
					}
				}
			}
		}
		result = append(result, item)
	}
	return result
}

func checksumMap(note ReleaseNote) map[string]map[int]string {
	result := map[string]map[int]string{}
	for _, content := range note.Contents {
		if result[content.Variant] == nil {
			result[content.Variant] = map[int]string{}
		}
		result[content.Variant][content.Index] = content.SHA256
	}
	return result
}

func (p *Publisher) ComparePublications(fromID, toID string) (ReleaseComparison, error) {
	from, err := p.ReleaseNote(fromID)
	if err != nil {
		return ReleaseComparison{}, err
	}
	to, err := p.ReleaseNote(toID)
	if err != nil {
		return ReleaseComparison{}, err
	}
	fromSums := checksumMap(from)
	toSums := checksumMap(to)
	changed := map[int]bool{}
	variants := map[string]bool{}
	for variant, segments := range fromSums {
		variants[variant] = true
		for index, sum := range segments {
			if toSums[variant][index] != sum {
				changed[index] = true
			}
		}
	}
	for variant, segments := range toSums {
		variants[variant] = true
		for index, sum := range segments {
			if fromSums[variant][index] != sum {
				changed[index] = true
			}
		}
	}
	indices := make([]int, 0, len(changed))
	for index := range changed {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	ranges := make([]store.Range, 0, len(indices))
	for _, index := range indices {
		start := uint64(index) * media.SegmentDuration
		end := start + media.SegmentDuration
		if len(ranges) > 0 && ranges[len(ranges)-1].End == start {
			ranges[len(ranges)-1].End = end
		} else {
			ranges = append(ranges, store.Range{Start: start, End: end})
		}
	}
	variantNames := make([]string, 0, len(variants))
	for name := range variants {
		variantNames = append(variantNames, name)
	}
	sort.Strings(variantNames)
	comparisons := make([]SegmentComparison, 0)
	for _, index := range indices {
		for _, variant := range variantNames {
			comparisons = append(comparisons, SegmentComparison{
				Index: index, Variant: variant,
				FromSHA256: fromSums[variant][index], ToSHA256: toSums[variant][index],
				Changed: true,
			})
		}
	}
	return ReleaseComparison{FromID: fromID, ToID: toID, Ranges: ranges, Segments: comparisons}, nil
}

func (p *Publisher) Compare(fromID, toID string) (ReleaseComparison, error) {
	return p.ComparePublications(fromID, toID)
}
