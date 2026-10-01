package live

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func safeName(name string) (string, error) {
	name = filepath.Clean(name)
	if name == "." || name == ".." || filepath.IsAbs(name) || strings.Contains(name, string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe stream name %q", name)
	}
	return name, nil
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

func cloneManifest(m Manifest) Manifest {
	copyValue := m
	copyValue.Variants = make([]Variant, len(m.Variants))
	for i := range m.Variants {
		copyValue.Variants[i] = m.Variants[i]
		copyValue.Variants[i].Segments = append([]Segment(nil), m.Variants[i].Segments...)
	}
	copyValue.Retired = append([]RetiredFile(nil), m.Retired...)
	return copyValue
}

func mergeVariants(existing, incoming []Variant) []Variant {
	byName := map[string]Variant{}
	var names []string
	for _, variant := range existing {
		byName[variant.Name] = cloneVariant(variant)
		names = append(names, variant.Name)
	}
	for _, variant := range incoming {
		current, exists := byName[variant.Name]
		if !exists {
			current = Variant{Name: variant.Name, Width: variant.Width, Height: variant.Height}
			byName[variant.Name] = current
			names = append(names, variant.Name)
		}
		current.Segments = append(current.Segments, variant.Segments...)
		byName[variant.Name] = current
	}
	sort.Strings(names)
	result := make([]Variant, 0, len(names))
	for _, name := range names {
		variant := byName[name]
		result = append(result, variant)
	}
	return result
}

func cloneVariant(v Variant) Variant {
	v.Segments = append([]Segment(nil), v.Segments...)
	return v
}

func trimVariants(variants []Variant, windowStart int) []Variant {
	result := make([]Variant, len(variants))
	for i, variant := range variants {
		result[i] = Variant{Name: variant.Name, Width: variant.Width, Height: variant.Height}
		for _, segment := range variant.Segments {
			if segment.Sequence >= windowStart {
				result[i].Segments = append(result[i].Segments, segment)
			}
		}
	}
	return result
}

func removedFiles(before, after []Variant) []string {
	active := map[string]bool{}
	for _, variant := range after {
		for _, segment := range variant.Segments {
			active[filepath.Join(variant.Name, segment.Path)] = true
		}
	}
	var removed []string
	for _, variant := range before {
		for _, segment := range variant.Segments {
			path := filepath.Join(variant.Name, segment.Path)
			if !active[path] {
				removed = append(removed, path)
			}
		}
	}
	return removed
}

func validateManifest(m Manifest, stream string, windowSize int) error {
	if m.Version != 1 || m.Stream != stream || m.WindowSegments != windowSize {
		return fmt.Errorf("live manifest does not match stream %q window %d", stream, windowSize)
	}
	if m.WindowEnd != m.NextSequence || m.WindowStart > m.WindowEnd || m.WindowEnd-m.WindowStart > windowSize {
		return fmt.Errorf("invalid live window [%d,%d)", m.WindowStart, m.WindowEnd)
	}
	if len(m.Variants) < 2 {
		return errors.New("live manifest requires at least two variants")
	}
	for _, variant := range m.Variants {
		if len(variant.Segments) != m.WindowEnd-m.WindowStart {
			return fmt.Errorf("variant %s does not cover the complete window", variant.Name)
		}
		for i, segment := range variant.Segments {
			sequence := m.WindowStart + i
			if segment.Sequence != sequence || segment.Start != uint64(sequence)*48000 || segment.End != segment.Start+48000 || segment.SHA256 == "" {
				return fmt.Errorf("invalid segment metadata in variant %s", variant.Name)
			}
		}
	}
	return nil
}
