package main

import (
	"fmt"
	"path/filepath"

	"streamcore/internal/encode"
)

// loadEncoded rebuilds Rendition metadata from an existing encoded directory.
func loadEncoded(encDir string) ([]encode.Rendition, error) {
	var out []encode.Rendition
	for _, spec := range encode.Renditions {
		dir := filepath.Join(encDir, spec.Name)
		segs, err := encode.ListSegments(dir)
		if err != nil {
			return nil, fmt.Errorf("rendition %s: %w", spec.Name, err)
		}
		out = append(out, encode.Rendition{Spec: spec, Dir: dir, Segments: segs})
	}
	return out, nil
}
