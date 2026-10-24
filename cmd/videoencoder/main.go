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
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

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
	rebuild    string // glob over slots, such as "*/av1-*"
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
	fs.StringVar(&o.rebuild, "rebuild", "", `rebuild matching outputs even if up to date: "*" for all, "av1-*", "hdr/h264-1080", "audio", "poster"`)
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
			err = render(ctx, r, d, o.rebuild)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", it.video.Name, err)
		}
	}
	return nil
}

// built is everything a video's outputs are made from.
type built struct {
	vd        probe.VideoData
	sources   []plan.Source // in encoding order; the first supplies audio and poster
	targets   plan.Targets
	perSource [][]*ffmpeg.Job // rendition jobs, in target order
	audio     *ffmpeg.Job
	poster    *ffmpeg.Job
	thumbs    *ffmpeg.Job // nil unless thumbnails are on
	manifest  *state.Manifest
	desc      map[*ffmpeg.Job]string // for plan output
}

// all returns every job, in a stable order.
func (b *built) all() []*ffmpeg.Job {
	var js []*ffmpeg.Job
	for _, s := range b.perSource {
		js = append(js, s...)
	}
	js = append(js, b.audio, b.poster)
	if b.thumbs != nil {
		js = append(js, b.thumbs)
	}
	return js
}

func prepare(ctx context.Context, r *config.Resolved, d deps) (*built, error) {
	b := &built{desc: map[*ffmpeg.Job]string{}}
	for _, label := range r.SourceLabels() {
		src, err := plan.NewSource(label, r.Sources[label])
		if err != nil {
			return nil, err
		}
		b.sources = append(b.sources, src)
	}
	first := b.sources[0]
	var err error
	if b.vd, err = d.prober.Probe(ctx, first.Path); err != nil {
		return nil, err
	}
	ladder, err := plan.FromConfig(&r.Config)
	if err != nil {
		return nil, err
	}
	b.targets = ladder.Filter(b.vd)
	for _, src := range b.sources {
		var js []*ffmpeg.Job
		for _, t := range b.targets {
			j := t.Job(src, r.Outdir)
			b.desc[j] = fmt.Sprintf("%s/%s %dx%d", src.Label, t.Name, t.Width, t.Height)
			js = append(js, j)
		}
		b.perSource = append(b.perSource, js)
	}
	b.audio = plan.AudioJob(first, r.Outdir)
	b.poster = plan.PosterJob(first, r.Outdir)
	if r.Thumbnails {
		b.thumbs = plan.SeekThumbnailJob(b.vd, first, r.Outdir)
	}
	b.manifest, err = state.Load(r.Outdir, r.Name)
	return b, err
}

// forced reports whether the --rebuild glob matches j's slot. A glob
// without a "/" matches the part after the source label, so "av1-*"
// covers every source's AV1 renditions and "*" covers everything.
func forced(rebuild string, j *ffmpeg.Job) bool {
	if rebuild == "" {
		return false
	}
	name := j.Slot
	if !strings.Contains(rebuild, "/") {
		name = path.Base(name)
	}
	ok, _ := path.Match(rebuild, name)
	return ok
}

func render(ctx context.Context, r *config.Resolved, d deps, rebuild string) error {
	b, err := prepare(ctx, r, d)
	if err != nil {
		return err
	}
	slog.Info("Source", "video", r.Name, "file", b.sources[0].Path, "video", fmt.Sprintf("%+v", b.vd))

	var videos []string
	for i, js := range b.perSource {
		batch := ffmpeg.Batch{Manifest: b.manifest}
		if i == 0 && b.thumbs != nil {
			js = append(slices.Clone(js), b.thumbs)
		}
		for _, j := range js {
			if forced(rebuild, j) {
				batch.Add(j)
			} else if _, err := batch.AddIfNeeded(j); err != nil {
				return err
			}
			if j != b.thumbs {
				videos = append(videos, j.Output)
			}
		}
		if err := batch.Run(ctx, d.runner); err != nil {
			return fmt.Errorf("encoding %q: %w", b.sources[i].Path, err)
		}
	}
	if err := ffmpeg.RunIfNeeded(ctx, d.runner, b.manifest, b.audio, forced(rebuild, b.audio)); err != nil {
		return fmt.Errorf("extracting audio: %w", err)
	}
	if err := ffmpeg.RunIfNeeded(ctx, d.runner, b.manifest, b.poster, forced(rebuild, b.poster)); err != nil {
		return fmt.Errorf("making poster: %w", err)
	}

	p := &pack.Packager{Runner: d.runner, Prober: d.prober, Thumbnails: r.Thumbnails}
	mpd, files, err := p.Package(ctx, r.Streamdir, videos, b.audio.Output)
	if err != nil {
		return fmt.Errorf("packaging: %w", err)
	}
	streamPoster := filepath.Join(r.Streamdir, "thumbnail.jpg")
	if err := copySmallFile(b.poster.Output, streamPoster); err != nil {
		return err
	}
	files = append(files, filepath.Base(streamPoster))

	// Only now that the new stream is complete, drop what it replaced.
	var slots []string
	for _, j := range b.all() {
		slots = append(slots, j.Slot)
	}
	if err := errors.Join(
		b.manifest.SetStream(r.Streamdir, files),
		b.manifest.Prune(slots),
		b.manifest.Save(),
	); err != nil {
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

	b, err := prepare(ctx, r, d)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "# outputs:")
	for _, j := range b.all() {
		desc, ok := b.desc[j]
		if !ok {
			desc = j.Slot
		}
		_, err := os.Stat(j.Result())
		reason := b.manifest.Explain(j.Slot, j.Key, j.Source, err == nil)
		if reason == state.UpToDate {
			reason = "up to date"
		} else {
			reason = "build: " + reason
		}
		fmt.Fprintf(w, "#   %s -> %s: %s\n", desc, filepath.Base(j.Result()), reason)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
