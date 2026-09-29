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
	publication string
	variants    map[string]store.Variant
	current     string
	segment     int
	position    uint64
}

func Open(s *store.Store, initialVariant string) (*Player, *store.Index, error) {
	index, id, err := s.Current()
	if err != nil {
		return nil, nil, err
	}
	variants := map[string]store.Variant{}
	for _, variant := range index.Variants {
		variants[variant.Name] = variant
	}
	if _, ok := variants[initialVariant]; !ok {
		return nil, nil, fmt.Errorf("unknown variant %q", initialVariant)
	}
	return &Player{store: s, publication: id, variants: variants, current: initialVariant}, index, nil
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
	p.segment = int(p.position / media.SegmentDuration)
	return nil
}

func (p *Player) ReadSegment() (media.Header, []media.Packet, error) {
	variant := p.variants[p.current]
	if p.segment >= len(variant.Segments) {
		return media.Header{}, nil, fmt.Errorf("playback ended at %d", p.position)
	}
	if p.segment < 0 || uint64(p.segment)*media.SegmentDuration != p.position {
		return media.Header{}, nil, errors.New("playback position is not on a segment boundary")
	}
	meta := variant.Segments[p.segment]
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
	p.segment++
	return header, packets, nil
}

func (p *Player) AtEnd() bool {
	return p.segment >= len(p.variants[p.current].Segments)
}
