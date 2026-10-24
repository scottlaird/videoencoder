// Command videoencoder turns a video into a DASH/HLS stream, rebuilding
// only renditions whose source or settings changed.
//
//	videoencoder [flags] video.mov [other-version.mov ...]
//
// Extra files are other versions of the same video (e.g. SDR next to
// HDR); every version is encoded and offered in one stream. Audio and
// the poster come from the first file.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/scottlaird/videoencoder/internal/ffmpeg"
	"github.com/scottlaird/videoencoder/internal/pack"
	"github.com/scottlaird/videoencoder/internal/plan"
	"github.com/scottlaird/videoencoder/internal/probe"
)

type options struct {
	outdir     string
	streamdir  string
	thumbnails bool
	sources    []string
}

var errUsage = errors.New("usage: videoencoder [flags] video.mov [other-version.mov ...]")

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("videoencoder", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.outdir, "outdir", "transcodes", "output directory for transcodes")
	fs.StringVar(&o.streamdir, "streamdir", "stream", "output directory for streaming video")
	fs.BoolVar(&o.thumbnails, "thumbnails", false, "generate seek thumbnails (not yet supported by shaka-packager)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	o.sources = fs.Args()
	if len(o.sources) == 0 {
		return o, errUsage
	}
	return o, nil
}

type deps struct {
	runner ffmpeg.Runner
	prober probe.Prober
}

func run(ctx context.Context, o options, d deps) error {
	first := o.sources[0]
	vd, err := d.prober.Probe(ctx, first)
	if err != nil {
		return err
	}
	slog.Info("Source", "file", first, "video", fmt.Sprintf("%+v", vd))

	targets := plan.DefaultTargets(o.thumbnails).Filter(vd)
	for _, t := range targets {
		slog.Info("Target", "codec", t.Codec, "width", t.Width, "height", t.Height)
	}

	var videos []string
	for seq, src := range o.sources {
		var batch ffmpeg.Batch
		for _, t := range targets {
			job, err := t.Job(src, o.outdir, seq)
			if err != nil {
				return err
			}
			if _, err := batch.AddIfNeeded(job); err != nil {
				return err
			}
			if t.Codec != "jpg" {
				videos = append(videos, job.Output)
			}
		}
		if err := batch.Run(ctx, d.runner); err != nil {
			return fmt.Errorf("encoding %q: %w", src, err)
		}
	}

	audio := plan.AudioJob(first, o.outdir)
	if err := ffmpeg.RunIfNeeded(ctx, d.runner, audio); err != nil {
		return fmt.Errorf("extracting audio: %w", err)
	}
	poster := plan.PosterJob(first, o.outdir)
	if err := ffmpeg.RunIfNeeded(ctx, d.runner, poster); err != nil {
		return fmt.Errorf("making poster: %w", err)
	}

	p := &pack.Packager{Runner: d.runner, Prober: d.prober, Thumbnails: o.thumbnails}
	mpd, err := p.Package(ctx, o.streamdir, videos, audio.Output)
	if err != nil {
		return fmt.Errorf("packaging: %w", err)
	}

	streamPoster := filepath.Join(o.streamdir, filepath.Base(poster.Output))
	if err := copySmallFile(poster.Output, streamPoster); err != nil {
		return err
	}

	slog.Info("Done", "manifest", mpd, "poster", streamPoster)
	return nil
}

// copySmallFile copies by reading the whole file into memory, so it is
// only for small files such as posters.
func copySmallFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("copying %q: %w", src, err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		return fmt.Errorf("copying %q: %w", src, err)
	}
	return nil
}

func main() {
	o, err := parseArgs(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	d := deps{
		runner: ffmpeg.ExecRunner{Stdout: os.Stdout, Stderr: os.Stderr},
		prober: probe.FFProbe{},
	}
	if err := run(ctx, o, d); err != nil {
		fmt.Fprintln(os.Stderr, "videoencoder:", err)
		os.Exit(1)
	}
}
