// Command videoencoder turns videos into DASH/HLS streams, rebuilding
// only renditions whose source or settings changed.
//
//	videoencoder [flags] [dir | name.video.yaml ...]
//	videoencoder [flags] video.mov [other-version.mov ...]
//	videoencoder plan [flags] [same arguments]
//	videoencoder publish [flags] [same arguments]
//	videoencoder tune [flags] [same arguments]
//	videoencoder help [command]
//
// With directories (default "."), it renders every video defined under
// them by a <name>.video.yaml sidecar or a videos: entry in a
// videoencoder.yaml. With media files, they are versions of one video
// (e.g. HDR and SDR), all offered in one stream; audio comes from the
// first, posters from the first of each tone, and seek thumbnails from
// the first SDR one. "plan" prints each video's merged config, with
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
	"github.com/scottlaird/videoencoder/internal/progress"
	"github.com/scottlaird/videoencoder/internal/state"
	"github.com/scottlaird/videoencoder/internal/tui"
)

type options struct {
	mode       string // "render", "plan", "publish" or "tune"
	publish    bool   // publish after rendering
	prune      bool
	rebuild    string // glob over slots, such as "*/av1-*"
	only       string // glob over slots: build just these, without packaging
	globalPath string
	tui        string // "auto", "on" or "off"
	overrides  map[string]any
	paths      []string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	o := options{mode: "render", overrides: map[string]any{}}
	help := false
	if len(args) > 0 && args[0] == "help" {
		help, args = true, args[1:]
	}
	if len(args) > 0 && slices.Contains([]string{"plan", "publish", "tune"}, args[0]) {
		o.mode, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("videoencoder", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { printUsage(stderr, fs, o.mode, o.mode == "render") }
	fs.StringVar(&o.globalPath, "config", config.DefaultGlobalPath(), "global config file")
	fs.BoolVar(&o.publish, "publish", true, "publish to the publish.to targets after rendering")
	fs.BoolVar(&o.prune, "prune", false, "when publishing, delete objects that are no longer part of the stream")
	to := fs.String("to", "", "override publish.to: comma-separated target names")
	fs.StringVar(&o.rebuild, "rebuild", "", `rebuild matching outputs even if up to date: "*" for all, "av1-*", "hdr/h264-1080", "audio", "poster/*"`)
	fs.StringVar(&o.only, "only", "", `build only matching outputs, as --rebuild matches them, and skip packaging and publishing: "hdr/av1-2160"`)
	outdir := fs.String("outdir", "", "override outdir: directory for intermediate renditions")
	streamdir := fs.String("streamdir", "", "override streamdir: directory for the finished stream")
	thumbnails := fs.Bool("thumbnails", true, "override thumbnails.enabled: add seek thumbnails to the stream")
	fs.StringVar(&o.tui, "tui", "auto", `show progress in a terminal UI: "auto" when stderr is a terminal, "on" or "off"`)
	jobs := fs.Int("jobs", 0, "override tune.jobs: anchor searches tune runs at once (0 for one per 8 CPU threads)")
	if help {
		switch {
		case len(args) == 0:
		case args[0] == "render":
			o.mode = "render"
			printUsage(stderr, fs, o.mode, false)
			return o, flag.ErrHelp
		default:
			return o, fmt.Errorf("help: no command %q; the commands are plan, publish and tune", args[0])
		}
		printUsage(stderr, fs, o.mode, o.mode == "render")
		return o, flag.ErrHelp
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if err := checkFlags(fs, o.mode); err != nil {
		return o, err
	}
	if !slices.Contains([]string{"auto", "on", "off"}, o.tui) {
		return o, fmt.Errorf(`--tui: want "auto", "on" or "off", not %q`, o.tui)
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "outdir":
			o.overrides["outdir"] = absOrSame(*outdir)
		case "streamdir":
			o.overrides["streamdir"] = absOrSame(*streamdir)
		case "thumbnails":
			o.overrides["thumbnails"] = map[string]any{"enabled": *thumbnails}
		case "jobs":
			o.overrides["tune"] = map[string]any{"jobs": *jobs}
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
		progress.QueueVideo(ctx, it.video.Name)
	}
	for _, it := range its {
		progress.StartVideo(ctx, it.video.Name)
		err := runOne(ctx, o, d, it)
		progress.FinishVideo(ctx, it.video.Name, err)
		if err != nil {
			return fmt.Errorf("%s: %w", it.video.Name, err)
		}
	}
	return nil
}

// runOne does o.mode's work for one video.
func runOne(ctx context.Context, o options, d deps, it item) error {
	r, err := it.loader.Load(it.video)
	if err != nil {
		return err
	}
	switch o.mode {
	case "plan":
		return printPlan(ctx, r, d, o.only)
	case "publish":
		return publishTask(ctx, r, o.prune)
	case "tune":
		return tuneVideo(ctx, r, it.adHoc, d)
	}
	// With --only the stream isn't updated, so there's nothing new to
	// publish.
	publishing := o.publish && o.only == "" && len(r.Publish.To) > 0
	if err := render(ctx, r, d, o.rebuild, o.only, publishing); err != nil {
		return err
	}
	if publishing {
		return publishTask(ctx, r, o.prune)
	}
	return nil
}

// publishTask publishes r as a reported task.
func publishTask(ctx context.Context, r *config.Resolved, prune bool) error {
	if len(r.Publish.To) == 0 {
		return nil
	}
	progress.StartTask(ctx, r.Name, "publish", strings.Join(r.Publish.To, ", "))
	detail, err := publishVideo(ctx, r, prune)
	progress.FinishTask(ctx, r.Name, "publish", detail, err)
	return err
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
	thumbSrc  int         // index of the source thumbs is taken from
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
			tt := t
			if hdr && !toneMap {
				tt = t.ForHDR()
			}
			j := tt.Job(src, r.Outdir, toneMap)
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
		// Seek thumbnails are SDR, so take them from the SDR source.
		b.thumbSrc = sdr
		b.thumbs = plan.SeekThumbnailJob(r.Thumbnails, b.vds[sdr], b.sources[sdr], r.Outdir)
	}
	b.manifest, err = state.Load(r.Outdir, r.Name)
	return b, err
}

// forced reports whether the --rebuild glob matches j's slot.
func forced(rebuild string, j *ffmpeg.Job) bool {
	return rebuild != "" && matches(rebuild, j.Slot)
}

// matches reports whether glob matches slot. A glob without a "/"
// matches the part after the source label, so "av1-*" covers every
// source's AV1 renditions and "*" covers everything.
func matches(glob, slot string) bool {
	name := slot
	if !strings.Contains(glob, "/") {
		name = path.Base(name)
	}
	ok, _ := path.Match(glob, name)
	return ok
}

// restrict drops the outputs whose slots don't match the --only glob,
// returning an error if none is left.
func (b *built) restrict(only string) error {
	drop := func(slot string) bool { return !matches(only, slot) }
	n := 0
	for i, js := range b.perSource {
		b.perSource[i] = slices.DeleteFunc(slices.Clone(js), func(j *ffmpeg.Job) bool { return drop(j.Slot) })
		n += len(b.perSource[i])
	}
	if b.audio != nil && drop(b.audio.Slot) {
		b.audio = nil
	}
	if b.thumbs != nil && drop(b.thumbs.Slot) {
		b.thumbs = nil
	}
	b.posters = slices.DeleteFunc(b.posters, func(p plan.Poster) bool { return drop(p.Job.Slot) })
	b.gainmaps = slices.DeleteFunc(b.gainmaps, func(g plan.GainMapPoster) bool { return drop(g.Slot) })
	if b.audio != nil {
		n++
	}
	if b.thumbs != nil {
		n++
	}
	if n += len(b.posters) + len(b.gainmaps); n == 0 {
		return fmt.Errorf("--only %q matches no outputs", only)
	}
	return nil
}

// render builds r's stale outputs and packages them into its stream.
// With only, it builds just the outputs that glob matches and stops
// there: packaging needs every rendition, and pruning would delete the
// ones left out. The next full render updates the stream.
// render builds r's stale outputs and packages its stream. Each output
// and step is reported as a task, queued up front so the whole job is
// visible from the start; publishing, which the caller does, is queued
// too when publishing is set.
func render(ctx context.Context, r *config.Resolved, d deps, rebuild, only string, publishing bool) error {
	b, err := prepare(ctx, r, d)
	if err != nil {
		return err
	}
	if only != "" {
		if err := b.restrict(only); err != nil {
			return err
		}
	}
	slog.Info("Source", "video", r.Name, "file", b.sources[0].Path, "video", fmt.Sprintf("%+v", b.vd))

	// Decide every source's batch first, so the encodes still to come
	// can be reported before any starts.
	var videos []string
	batches := make([]*ffmpeg.Batch, len(b.perSource))
	for i, js := range b.perSource {
		batch := &ffmpeg.Batch{Manifest: b.manifest}
		batches[i] = batch
		if i == b.thumbSrc && b.thumbs != nil {
			js = append(slices.Clone(js), b.thumbs)
		}
		for _, j := range js {
			added := true
			if forced(rebuild, j) {
				batch.Add(j)
			} else if added, err = batch.AddIfNeeded(j); err != nil {
				return err
			}
			if added {
				progress.QueueTask(ctx, r.Name, j.Slot, "")
			} else {
				progress.SkipTask(ctx, r.Name, j.Slot, "up to date")
			}
			if j != b.thumbs {
				videos = append(videos, j.Output)
			}
		}
	}
	steps := []string{}
	if b.audio != nil {
		steps = append(steps, "audio")
	}
	steps = append(steps, "posters")
	if len(b.gainmaps) > 0 {
		steps = append(steps, "gain maps")
	}
	if only == "" {
		steps = append(steps, "package")
		if publishing {
			steps = append(steps, "publish")
		}
	}
	for _, s := range steps {
		progress.QueueTask(ctx, r.Name, s, "")
	}

	for i, batch := range batches {
		for _, j := range batch.Jobs {
			progress.StartTask(ctx, r.Name, j.Slot, fmt.Sprintf("with %d others from %s", len(batch.Jobs)-1, b.sources[i].Label))
		}
		err := batch.Run(ctx, d.runner)
		for _, j := range batch.Jobs {
			progress.FinishTask(ctx, r.Name, j.Slot, "", err)
		}
		if err != nil {
			return fmt.Errorf("encoding %q: %w", b.sources[i].Path, err)
		}
	}
	audio := ""
	if b.audio != nil {
		err := step(ctx, r.Name, "audio", func() error {
			return ffmpeg.RunIfNeeded(ctx, d.runner, b.manifest, b.audio, forced(rebuild, b.audio))
		})
		if err != nil {
			return fmt.Errorf("extracting audio: %w", err)
		}
		audio = b.audio.Output
	}
	if err := step(ctx, r.Name, "posters", func() error { return runPosters(ctx, d, b, rebuild) }); err != nil {
		return fmt.Errorf("making posters: %w", err)
	}
	if len(b.gainmaps) > 0 {
		if err := step(ctx, r.Name, "gain maps", func() error { return buildGainMaps(ctx, d, b, rebuild) }); err != nil {
			return fmt.Errorf("making gain-mapped posters: %w", err)
		}
	}
	if only != "" {
		slog.Info("Built only matching outputs; the stream is unchanged", "video", r.Name, "only", only)
		return nil
	}

	progress.StartTask(ctx, r.Name, "package", "")
	p := &pack.Packager{Runner: d.runner, Prober: d.prober, Segment: r.Segment()}
	mpd, files, err := p.Package(ctx, r.Streamdir, videos, audio)
	progress.FinishTask(ctx, r.Name, "package", "", err)
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
	if r.CopySources {
		srcFiles, err := copySources(r.Streamdir, b.sources)
		if err != nil {
			return err
		}
		files = append(files, srcFiles...)
	}

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

// step runs f as task, part of video, reporting its start and end.
func step(ctx context.Context, video, task string, f func() error) error {
	progress.StartTask(ctx, video, task, "")
	err := f()
	progress.FinishTask(ctx, video, task, "", err)
	return err
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

func printPlan(ctx context.Context, r *config.Resolved, d deps, only string) error {
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
	if only != "" {
		if err := b.restrict(only); err != nil {
			return err
		}
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
	if r.CopySources {
		for _, src := range b.sources {
			fmt.Fprintf(w, "#   %s source -> %s: copied to the stream\n", src.Label, filepath.Base(src.Path))
		}
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

	newDeps := func(stdout, stderr io.Writer) deps {
		return deps{
			runner: ffmpeg.ExecRunner{Stdout: stderr, Stderr: stderr},
			exec:   bench.ExecExecutor{Stderr: stderr},
			prober: probe.FFProbe{},
			stdout: stdout,
		}
	}
	// plan prints its own report. Everything else shows its progress in
	// a terminal UI when it can, and otherwise logs where it is in the
	// run.
	useTUI := o.mode != "plan" && (o.tui == "on" || (o.tui == "auto" && tui.Available(os.Stderr)))
	switch {
	case useTUI:
		logDir := ""
		if cache, err := os.UserCacheDir(); err == nil {
			logDir = filepath.Join(cache, "videoencoder", "logs")
		}
		title := "videoencoder " + o.mode
		err = tui.Run(ctx, title, os.Stderr, os.Stdout, logDir, func(ctx context.Context, out tui.Outputs) error {
			prev := slog.Default()
			slog.SetDefault(slog.New(tui.LogHandler(out.Log)))
			defer slog.SetDefault(prev)
			return run(ctx, o, newDeps(out.Stdout, out.Log))
		})
	case o.mode != "plan":
		ctx = progress.WithReporter(ctx, &progress.LogReporter{})
		fallthrough
	default:
		err = run(ctx, o, newDeps(os.Stdout, os.Stderr))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "videoencoder:", err)
		os.Exit(1)
	}
}
