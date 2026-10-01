package live

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"localstream/internal/media"
)

var (
	ErrWindowExpired = errors.New("live window expired")
	ErrNotAvailable  = errors.New("live segment is not available yet")
	ErrStopped       = errors.New("live playback is stopped")
	ErrNoWindow      = errors.New("live window is empty")
)

type Segment struct {
	Sequence int    `json:"sequence"`
	Start    uint64 `json:"start_pts"`
	End      uint64 `json:"end_pts"`
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
}

type Variant struct {
	Name     string    `json:"name"`
	Width    uint16    `json:"width"`
	Height   uint16    `json:"height"`
	Segments []Segment `json:"segments"`
}

type Manifest struct {
	Version        int           `json:"version"`
	Stream         string        `json:"stream"`
	Generation     int64         `json:"generation"`
	NextSequence   int           `json:"next_sequence"`
	WindowStart    int           `json:"window_start"`
	WindowEnd      int           `json:"window_end"`
	WindowSegments int           `json:"window_segments"`
	Variants       []Variant     `json:"variants"`
	Retired        []RetiredFile `json:"retired_files"`
}

type RetiredFile struct {
	Path                    string `json:"path"`
	SinceManifestGeneration int64  `json:"since_manifest_generation"`
}

type Publisher struct {
	mu        sync.Mutex
	root      string
	stream    string
	window    int
	manifest  Manifest
	leases    map[int64]int64
	nextLease int64
	has       bool
}

type Lease struct {
	ID         int64
	Generation int64
	Position   int
}

func NewPublisher(root, stream string, windowSegments int) (*Publisher, error) {
	if windowSegments <= 0 {
		return nil, errors.New("windowSegments must be positive")
	}
	stream, err := safeName(stream)
	if err != nil {
		return nil, err
	}
	p := &Publisher{
		root:      filepath.Join(root, "live", stream),
		stream:    stream,
		window:    windowSegments,
		leases:    map[int64]int64{},
		nextLease: 1,
	}
	if err := os.MkdirAll(p.root, 0o755); err != nil {
		return nil, err
	}
	if err := p.removeOrphanFiles(); err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(p.manifestPath()); err == nil {
		if err := json.Unmarshal(data, &p.manifest); err != nil {
			return nil, err
		}
		if err := validateManifest(p.manifest, p.stream, windowSegments); err != nil {
			return nil, err
		}
		p.manifest.Retired = p.cleanRetired(p.manifest.Retired)
		p.has = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return p, nil
}

func (p *Publisher) Stream() string { return p.stream }

func (p *Publisher) Manifest() (Manifest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.has {
		return Manifest{}, ErrNoWindow
	}
	return cloneManifest(p.manifest), nil
}

func (p *Publisher) NextSequence() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.has {
		return 0
	}
	return p.manifest.NextSequence
}

func (p *Publisher) Acquire() (Lease, Manifest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.has {
		return Lease{}, Manifest{}, ErrNoWindow
	}
	id := p.nextLease
	p.nextLease++
	p.leases[id] = p.manifest.Generation
	return Lease{ID: id, Generation: p.manifest.Generation, Position: p.manifest.WindowStart}, cloneManifest(p.manifest), nil
}

func (p *Publisher) Release(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.leases, id)
	p.collectAfterLock()
}

func (p *Publisher) SnapshotFor(lease Lease, sequence int) (Manifest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.has {
		return Manifest{}, ErrNoWindow
	}
	if _, ok := p.leases[lease.ID]; !ok {
		return Manifest{}, errors.New("released live playback lease")
	}
	if sequence < p.manifest.WindowStart {
		return cloneManifest(p.manifest), ErrWindowExpired
	}
	if sequence >= p.manifest.WindowEnd {
		return cloneManifest(p.manifest), ErrNotAvailable
	}
	return cloneManifest(p.manifest), nil
}

