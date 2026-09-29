package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"localstream/internal/indexer"
	"localstream/internal/media"
	"localstream/internal/player"
	"localstream/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "demo":
		err = demo(os.Args[2:])
	case "read":
		err = read(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: streamctl demo|read [flags]")
	os.Exit(2)
}

func demo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	root := fs.String("root", "data", "media repository root")
	id := fs.String("id", "v1", "publication id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sourceDir := filepath.Join(*root, "sources", "short")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		return err
	}
	for _, variant := range media.Variants {
		dir := filepath.Join(sourceDir, variant.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for segment := 0; segment < media.SegmentCount; segment++ {
			path := filepath.Join(dir, fmt.Sprintf("%06d.sm", segment))
			if err := media.WriteCompleteSegment(path, variant, segment); err != nil {
				return err
			}
		}
		partial := filepath.Join(dir, ".000006.sm.part")
		if err := os.WriteFile(partial, []byte("unfinished"), 0o644); err != nil {
			return err
		}
	}
	sourceIndex, err := indexer.Scan(sourceDir)
	if err != nil {
		return err
	}
	indexPath := filepath.Join(*root, "sources", "short-index.json")
	if err := indexer.Write(indexPath, sourceIndex); err != nil {
		return err
	}
	variants := make([]store.Variant, 0, len(sourceIndex.Variants))
	var timeline uint64
	for _, sourceVariant := range sourceIndex.Variants {
		segments := make([]store.Segment, 0, len(sourceVariant.Segments))
		for _, segment := range sourceVariant.Segments {
			segments = append(segments, store.Segment{Index: segment.Index, Start: segment.Start, End: segment.End, Path: filepath.Base(segment.Path), SHA256: segment.SHA256})
			timeline = segment.End
		}
		dimensions := media.Variants[0]
		for _, spec := range media.Variants {
			if spec.Name == sourceVariant.Name {
				dimensions = spec
			}
		}
		variants = append(variants, store.Variant{Name: sourceVariant.Name, Width: dimensions.Width, Height: dimensions.Height, Segments: segments})
	}
	s := store.Open(*root)
	staging, err := s.Prepare(*id, variants, func(variantName string, segmentIndex int, destination string) error {
		return copyFile(filepath.Join(sourceDir, variantName, fmt.Sprintf("%06d.sm", segmentIndex)), destination)
	})
	if err != nil {
		return err
	}
	_ = staging
	return s.Commit(*id, store.Index{Timeline: timeline, Variants: variants})
}

func read(args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	root := fs.String("root", "data", "media repository root")
	start := fs.String("variant", "base", "initial variant")
	switchAt := fs.Int("switch-segment", 2, "segment boundary at which to switch")
	switchTo := fs.String("switch-to", "sharp", "variant after boundary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, index, err := player.Open(store.Open(*root), *start)
	if err != nil {
		return err
	}
	fmt.Printf("publication=%s duration=%d timescale=%d\n", index.ID, index.Timeline, media.TimeScale)
	for segment := 0; !p.AtEnd(); segment++ {
		if segment == *switchAt {
			if err := p.Switch(*switchTo); err != nil {
				return err
			}
		}
		header, packets, err := p.ReadSegment()
		if err != nil {
			return err
		}
		fmt.Printf("variant=%s segment=[%d,%d) video=%d audio=%d\n", p.Variant(), header.StartPTS, header.StartPTS+header.Duration, header.VideoFrames, header.AudioFrames)
		_ = packets
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
