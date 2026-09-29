// Command streamcore drives the local streaming-distribution core:
// source generation, two-tier encoding, versioned publishing, tier-switch
// playback planning, and real-decode verification.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"streamcore/internal/client"
	"streamcore/internal/encode"
	"streamcore/internal/manifest"
	"streamcore/internal/publish"
	"streamcore/internal/source"
	"streamcore/internal/verify"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = cmdGenerate(os.Args[2:])
	case "encode":
		err = cmdEncode(os.Args[2:])
	case "publish":
		err = cmdPublish(os.Args[2:])
	case "play":
		err = cmdPlay(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `streamcore - local two-tier media distribution core

usage:
  streamcore generate -workdir DIR
  streamcore encode   -workdir DIR
  streamcore publish  -workdir DIR
  streamcore play     -workdir DIR -start low -switch high@2 -switch low@4`)
}

func paths(workdir string) (srcDir, encDir, pubRoot string) {
	return filepath.Join(workdir, "source"),
		filepath.Join(workdir, "encoded"),
		filepath.Join(workdir, "publication")
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	wd := fs.String("workdir", "work", "working directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	srcDir, _, _ := paths(*wd)
	m, err := source.Generate(srcDir)
	if err != nil {
		return err
	}
	fmt.Printf("generated bundled source: %s (%d video frames, %d audio samples)\n", m.MuxedMP4, m.Frames, m.Samples)
	return nil
}

func cmdEncode(args []string) error {
	fs := flag.NewFlagSet("encode", flag.ContinueOnError)
	wd := fs.String("workdir", "work", "working directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	srcDir, encDir, _ := paths(*wd)
	rend, err := encode.All(filepath.Join(srcDir, "source.mp4"), encDir)
	if err != nil {
		return err
	}
	for _, r := range rend {
		fmt.Printf("rendition %-4s %dx%d: %d segments in %s\n", r.Spec.Name, r.Spec.Width, r.Spec.Height, len(r.Segments), r.Dir)
	}
	return nil
}

func cmdPublish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	wd := fs.String("workdir", "work", "working directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, encDir, pubRoot := paths(*wd)
	rend, err := loadEncoded(encDir)
	if err != nil {
		return err
	}
	p := publish.New(pubRoot)
	m, err := p.Publish(rend, "source.mp4")
	if err != nil {
		return err
	}
	n, _ := m.Timeline()
	fmt.Printf("published version %d with %d renditions x %d segments -> %s\n", m.Version, len(m.Renditions), n, p.CurrentPath())
	return nil
}

func cmdPlay(args []string) error {
	fs := flag.NewFlagSet("play", flag.ContinueOnError)
	wd := fs.String("workdir", "work", "working directory")
	start := fs.String("start", "low", "starting rendition")
	var switches multiFlag
	fs.Var(&switches, "switch", "tier switch at boundary, e.g. high@2")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, _, pubRoot := paths(*wd)
	m, err := manifest.Load(filepath.Join(pubRoot, "current.json"))
	if err != nil {
		return err
	}
	var sw []client.Switch
	for _, s := range switches {
		parts := strings.Split(s, "@")
		if len(parts) != 2 {
			return fmt.Errorf("invalid -switch %q, want tier@boundary", s)
		}
		at, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return fmt.Errorf("invalid switch time %q: %w", parts[1], err)
		}
		sw = append(sw, client.Switch{Tier: parts[0], At: at})
	}
	plan, err := client.Plan(m, *start, sw)
	if err != nil {
		return err
	}
	res, err := verify.Plan(pubRoot, m, plan)
	if err != nil {
		return err
	}
	out := map[string]any{
		"reads":                 planSummary(plan),
		"decoded_video_frames":  res.DecodedVideoFrames,
		"decoded_audio_samples": res.DecodedAudioSamples,
		"max_luma_error":        res.MaxLumaError,
		"max_freq_error_hz":     res.MaxFreqError,
		"boundaries":            res.Boundaries,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func planSummary(plan []client.PlannedSegment) []map[string]any {
	var out []map[string]any
	for _, p := range plan {
		out = append(out, map[string]any{
			"rendition": p.Rendition,
			"index":     p.Index,
			"start_pts": p.StartPTS,
			"duration":  p.Duration,
			"file":      p.File,
		})
	}
	return out
}
