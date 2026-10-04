package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"localstream/internal/indexer"
	"localstream/internal/live"
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
	case "live":
		err = publishLive(os.Args[2:])
	case "correct":
		err = correct(os.Args[2:])
	case "withdraw-correction":
		err = withdrawCorrection(os.Args[2:])
	case "read-live":
		err = readLive(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: streamctl demo|read|live|correct|withdraw-correction|read-live [flags]")
	os.Exit(2)
}

func publishLive(args []string) error {
	fs := flag.NewFlagSet("live", flag.ContinueOnError)
	root := fs.String("root", "data", "media repository root")
	stream := fs.String("stream", "live", "stream name")
	window := fs.Int("window", live.DefaultWindowSegments, "segments retained in the live window")
	segments := fs.Int("segments", 1, "segments to publish in this invocation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	publisher, err := live.NewPublisher(*root, *stream, *window)
	if err != nil {
		return err
	}
	for range *segments {
		index, err := publisher.PublishNext()
		if err != nil {
			return err
		}
		fmt.Printf("published=%s segment=%d window=[%d,%d)\n", index.ID, publisher.NextSegment()-1, index.WindowStart, index.Timeline)
	}
	return nil
}

func correct(args []string) error {
	fs := flag.NewFlagSet("correct", flag.ContinueOnError)
	root := fs.String("root", "data", "media repository root")
	stream := fs.String("stream", "live", "stream name")
	window := fs.Int("window", live.DefaultWindowSegments, "segments retained in the live window")
	segment := fs.Int("segment", -1, "in-window segment index to replace with corrected content")
	reason := fs.String("reason", "correct live content", "operator-visible reason for the correction")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *segment < 0 {
		return fmt.Errorf("-segment is required")
	}
	publisher, err := live.NewPublisher(*root, *stream, *window)
	if err != nil {
		return err
	}
	index, err := publisher.Correct(*segment, *reason)
	if err != nil {
		return err
	}
	fmt.Printf("corrected=%s segment=%d window=[%d,%d)\n", index.ID, *segment, index.WindowStart, index.Timeline)
	return nil
}

func withdrawCorrection(args []string) error {
	fs := flag.NewFlagSet("withdraw-correction", flag.ContinueOnError)
	root := fs.String("root", "data", "media repository root")
	stream := fs.String("stream", "live", "stream name")
	window := fs.Int("window", live.DefaultWindowSegments, "segments retained in the live window")
	correctionID := fs.String("correction-id", "", "correction publication id to withdraw")
	reason := fs.String("reason", "withdraw live correction", "operator-visible reason for the withdrawal")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *correctionID == "" {
		return fmt.Errorf("-correction-id is required")
	}
	publisher, err := live.NewPublisher(*root, *stream, *window)
	if err != nil {
		return err
	}
	index, err := publisher.WithdrawCorrection(*correctionID, *reason)
	if err != nil {
		return err
	}
	fmt.Printf("withdrawn=%s correction=%s window=[%d,%d)\n", index.ID, *correctionID, index.WindowStart, index.Timeline)
	return nil
}

func readLive(args []string) error {
	fs := flag.NewFlagSet("read-live", flag.ContinueOnError)
	root := fs.String("root", "data", "media repository root")
	stream := fs.String("stream", "live", "stream name")
	variant := fs.String("variant", "base", "initial variant")
	start := fs.Int64("start-pts", -1, "segment boundary PTS in the current window")
	switchTo := fs.String("switch-to", "sharp", "variant after the first segment")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, index, err := player.OpenStream(store.Open(*root), *stream, *variant)
	if err != nil {
		return err
	}
	defer p.Close()
	if *start >= 0 {
		if err := p.JumpTo(uint64(*start)); err != nil {
			return err
		}
	}
	first := true
	for !p.AtEnd() {
		if first && *switchTo != "" && *variant != *switchTo {
			if err := p.Switch(*switchTo); err != nil {
				return err
			}
		}
		header, _, err := p.ReadSegment()
		if err != nil {
			return err
		}
		fmt.Printf("publication=%s variant=%s segment=[%d,%d)\n", index.ID, p.Variant(), header.StartPTS, header.StartPTS+header.Duration)
		first = false
	}
	return nil
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