func (p *Publisher) PublishNext(specs []media.VariantSpec) (Manifest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sequence := 0
	generation := int64(1)
	if p.has {
		sequence = p.manifest.NextSequence
		generation = p.manifest.Generation + 1
	}
	if len(specs) < 2 {
		return Manifest{}, errors.New("live publication requires at least two variants")
	}
	byName := map[string]media.VariantSpec{}
	for _, spec := range specs {
		if _, exists := byName[spec.Name]; exists {
			return Manifest{}, fmt.Errorf("duplicate variant %q", spec.Name)
		}
		byName[spec.Name] = spec
	}

	var variants []Variant
	for _, spec := range specs {
		dir := filepath.Join(p.root, spec.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Manifest{}, err
		}
		final := filepath.Join(dir, fmt.Sprintf("%06d.sm", sequence))
		temporary := final + ".part"
		_ = os.Remove(temporary)
		if err := media.WriteCompleteSegment(temporary, spec, sequence); err != nil {
			_ = os.Remove(temporary)
			return Manifest{}, err
		}
		checksum, err := hashFile(temporary)
		if err != nil {
			_ = os.Remove(temporary)
			return Manifest{}, err
		}
		if err := os.Rename(temporary, final); err != nil {
			_ = os.Remove(temporary)
			return Manifest{}, err
		}
		variants = append(variants, Variant{
			Name:   spec.Name,
			Width:  spec.Width,
			Height: spec.Height,
			Segments: []Segment{{
				Sequence: sequence,
				Start:    uint64(sequence) * media.SegmentDuration,
				End:      uint64(sequence+1) * media.SegmentDuration,
				Path:     filepath.Base(final),
				SHA256:   checksum,
			}},
		})
	}
	sort.Slice(variants, func(i, j int) bool { return variants[i].Name < variants[j].Name })

	next := p.manifestWithoutLock()
	merged := mergeVariants(next.Variants, variants)
	windowStart := 0
	if sequence+1 > p.window {
		windowStart = sequence + 1 - p.window
	}
	updated := Manifest{
		Version:        1,
		Stream:         p.stream,
		Generation:     generation,
		NextSequence:   sequence + 1,
		WindowStart:    windowStart,
		WindowEnd:      sequence + 1,
		WindowSegments: p.window,
		Variants:       trimVariants(merged, windowStart),
	}
	updated.Retired = append(updated.Retired, p.manifest.Retired...)
	for _, path := range removedFiles(next.Variants, updated.Variants) {
		updated.Retired = append(updated.Retired, RetiredFile{Path: path, SinceManifestGeneration: generation})
	}
	if err := validateManifest(updated, p.stream, p.window); err != nil {
		return Manifest{}, err
	}
	if err := p.writeManifest(updated); err != nil {
		return Manifest{}, err
	}
	p.manifest = updated
	p.has = true
	p.collectAfterLock()
	return cloneManifest(p.manifest), nil
}

func (p *Publisher) RecoverInterruption() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.removeOrphanFiles()
}

func (p *Publisher) SegmentPath(variantName string, segment Segment) string {
	return filepath.Join(p.root, variantName, filepath.Base(segment.Path))
}

func (p *Publisher) manifestPath() string { return filepath.Join(p.root, "index.json") }

func (p *Publisher) manifestWithoutLock() Manifest {
	if !p.has {
		return Manifest{Variants: nil}
	}
	return cloneManifest(p.manifest)
}

func (p *Publisher) writeManifest(m Manifest) error {
	temporary := p.manifestPath() + ".tmp"
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(temporary, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, p.manifestPath())
}

func (p *Publisher) collectAfterLock() {
	if !p.has {
		return
	}
	oldest := int64(0)
	for _, generation := range p.leases {
		if oldest == 0 || generation < oldest {
			oldest = generation
		}
	}
	var keepRetired []RetiredFile
	for _, retired := range p.manifest.Retired {
		if oldest != 0 && retired.SinceManifestGeneration >= oldest {
			keepRetired = append(keepRetired, retired)
			continue
		}
		if err := os.Remove(filepath.Join(p.root, retired.Path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			keepRetired = append(keepRetired, retired)
		}
	}
	p.manifest.Retired = keepRetired
	_ = p.writeManifest(p.manifest)
}

func (p *Publisher) cleanRetired(files []RetiredFile) []RetiredFile {
	var keep []RetiredFile
	for _, file := range files {
		if err := os.Remove(filepath.Join(p.root, file.Path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			keep = append(keep, file)
		}
	}
	return keep
}

func (p *Publisher) removeOrphanFiles() error {
	listed := map[string]bool{"index.json": true}
	var known []string
	if data, err := os.ReadFile(p.manifestPath()); err == nil {
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return err
		}
		for _, file := range m.Retired {
			known = append(known, file.Path)
		}
		for _, variant := range m.Variants {
			listed[variant.Name] = true
			for _, segment := range variant.Segments {
				known = append(known, filepath.Join(variant.Name, segment.Path))
			}
		}
	}
	for _, name := range known {
		listed[filepath.Clean(name)] = true
	}
	entries, err := os.ReadDir(p.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			if strings.HasSuffix(entry.Name(), ".tmp") {
				_ = os.Remove(filepath.Join(p.root, entry.Name()))
			}
			continue
		}
		variantEntries, err := os.ReadDir(filepath.Join(p.root, entry.Name()))
		if err != nil {
			return err
		}
		for _, file := range variantEntries {
			relative := filepath.Join(entry.Name(), file.Name())
			if listed[relative] {
				continue
			}
			if strings.HasSuffix(file.Name(), ".part") {
				_ = os.Remove(filepath.Join(p.root, relative))
			}
		}
	}
	return nil
}
