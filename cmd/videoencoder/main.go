// Command videoencoder turns videos into DASH/HLS streams, rebuilding
// only renditions whose source or settings changed.
//
//	videoencoder [flags] [dir | name.video.yaml ...]
//	videoencoder [flags] video.mov [other-version.mov ...]
//	videoencoder plan [flags] [same arguments]
//
// With directories (default "."), it renders every video defined under
// them by a <name>.video.yaml sidecar or a videos: entry in a
// videoencoder.yaml. With media files, they are versions of one video
// (e.g. HDR and SDR), all offered in one stream; audio and the poster
// come from the first. "plan" prints each video's merged config, with
// where each value came from, and which renditions would be rebuilt.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/scottlaird/videoencoder/internal/config"
	"github.com/scottlaird/videoencoder/internal/ffmpeg"
	"github.com/scottlaird/videoencoder/internal/pack"
	"github.com/scottlaird/videoencoder/internal/plan"
	"github.com/scottlaird/videoencoder/internal/probe"
	"github.com/scottlaird/videoencoder/internal/state"
)

type options struct {
	plan       bool
	globalPath string
	overrides  map[string]any
	paths      []string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	o := options{overrides: map[string]any{}}
	if len(args) > 0 && args[0] == "plan" {
		o.plan, args = true, args[1:]
	}
	fs := flag.NewFlagSet("videoencoder", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.globalPath, "config", config.DefaultGlobalPath(), "global config file")
	outdir := fs.String("outdir", "", "override outdir: directory for intermediate renditions")
	streamdir := fs.String("streamdir", "", "override streamdir: directory for the finished stream")
	thumbnails := fs.Bool("thumbnails", false, "override thumbnails: generate seek thumbnails (not yet supported by shaka-packager)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "outdir":
			o.overrides["outdir"] = absOrSame(*outdir)
		case "streamdir":
			o.overrides["streamdir"] = absOrSame(*streamdir)
		case "thumbnails":
			o.overrides["thumbnails"] = *thumbnails
		}
	})
	o.paths = fs.Args()
	if len(o.paths) == 0 {
		o.paths = []string{"."}
	}
	return o, nil
}

// absOrSame makes a command-line path absolute, since config paths are
// otherwise relative to each video's directory, not the working
// directory.
func absOrSame(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

type deps struct {
	runner ffmpeg.Runner
	prober probe.Prober
	stdout io.Writer
}

// item is one video to process, with the loader for its config.
type item struct {
	loader config.Loader
	video  config.Video
}

// items resolves the command-line paths into videos. Directories and
// sidecars are searched for configured videos; any other files are
// sources of one ad hoc video.
func items(paths []string, base config.Loader) ([]item, error) {
	var out []item
	var files []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() && !strings.HasSuffix(p, config.SidecarSuffix) {
			files = append(files, p)
			continue
		}
		found, err := config.Discover(p)
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			slog.Warn("No videos defined", "path", p)
		}
		for _, v := range found {
			out = append(out, item{base, v})
		}
	}
	if len(files) > 0 {
		v, sources, err := config.AdHoc(files)
		if err != nil {
			return nil, err
		}
		l := base
		l.Overrides = maps.Clone(base.Overrides)
		maps.Copy(l.Overrides, sources)
		out = append(out, item{l, v})
	}
	return out, nil
}

func run(ctx context.Context, o options, d deps) error {
	its, err := items(o.paths, config.Loader{GlobalPath: o.globalPath, Overrides: o.overrides})
	if err != nil {
		return err
	}
	for _, it := range its {
		r, err := it.loader.Load(it.video)
		if err != nil {
			return err
		}
		if o.plan {
			err = printPlan(ctx, r, d)
		} else {
			err = render(ctx, r, d)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", it.video.Name, err)
		}
	}
	return nil
}

