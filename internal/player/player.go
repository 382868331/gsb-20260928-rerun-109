package player

import (
	"errors"
	"fmt"
	"os"

	"localstream/internal/media"
	"localstream/internal/store"
)

type Player struct {
	store       *store.Store
	stream      string
	publication string
	variants    map[string]store.Variant
	current     string
	position    uint64
	retention   *store.Retention
}

var (
	ErrWindowExpired = errors.New("playback position is outside the live window")
	ErrPlaybackEnded = errors.New("playback has reached the end")
	ErrInvalidJump   = errors.New("jump target is outside an available segment boundary")
)

func Open(s *store.Store, initialVariant string) (*Player, *store.Index, error) {
	index, _, err := s.Current()
	if err != nil {
		return nil, nil, err
	}
	return OpenAt(s, initialVariant, index.WindowStart)
}

func OpenStream(s *store.Store, stream, initialVariant string) (*Player, *store.Index, error) {
	index, _, err := s.CurrentForStream(stream)
	if err != nil {
		return nil, nil, err
	}
	return OpenStreamAt(s, stream, initialVariant, index.WindowStart)
}

func OpenStreamAt(s *store.Store, stream, initialVariant string, position uint64) (*Player, *store.Index, error) {
	index, id, err := s.CurrentForStream(stream)
	if err != nil {
		return nil, nil, err
	}
	return openIndex(s, stream, id, index, initialVariant, position)
}

func OpenAt(s *store.Store, initialVariant string, position uint64) (*Player, *store.Index, error) {
	index, id, err := s.Current()
	if err != nil {
		return nil, nil, err
	}
	return openIndex(s, "", id, index, initialVariant, position)
}

func openIndex(s *store.Store, stream, id string, index *store.Index, initialVariant string, position uint64) (*Player, *store.Index, error) {
	variants := map[string]store.Variant{}
	for _, variant := range index.Variants {
		variants[variant.Name] = variant
	}
	if _, ok := variants[initialVariant]; !ok {
		return nil, nil, fmt.Errorf("unknown variant %q", initialVariant)
	}
	if position == 0 {
		position = index.WindowStart
	}
	if position < index.WindowStart || position >= index.Timeline || position%media.SegmentDuration != 0 {
		return nil, nil, fmt.Errorf("position %d is not in window [%d,%d): %w", position, index.WindowStart, index.Timeline, ErrInvalidJump)
	}
	retention, err := s.Retain(id)
	if err != nil {
		return nil, nil, err
	}
	return &Player{store: s, stream: stream, publication: id, variants: variants, current: initialVariant, position: position, retention: retention}, index, nil
}

func (p *Player) Variant() string  { return p.current }
func (p *Player) Position() uint64 { return p.position }

func (p *Player) ReadTo(position uint64) error {
	for p.position < position {
		if _, _, err := p.ReadSegment(); err != nil {
			return err
		}
	}
	if p.position != position {
		return fmt.Errorf("position %d is not spliceable", position)
	}
	return nil
}

func (p *Player) Switch(name string) error {
	if p.position != 0 && p.position%media.SegmentDuration != 0 {
		return errors.New("switch is only allowed at a spliceable segment boundary")
	}
	if _, ok := p.variants[name]; !ok {
		return fmt.Errorf("unknown variant %q", name)
	}
	p.current = name
	return nil
}

func (p *Player) Publication() string { return p.publication }

func (p *Player) Window() (uint64, uint64) {
	variant := p.variants[p.current]
	if len(variant.Segments) == 0 {
		return p.position, p.position
	}
	return variant.Segments[0].Start, variant.Segments[len(variant.Segments)-1].End
}

func (p *Player) Refresh() (*store.Index, error) {
	index, id, err := p.store.CurrentForStream(p.stream)
	if err != nil {
		return nil, err
	}
	if p.position < index.WindowStart {
		return index, fmt.Errorf("position %d before window start %d: %w", p.position, index.WindowStart, ErrWindowExpired)
	}
	retention, err := p.store.Retain(id)
	if err != nil {
		return nil, err
	}
	oldRetention := p.retention
	oldPublication := p.publication
	p.publication = id
	p.variants = map[string]store.Variant{}
	for _, variant := range index.Variants {
		p.variants[variant.Name] = variant
	}
	if _, ok := p.variants[p.current]; !ok {
		p.publication = oldPublication
		retention.Release()
		return nil, fmt.Errorf("current variant %q is unavailable after refresh", p.current)
	}
	p.retention = retention
	if err := oldRetention.Release(); err != nil {
		return nil, err
	}
	return index, nil
}

func (p *Player) JumpTo(position uint64) error {
	start, end := p.Window()
	if position < start || position >= end || position%media.SegmentDuration != 0 {
		return fmt.Errorf("cannot jump to %d in window [%d,%d): %w", position, start, end, ErrInvalidJump)
	}
	p.position = position
	return nil
}

func (p *Player) JumpToLive(targets ...store.Index) error {
	index := &store.Index{}
	id := p.publication
	if len(targets) > 0 {
		index = &targets[0]
		id = index.ID
	} else {
		current, currentID, err := p.store.CurrentForStream(p.stream)
		if err != nil {
			return err
		}
		index = current
		id = currentID
	}
	end := index.Timeline
	if end < media.SegmentDuration {
		return ErrPlaybackEnded
	}
	target := end - media.SegmentDuration
	if target < index.WindowStart || target%media.SegmentDuration != 0 {
		return ErrInvalidJump
	}
	retention, err := p.store.Retain(id)
	if err != nil {
		return err
	}
	variants := map[string]store.Variant{}
	for _, variant := range index.Variants {
		variants[variant.Name] = variant
	}
	if _, ok := variants[p.current]; !ok {
		retention.Release()
		return fmt.Errorf("current variant %q is unavailable", p.current)
	}
	old := p.retention
	p.publication = id
	p.variants = variants
	p.position = target
	p.retention = retention
	if old != nil {
		return old.Release()
	}
	return nil
}

func (p *Player) Close() error {
	if p.retention == nil {
		return nil
	}
	err := p.retention.Release()
	p.retention = nil
	return err
}

func (p *Player) ReadSegment() (media.Header, []media.Packet, error) {
	variant := p.variants[p.current]
	start, end := p.Window()
	if p.position < start {
		return media.Header{}, nil, fmt.Errorf("position %d before window start %d: %w", p.position, start, ErrWindowExpired)
	}
	if p.position >= end {
		return media.Header{}, nil, fmt.Errorf("playback ended at %d: %w", p.position, ErrPlaybackEnded)
	}
	if p.position%media.SegmentDuration != 0 {
		return media.Header{}, nil, errors.New("playback position is not on a segment boundary")
	}
	var meta store.Segment
	found := false
	for _, candidate := range variant.Segments {
		if candidate.Start == p.position {
			meta = candidate
			found = true
			break
		}
	}
	if !found {
		return media.Header{}, nil, fmt.Errorf("segment at %d is unavailable: %w", p.position, ErrWindowExpired)
	}
	f, err := os.Open(p.store.SegmentPath(p.publication, p.current, meta))
	if err != nil {
		return media.Header{}, nil, err
	}
	defer f.Close()
	header, packets, err := media.Decode(f)
	if err != nil {
		return media.Header{}, nil, err
	}
	if header.StartPTS != p.position {
		return media.Header{}, nil, fmt.Errorf("segment started at %d, playback expected %d", header.StartPTS, p.position)
	}
	p.position = header.StartPTS + header.Duration
	return header, packets, nil
}

func (p *Player) AtEnd() bool {
	_, end := p.Window()
	return p.position >= end
}
