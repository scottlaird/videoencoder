// Command videoencoder turns videos into DASH/HLS streams, rebuilding
// only renditions whose source or settings changed.
//
//	videoencoder [flags] [dir | name.video.yaml ...]
//	videoencoder [flags] video.mov [other-version.mov ...]
//	videoencoder plan [flags] [same arguments]
//	videoencoder publish [flags] [same arguments]
//	videoencoder tune [flags] [same arguments]
//
// With directories (default "."), it renders every video defined under
// them by a <name>.video.yaml sidecar or a videos: entry in a
// videoencoder.yaml. With media files, they are versions of one video
// (e.g. HDR and SDR), all offered in one stream; audio comes from the
// first, and posters from the first of each tone. "plan" prints each video's merged config, with
// where each value came from, and which renditions would be rebuilt.
// After rendering, the stream is published to the config's publish.to
// targets; "publish" does only that, for streams already rendered.
// "tune" encodes a short sample of each video to find the quality
// setting each rendition needs to reach a target VMAF, and writes the
// results into the video's sidecar.
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

	"github.com/scottlaird/videoencoder/internal/bench"
	"github.com/scottlaird/videoencoder/internal/config"
	"github.com/scottlaird/videoencoder/internal/ffmpeg"
	"github.com/scottlaird/videoencoder/internal/pack"
	"github.com/scottlaird/videoencoder/internal/plan"
	"github.com/scottlaird/videoencoder/internal/probe"
	"github.com/scottlaird/videoencoder/internal/state"
)