// jobs returns the encode jobs for every source of r, by source, in
// the same order as the targets they come from.
func jobs(ctx context.Context, r *config.Resolved, d deps) (probe.VideoData, plan.Targets, [][]*ffmpeg.Job, error) {
	labels := r.SourceLabels()
	vd, err := d.prober.Probe(ctx, r.Sources[labels[0]])
	if err != nil {
		return vd, nil, nil, err
	}
	ladder, err := plan.FromConfig(&r.Config)
	if err != nil {
		return vd, nil, nil, err
	}
	targets := ladder.Filter(vd)

	var all [][]*ffmpeg.Job
	for seq, label := range labels {
		src := r.Sources[label]
		var js []*ffmpeg.Job
		for _, t := range targets {
			js = append(js, t.Job(src, r.Outdir, seq))
		}
		all = append(all, js)
	}
	return vd, targets, all, nil
}

func render(ctx context.Context, r *config.Resolved, d deps) error {
	vd, _, perSource, err := jobs(ctx, r, d)
	if err != nil {
		return err
	}
	labels := r.SourceLabels()
	first := r.Sources[labels[0]]
	slog.Info("Source", "video", r.Name, "file", first, "video", fmt.Sprintf("%+v", vd))

	var videos []string
	for i, js := range perSource {
		var batch ffmpeg.Batch
		for _, j := range js {
			if _, err := batch.AddIfNeeded(j); err != nil {
				return err
			}
			videos = append(videos, j.Output)
		}
		if r.Thumbnails && i == 0 {
			if _, err := batch.AddIfNeeded(plan.SeekThumbnailJob(vd, first, r.Outdir)); err != nil {
				return err
			}
		}
		if err := batch.Run(ctx, d.runner); err != nil {
			return fmt.Errorf("encoding %q: %w", r.Sources[labels[i]], err)
		}
	}

	audio := plan.AudioJob(first, r.Outdir)
	if err := ffmpeg.RunIfNeeded(ctx, d.runner, audio); err != nil {
		return fmt.Errorf("extracting audio: %w", err)
	}
	poster := plan.PosterJob(first, r.Outdir)
	if err := ffmpeg.RunIfNeeded(ctx, d.runner, poster); err != nil {
		return fmt.Errorf("making poster: %w", err)
	}

	p := &pack.Packager{Runner: d.runner, Prober: d.prober, Thumbnails: r.Thumbnails}
	mpd, err := p.Package(ctx, r.Streamdir, videos, audio.Output)
	if err != nil {
		return fmt.Errorf("packaging: %w", err)
	}

	streamPoster := filepath.Join(r.Streamdir, filepath.Base(poster.Output))
	if err := copySmallFile(poster.Output, streamPoster); err != nil {
		return err
	}
	slog.Info("Done", "video", r.Name, "manifest", mpd, "poster", streamPoster)
	return nil
}

func printPlan(ctx context.Context, r *config.Resolved, d deps) error {
	w := d.stdout
	fmt.Fprintf(w, "# %s in %s\n", r.Name, r.Dir)
	merged, err := yaml.Dump(r.Merged)
	if err != nil {
		return err
	}
	w.Write(merged)

	_, targets, perSource, err := jobs(ctx, r, d)
	if err != nil {
		return err
	}
	labels := r.SourceLabels()
	fmt.Fprintln(w, "# renditions:")
	for i, js := range perSource {
		for n, j := range js {
			reason, err := state.Needs(j.Input, j.Output, j.Cmdline())
			if err != nil {
				return err
			}
			if reason == "" {
				reason = "up to date"
			} else {
				reason = "rebuild: " + reason
			}
			t := targets[n]
			fmt.Fprintf(w, "#   %s %s %dx%d -> %s: %s\n", labels[i], t.Name, t.Width, t.Height, filepath.Base(j.Output), reason)
		}
	}
	fmt.Fprintln(w)
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
		stdout: os.Stdout,
	}
	if err := run(ctx, o, d); err != nil {
		fmt.Fprintln(os.Stderr, "videoencoder:", err)
		os.Exit(1)
	}
}
