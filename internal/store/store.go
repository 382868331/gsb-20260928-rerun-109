package store

import (
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
	Version  int       `json:"version"`
	ID       string    `json:"id"`
	Timeline uint64    `json:"timeline_duration"`
	Variants []Variant `json:"variants"`
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
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}
	var timeline uint64
	for _, variant := range variants {
		if err := validateVariant(variant); err != nil {
			return "", err
		}
		if timeline == 0 {
			timeline = variant.Segments[len(variant.Segments)-1].End
		} else if variant.Segments[len(variant.Segments)-1].End != timeline {
			return "", errors.New("variants do not share the same timeline")
		}
		variantDir := filepath.Join(staging, variant.Name)
		if err := os.MkdirAll(variantDir, 0o755); err != nil {
			return "", err
		}
		for _, segment := range variant.Segments {
			destination := filepath.Join(variantDir, fmt.Sprintf("%06d.sm", segment.Index))
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

func (s *Store) SegmentPath(id, variant string, segment Segment) string {
	return s.path("publish", "versions", id, variant, filepath.Base(segment.Path))
}

func validateVariant(v Variant) error {
	if v.Name == "" || len(v.Segments) == 0 {
		return errors.New("variant and complete segments are required")
	}
	for i, segment := range v.Segments {
		if segment.Index != i || (i == 0 && segment.Start != 0) || (i > 0 && segment.Start != v.Segments[i-1].End) || segment.End <= segment.Start {
			return fmt.Errorf("variant %s is not contiguous at segment %d", v.Name, i)
		}
		if segment.SHA256 == "" {
			return errors.New("published segment requires a checksum")
		}
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