type options struct {
	mode       string // "render", "plan", "publish" or "tune"
	publish    bool   // publish after rendering
	prune      bool
	rebuild    string // glob over slots, such as "*/av1-*"
	globalPath string
	overrides  map[string]any
	paths      []string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	o := options{mode: "render", overrides: map[string]any{}}
	if len(args) > 0 && slices.Contains([]string{"plan", "publish", "tune"}, args[0]) {
		o.mode, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("videoencoder", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.globalPath, "config", config.DefaultGlobalPath(), "global config file")
	fs.BoolVar(&o.publish, "publish", true, "publish to the publish.to targets after rendering")
	fs.BoolVar(&o.prune, "prune", false, "when publishing, delete objects that are no longer part of the stream")
	to := fs.String("to", "", "override publish.to: comma-separated target names")
	fs.StringVar(&o.rebuild, "rebuild", "", `rebuild matching outputs even if up to date: "*" for all, "av1-*", "hdr/h264-1080", "audio", "poster/*"`)
	outdir := fs.String("outdir", "", "override outdir: directory for intermediate renditions")
	streamdir := fs.String("streamdir", "", "override streamdir: directory for the finished stream")
	thumbnails := fs.Bool("thumbnails", true, "override thumbnails.enabled: add seek thumbnails to the stream")
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
			o.overrides["thumbnails"] = map[string]any{"enabled": *thumbnails}
		case "to":
			names := []string{}
			for _, n := range strings.Split(*to, ",") {
				if n = strings.TrimSpace(n); n != "" {
					names = append(names, n)
				}
			}
			o.overrides["publish"] = map[string]any{"to": names}
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
	exec   bench.Executor // for measured commands: tune's encodes and scorings
	prober probe.Prober
	stdout io.Writer
}

// item is one video to process, with the loader for its config.
type item struct {
	loader config.Loader
	video  config.Video
	adHoc  bool // given as media files, not defined by config
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
			out = append(out, item{loader: base, video: v})
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
		out = append(out, item{loader: l, video: v, adHoc: true})
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
		switch o.mode {
		case "plan":
			err = printPlan(ctx, r, d)
		case "publish":
			err = publishVideo(ctx, r, o.prune)
		case "tune":
			err = tuneVideo(ctx, r, it.adHoc, d)
		default:
			err = render(ctx, r, d, o.rebuild)
			if err == nil && o.publish {
				err = publishVideo(ctx, r, o.prune)
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", it.video.Name, err)
		}
	}
	return nil
}

// built is everything a video's outputs are made from.
type built struct {
	vd        probe.VideoData // of the first source
	sources   []plan.Source   // in encoding order; the first supplies audio
	vds       []probe.VideoData
	targets   plan.Targets
	perSource [][]*ffmpeg.Job // rendition jobs, in target order
	audio     *ffmpeg.Job     // nil for a silent video
	posters   []plan.Poster
	gainmaps  []plan.GainMapPoster
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
	if b.audio != nil {
		js = append(js, b.audio)
	}
	for _, p := range b.posters {
		js = append(js, p.Job)
	}
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
		vd, err := d.prober.Probe(ctx, src.Path)
		if err != nil {
			return nil, err
		}
		if off, _ := config.ParseOffset(r.Poster.Offset); vd.Duration > 0 && off >= vd.Duration {
			return nil, fmt.Errorf("poster.offset %s is past the end of %s (%v)", r.Poster.Offset, src.Path, vd.Duration)
		}
		b.sources = append(b.sources, src)
		b.vds = append(b.vds, vd)
	}
	first := b.sources[0]
	b.vd = b.vds[0]
	var err error
	ladder, err := plan.FromConfig(&r.Config)
	if err != nil {
		return nil, err
	}
	b.targets = ladder.Filter(b.vd)
	sdr, _ := toneSources(b.vds)
	hasSDR := b.vds[sdr].Tone() != probe.HDR
	for i, src := range b.sources {
		hdr := b.vds[i].Tone() == probe.HDR
		var js []*ffmpeg.Job
		for _, t := range b.targets {
			// SDR-only encoders skip HDR sources, unless there is no
			// SDR source, in which case they tone-map it.
			toneMap := t.Encoder.SDROnly && hdr
			if toneMap && hasSDR {
				continue
			}
			j := t.Job(src, r.Outdir, toneMap)
			desc := fmt.Sprintf("%s/%s %dx%d", src.Label, t.Name, t.Width, t.Height)
			if toneMap {
				desc += " tone-mapped"
			}
			b.desc[j] = desc
			js = append(js, j)
		}
		b.perSource = append(b.perSource, js)
	}
	if b.vd.HasAudio {
		b.audio = plan.AudioJob(first, r.Outdir)
	}
	b.posters, b.gainmaps = posters(r.Poster, b.sources, b.vds, r.Outdir)
	if err := checkGainMapTools(b.gainmaps); err != nil {
		return nil, err
	}
	for _, p := range b.posters {
		b.desc[p.Job] = fmt.Sprintf("%s %dx%d from %s", p.Job.Slot, p.Width, p.Height, p.Source)
	}
	if r.Thumbnails.Enabled {
		b.thumbs = plan.SeekThumbnailJob(r.Thumbnails, b.vd, first, r.Outdir)
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
	audio := ""
	if b.audio != nil {
		if err := ffmpeg.RunIfNeeded(ctx, d.runner, b.manifest, b.audio, forced(rebuild, b.audio)); err != nil {
			return fmt.Errorf("extracting audio: %w", err)
		}
		audio = b.audio.Output
	}
	if err := runPosters(ctx, d, b, rebuild); err != nil {
		return fmt.Errorf("making posters: %w", err)
	}
	if err := buildGainMaps(ctx, d, b, rebuild); err != nil {
		return fmt.Errorf("making gain-mapped posters: %w", err)
	}

	p := &pack.Packager{Runner: d.runner, Prober: d.prober}
	mpd, files, err := p.Package(ctx, r.Streamdir, videos, audio)
	if err != nil {
		return fmt.Errorf("packaging: %w", err)
	}
	if b.thumbs != nil {
		thumbFiles, err := publishThumbnails(r, b)
		if err != nil {
			return fmt.Errorf("adding seek thumbnails: %w", err)
		}
		files = append(files, thumbFiles...)
	}
	sdr, _ := toneSources(b.vds)
	posterFiles, err := publishPosters(r.Streamdir, r.Poster.Offset, b.vds[sdr].Width, b.posters, b.gainmaps)
	if err != nil {
		return fmt.Errorf("publishing posters: %w", err)
	}
	files = append(files, posterFiles...)

	// Only now that the new stream is complete, drop what it replaced.
	var slots []string
	for _, j := range b.all() {
		slots = append(slots, j.Slot)
	}
	for _, g := range b.gainmaps {
		slots = append(slots, g.Slot)
	}
	if err := errors.Join(
		b.manifest.SetStream(r.Streamdir, files),
		b.manifest.Prune(slots),
		b.manifest.Save(),
	); err != nil {
		return err
	}
	slog.Info("Done", "video", r.Name, "manifest", mpd)
	return nil
}

// runPosters builds the stale posters, one ffmpeg per source.
func runPosters(ctx context.Context, d deps, b *built, rebuild string) error {
	batches := map[string]*ffmpeg.Batch{}
	for _, p := range b.posters {
		batch, ok := batches[p.Job.Input]
		if !ok {
			batch = &ffmpeg.Batch{Manifest: b.manifest}
			batches[p.Job.Input] = batch
		}
		if forced(rebuild, p.Job) {
			batch.Add(p.Job)
		} else if _, err := batch.AddIfNeeded(p.Job); err != nil {
			return err
		}
	}
	for _, src := range b.sources {
		if batch, ok := batches[src.Path]; ok {
			if err := batch.Run(ctx, d.runner); err != nil {
				return err
			}
		}
	}
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
	for _, g := range b.gainmaps {
		_, err := os.Stat(g.Output)
		reason := b.manifest.Explain(g.Slot, g.Key, g.SourceID(), err == nil)
		if reason == state.UpToDate {
			reason = "up to date"
		} else {
			reason = "build: " + reason
		}
		fmt.Fprintf(w, "#   %s -> %s: %s\n", gainMapDesc(g), filepath.Base(g.Output), reason)
	}
	fmt.Fprintln(w)
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
		exec:   bench.ExecExecutor{Stderr: os.Stderr},
		prober: probe.FFProbe{},
		stdout: os.Stdout,
	}
	if err := run(ctx, o, d); err != nil {
		fmt.Fprintln(os.Stderr, "videoencoder:", err)
		os.Exit(1)
	}
}
