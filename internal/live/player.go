package live

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"localstream/internal/media"
)

type Player struct {
	publisher *Publisher
	lease     Lease
	manifest  Manifest
	variant   string
	sequence  int
	stopped   bool
}

func OpenPlayer(p *Publisher, initialVariant string) (*Player, error) {
	lease, manifest, err := p.Acquire()
	if err != nil {
		return nil, err
	}
	if _, err := findVariant(manifest, initialVariant); err != nil {
		p.Release(lease.ID)
		return nil, err
	}
	return &Player{publisher: p, lease: lease, manifest: manifest, variant: initialVariant, sequence: manifest.WindowStart}, nil
}

func (p *Player) Close() {
	if p != nil && p.publisher != nil {
		p.publisher.Release(p.lease.ID)
		p.publisher = nil
	}
}

func (p *Player) Variant() string  { return p.variant }
func (p *Player) Position() uint64 { return uint64(p.sequence) * media.SegmentDuration }
func (p *Player) Sequence() int    { return p.sequence }
func (p *Player) Stopped() bool    { return p.stopped }
func (p *Player) WindowStart() int { return p.manifest.WindowStart }
func (p *Player) LiveEdge() int    { return p.manifest.WindowEnd }

func (p *Player) Stop() {
	p.stopped = true
	p.Close()
}

func (p *Player) Switch(name string) error {
	if p.stopped {
		return ErrStopped
	}
	if _, err := findVariant(p.manifest, name); err != nil {
		return err
	}
	p.variant = name
	return nil
}

func (p *Player) Jump(sequence int) (Manifest, error) {
	if p.stopped {
		return Manifest{}, ErrStopped
	}
	manifest, err := p.publisher.SnapshotFor(p.lease, sequence)
	if err != nil {
		return manifest, err
	}
	if _, err := findVariant(manifest, p.variant); err != nil {
		return manifest, err
	}
	p.manifest = manifest
	p.sequence = sequence
	return manifest, nil
}

func (p *Player) JumpToLive() (Manifest, error) {
	manifest, err := p.publisher.Manifest()
	if err != nil {
		return Manifest{}, err
	}
	return p.Jump(manifest.WindowEnd - 1)
}

func (p *Player) Refresh() (Manifest, bool, error) {
	if p.stopped {
		return Manifest{}, false, ErrStopped
	}
	manifest, err := p.publisher.SnapshotFor(p.lease, p.sequence)
	if errors.Is(err, ErrWindowExpired) || errors.Is(err, ErrNotAvailable) {
		return manifest, false, err
	}
	if err != nil {
		return Manifest{}, false, err
	}
	changed := manifest.Generation != p.manifest.Generation
	p.manifest = manifest
	return manifest, changed, nil
}

func (p *Player) ReadSegment() (media.Header, []media.Packet, error) {
	if p.stopped {
		return media.Header{}, nil, ErrStopped
	}
	manifest, err := p.publisher.SnapshotFor(p.lease, p.sequence)
	if err != nil {
		return media.Header{}, nil, err
	}
	variant, err := findVariant(manifest, p.variant)
	if err != nil {
		return media.Header{}, nil, err
	}
	segment, err := findSegment(variant, p.sequence)
	if err != nil {
		return media.Header{}, nil, err
	}
	f, err := os.Open(p.publisher.SegmentPath(variant.Name, segment))
	if err != nil {
		return media.Header{}, nil, err
	}
	defer f.Close()
	header, packets, err := media.Decode(f)
	if err != nil {
		return media.Header{}, nil, err
	}
	checksum, err := hashOpenFile(f)
	if err != nil {
		return media.Header{}, nil, err
	}
	if checksum != segment.SHA256 {
		return media.Header{}, nil, fmt.Errorf("checksum mismatch for live segment %d", segment.Sequence)
	}
	if header.StartPTS != segment.Start || header.StartPTS+header.Duration != segment.End {
		return media.Header{}, nil, fmt.Errorf("live segment %d does not match the shared timeline", segment.Sequence)
	}
	p.manifest = manifest
	p.sequence++
	return header, packets, nil
}

func findVariant(manifest Manifest, name string) (Variant, error) {
	for _, variant := range manifest.Variants {
		if variant.Name == name {
			return variant, nil
		}
	}
	return Variant{}, fmt.Errorf("unknown live variant %q", name)
}

func findSegment(variant Variant, sequence int) (Segment, error) {
	index := sequence - variant.Segments[0].Sequence
	if index < 0 || index >= len(variant.Segments) {
		return Segment{}, fmt.Errorf("live segment %d is outside %s window", sequence, variant.Name)
	}
	segment := variant.Segments[index]
	if segment.Sequence != sequence {
		return Segment{}, fmt.Errorf("live segment %d has a discontinuous index", sequence)
	}
	return segment, nil
}

func hashOpenFile(f *os.File) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
