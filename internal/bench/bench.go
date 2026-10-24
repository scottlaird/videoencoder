package bench

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/scottlaird/videoencoder/internal/fileid"
	"github.com/scottlaird/videoencoder/internal/plan"
	"github.com/scottlaird/videoencoder/internal/probe"
)

// Bench runs the searches described by a Config.
type Bench struct {
	Cfg    *Config
	Exec   Executor
	Prober probe.Prober
	// Host and FFmpegVersion are recorded with each result.
	Host          string
	FFmpegVersion string
	// SampleFilter and FlagsetFilter, if set, are path.Match patterns
	// limiting which samples and flagsets run.
	SampleFilter, FlagsetFilter string
	// DryRun reports what would be encoded without encoding anything.
	DryRun bool
	Now    func() time.Time
}

type sample struct {
	name string // file name, used in results and keeper paths
	path string
	id   string
	vd   probe.VideoData
	// prefilter is applied to both sides before scoring: a tone map to
	// SDR for HDR samples, since VMAF and CIEDE2000 are SDR metrics.
	prefilter string
}

// task is the search for one flagset on one sample.
type task struct {
	sample  *sample
	name    string
	fs      *Flagset
	fkey    string
	search  *Search
	outcome Outcome
	encoded map[int]string // files encoded this run, by index
}

// pending is an encode a task is waiting on.
type pending struct {
	t       *task
	i       int
	quality float64
	key     string
	out     string
	m       Measurement
	bytes   int64
	metrics Metrics
	err     error
}

func (b *Bench) resultsPath() string { return filepath.Join(b.Cfg.Workdir, "results.csv") }
func (b *Bench) bestPath() string    { return filepath.Join(b.Cfg.Workdir, "best.csv") }
func (b *Bench) scratchDir() string  { return filepath.Join(b.Cfg.Workdir, "tmp") }
func (b *Bench) keepDir() string     { return filepath.Join(b.Cfg.Workdir, "keep") }

// Run searches every (sample, flagset) pair that isn't finished,
// reusing logged encodes, then rewrites best.csv.
//
// Encodes run one at a time so their timings are comparable; scoring
// runs in parallel, but only while no encode is running.
func (b *Bench) Run(ctx context.Context) error {
	for _, d := range []string{b.Cfg.Workdir, b.scratchDir(), b.keepDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	results, err := LoadResults(b.resultsPath())
	if err != nil {
		return err
	}
	samples, err := b.loadSamples(ctx)
	if err != nil {
		return err
	}
	tasks := b.tasks(samples, results)

	for round := 1; ; round++ {
		batch := b.advance(tasks, results)
		if len(batch) == 0 {
			break
		}
		if b.DryRun {
			for _, p := range batch {
				slog.Info("Would encode", "sample", p.t.sample.name, "flagset", p.t.name, "quality", p.quality)
				p.t.outcome = Searching
				p.t.search = nil
			}
			continue
		}
		slog.Info("Round", "n", round, "encodes", len(batch))
		if err := b.encodeAll(ctx, batch); err != nil {
			return err
		}
		err := b.scoreAll(ctx, batch, func(p *pending) error { return b.record(results, p) })
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return WriteBest(b.bestPath(), b.Cfg, results)
}

func (b *Bench) loadSamples(ctx context.Context) ([]*sample, error) {
	entries, err := os.ReadDir(b.Cfg.Samples)
	if err != nil {
		return nil, fmt.Errorf("reading samples: %w", err)
	}
	var samples []*sample
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !isVideo(name) || !matches(b.SampleFilter, name) {
			continue
		}
		s := &sample{name: name, path: filepath.Join(b.Cfg.Samples, name)}
		if s.id, err = fileid.Content(s.path); err != nil {
			return nil, err
		}
		if s.vd, err = b.Prober.Probe(ctx, s.path); err != nil {
			return nil, err
		}
		if s.vd.Tone() == probe.HDR {
			s.prefilter = plan.ToneMapSDR
		}
		scaled, err := b.scaledSamples(ctx, s)
		if err != nil {
			return nil, err
		}
		samples = append(samples, scaled...)
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("no sample videos in %s", b.Cfg.Samples)
	}
	return samples, nil
}

// scaledSamples returns the versions of s to test: s itself if no
// scales are configured, otherwise one per scale, downscaled copies
// for scales smaller than s and s itself for the rest (once). A copy is
// named "<name>@<height>p.mov", made once with ProRes HQ, and keeps an
// identity derived from s's, so remaking it doesn't invalidate results.
func (b *Bench) scaledSamples(ctx context.Context, s *sample) ([]*sample, error) {
	if len(b.Cfg.Scales) == 0 {
		return []*sample{s}, nil
	}
	var out []*sample
	usedOriginal := false
	for _, h := range b.Cfg.Scales {
		if h >= s.vd.Height {
			if !usedOriginal {
				out = append(out, s)
				usedOriginal = true
			}
			continue
		}
		w := uint64(float64(s.vd.Width)*float64(h)/float64(s.vd.Height)+0.5) &^ 1
		base := strings.TrimSuffix(s.name, filepath.Ext(s.name))
		c := &sample{
			name:      fmt.Sprintf("%s@%dp.mov", base, h),
			id:        hashKey("scaled/v1", s.id, fmt.Sprint(h)),
			prefilter: s.prefilter,
		}
		c.path = filepath.Join(b.Cfg.Workdir, "samples", c.name)
		c.vd = s.vd
		c.vd.Width, c.vd.Height = w, h
		if _, err := os.Stat(c.path); err != nil {
			if b.DryRun {
				slog.Info("Would make sample", "sample", c.name)
				out = append(out, c)
				continue
			}
			if err := b.downscale(ctx, s.path, c.path, w, h); err != nil {
				return nil, fmt.Errorf("making %s: %w", c.name, err)
			}
		}
		vd, err := b.Prober.Probe(ctx, c.path)
		if err != nil {
			return nil, err
		}
		c.vd = vd
		out = append(out, c)
	}
	return out, nil
}

// downscale writes src at w x h as ProRes 422 HQ, video only, keeping
// its bit depth, chroma and colour tags.
func (b *Bench) downscale(ctx context.Context, src, dst string, w, h uint64) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	slog.Info("Making sample", "from", filepath.Base(src), "to", filepath.Base(dst))
	partial := strings.TrimSuffix(dst, ".mov") + ".partial.mov"
	_, err := b.Exec.Exec(ctx, []string{"ffmpeg", "-hide_banner", "-loglevel", "error", "-nostats", "-y",
		"-i", src, "-map", "0:v:0", "-vf", fmt.Sprintf("scale=%d:%d:flags=lanczos", w, h),
		"-c:v", "prores_ks", "-profile:v", "3", "-pix_fmt", "yuv422p10le", partial})
	if err != nil {
		os.Remove(partial)
		return err
	}
	return os.Rename(partial, dst)
}

func isVideo(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mov", ".mp4", ".mkv", ".mxf", ".y4m", ".webm":
		return true
	}
	return false
}

func matches(pattern, name string) bool {
	if pattern == "" {
		return true
	}
	ok, _ := path.Match(pattern, name)
	return ok
}

func (b *Bench) tasks(samples []*sample, results *Results) []*task {
	var tasks []*task
	for _, s := range samples {
		for _, name := range b.Cfg.FlagsetNames() {
			if !matches(b.FlagsetFilter, name) {
				continue
			}
			fs := b.Cfg.Flagsets[name]
			fkey := flagsetKey(fs)
			if settled(b.Cfg, fs, fkey, s, results) {
				continue
			}
			seed := b.seed(fs, fkey, s, results)
			tasks = append(tasks, &task{
				sample:  s,
				name:    name,
				fs:      fs,
				fkey:    fkey,
				search:  NewSearch(fs.Quality.steps(), seed),
				encoded: map[int]string{},
			})
		}
	}
	return tasks
}

// settled reports whether the log already answers the search for fs on
// s: a passing setting whose next step fails, or a pass or fail at the
// end of the range. Without this, a finished pair would be searched
// again whenever its seed moved (say, because samples were added), and
// a different path needs encodes the log doesn't have.
func settled(c *Config, fs *Flagset, fkey string, s *sample, results *Results) bool {
	q := fs.Quality
	pass := map[int]bool{}
	for _, r := range results.Rows {
		if r.Sample == s.name && r.FlagsetKey == fkey && r.Scorer == scorer {
			pass[q.qualityToIndex(r.Quality)] = c.margin(r.Metrics) >= 0
		}
	}
	best := -1
	for i, ok := range pass {
		if ok && i > best {
			best = i
		}
	}
	switch {
	case best == q.steps():
		return true
	case best >= 0:
		failed, logged := pass[best+1]
		return logged && !failed
	}
	failed, logged := pass[0]
	return logged && !failed
}

// seed guesses where a search should start: the median answer of the
// same flagset on other samples, else of any flagset using the same
// encoder, else the middle of the range.
func (b *Bench) seed(fs *Flagset, fkey string, s *sample, results *Results) int {
	q := fs.Quality
	best := bestByPair(b.Cfg, results)
	var same, sameEncoder []int
	for pair, row := range best {
		if pair.sample == s.name || row.QualityFlag != q.Flag {
			continue
		}
		i := q.qualityToIndex(row.Quality)
		if pair.fkey == fkey {
			same = append(same, i)
		} else if row.Encoder == fs.Encoder() {
			sameEncoder = append(sameEncoder, i)
		}
	}
	for _, xs := range [][]int{same, sameEncoder} {
		if len(xs) > 0 {
			slices.Sort(xs)
			return xs[len(xs)/2]
		}
	}
	if q.Seed != 0 {
		return q.qualityToIndex(q.Seed)
	}
	return q.steps() / 2
}

// advance moves every unfinished task forward, answering from the log
// where possible, and returns the encodes needed to go further.
func (b *Bench) advance(tasks []*task, results *Results) []*pending {
	var batch []*pending
	queued := map[string]bool{}
	for _, t := range tasks {
		if t.search == nil {
			continue
		}
		for {
			i, out := t.search.Next()
			if out != Searching {
				t.outcome = out
				b.finish(t, results)
				break
			}
			q := t.fs.Quality.indexToQuality(i)
			key := encodeKey(t.sample.id, t.fkey, q)
			if row, ok := results.Lookup(key); ok {
				m := b.Cfg.margin(row.Metrics)
				t.search.Observe(i, m >= 0, m)
				continue
			}
			// Two flagsets with identical args would queue the same
			// encode; let the second wait a round for the first.
			if !queued[key] {
				queued[key] = true
				batch = append(batch, &pending{
					t: t, i: i, quality: q, key: key,
					out: filepath.Join(b.scratchDir(), key+".mp4"),
				})
			}
			break
		}
	}
	return batch
}

func (b *Bench) encodeArgv(p *pending) []string {
	argv := []string{"ffmpeg", "-hide_banner", "-loglevel", "error", "-nostats", "-y",
		"-i", p.t.sample.path, "-map", "0:v:0"}
	argv = append(argv, p.t.fs.Args...)
	argv = append(argv, p.t.fs.Quality.Flag, ftoa(p.quality), "-an")
	argv = append(argv, containerArgs(p.t.fs)...)
	return append(argv, p.out)
}

// containerArgs are muxing options that don't change the encoded
// stream, so they aren't part of the flagset's key. HEVC is tagged
// hvc1, without which QuickTime and Safari won't open the keepers.
func containerArgs(fs *Flagset) []string {
	if strings.Contains(fs.Encoder(), "265") || strings.HasPrefix(fs.Encoder(), "hevc") {
		if !slices.Contains(fs.Args, "-tag:v") {
			return []string{"-tag:v", "hvc1"}
		}
	}
	return nil
}

func (b *Bench) encodeAll(ctx context.Context, batch []*pending) error {
	for _, p := range batch {
		slog.Info("Encoding", "sample", p.t.sample.name, "flagset", p.t.name, "quality", p.quality)
		p.m, p.err = b.Exec.Exec(ctx, b.encodeArgv(p))
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p.err != nil {
			continue
		}
		st, err := os.Stat(p.out)
		if err != nil {
			p.err = err
			continue
		}
		p.bytes = st.Size()
	}
	return nil
}

// scoreAll scores the batch in parallel and calls record for each
// encode as soon as its score (or failure) is known, so an interrupted
// round loses only the scorings still in flight. record runs on the
// calling goroutine.
func (b *Bench) scoreAll(ctx context.Context, batch []*pending, record func(*pending) error) error {
	var toScore []*pending
	for _, p := range batch {
		if p.err != nil {
			if err := record(p); err != nil {
				return err
			}
			continue
		}
		toScore = append(toScore, p)
	}

	sem := make(chan struct{}, b.Cfg.VMAFJobs)
	done := make(chan *pending)
	for _, p := range toScore {
		go func() {
			sem <- struct{}{}
			p.metrics, p.err = b.score(ctx, p)
			<-sem
			done <- p
		}()
	}
	var first error
	for range toScore {
		if err := record(<-done); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (b *Bench) score(ctx context.Context, p *pending) (Metrics, error) {
	vd := p.t.sample.vd
	return Score(ctx, b.Exec, p.out, p.t.sample.path, vd.Width, vd.Height, b.Cfg.VMAFThreads, b.scratchDir(), p.key, p.t.sample.prefilter)
}

func (b *Bench) record(results *Results, p *pending) error {
	t := p.t
	if p.err != nil {
		// A failed encode (say, an unsupported option) would fail at
		// every quality, so give up on this task rather than retrying.
		slog.Error("Encode or scoring failed; skipping flagset for this sample", "sample", t.sample.name, "flagset", t.name, "err", p.err)
		os.Remove(p.out)
		for _, f := range t.encoded {
			os.Remove(f)
		}
		t.search = nil
		return nil
	}
	t.encoded[p.i] = p.out
	m := p.metrics
	row := Row{
		Sample:        t.sample.name,
		Encoder:       t.fs.Encoder(),
		Flagset:       t.name,
		Args:          joinArgs(t.fs.Args),
		QualityFlag:   t.fs.Quality.Flag,
		Quality:       p.quality,
		Key:           p.key,
		FlagsetKey:    t.fkey,
		Bytes:         p.bytes,
		Kbps:          kbps(p.bytes, t.sample.vd.Duration),
		WallS:         p.m.Wall.Seconds(),
		UserS:         p.m.User.Seconds(),
		SysS:          p.m.Sys.Seconds(),
		MaxRSSKB:      p.m.MaxRSSKB,
		Metrics:       m,
		Scorer:        scorer,
		FFmpegVersion: b.FFmpegVersion,
		Host:          b.Host,
		Time:          b.now(),
	}
	if err := results.Append(row); err != nil {
		return err
	}
	margin := b.Cfg.margin(m)
	slog.Info("Scored", "sample", t.sample.name, "flagset", t.name, "quality", p.quality,
		"kbps", int(row.Kbps), "vmaf", m.VMAF, "ciede2000", m.CIEDE2000, "colour_shift", m.ColourShiftP99, "pass", margin >= 0)
	t.search.Observe(p.i, margin >= 0, margin)
	return nil
}

func (b *Bench) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func kbps(bytes int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) * 8 / 1000 / d.Seconds()
}

// finish keeps the task's best encode for viewing and deletes the rest.
func (b *Bench) finish(t *task, results *Results) {
	defer func() {
		for _, f := range t.encoded {
			os.Remove(f)
		}
		t.search = nil
	}()
	slog.Info("Finished", "sample", t.sample.name, "flagset", t.name, "outcome", t.outcome)
	best := t.search.Answer()
	if best < 0 {
		return
	}
	q := t.fs.Quality.indexToQuality(best)
	keep := KeeperPath(b.keepDir(), t.sample.name, t.name, q)
	if _, err := os.Stat(keep); err == nil {
		return
	}
	src, ok := t.encoded[best]
	if !ok {
		slog.Warn("Best encode came from an earlier run and wasn't kept", "sample", t.sample.name, "flagset", t.name, "quality", q)
		return
	}
	if err := replaceKeeper(keep, src); err != nil {
		slog.Warn("Couldn't keep best encode", "path", keep, "err", err)
		return
	}
	delete(t.encoded, best)
}

// KeeperPath is where the best encode of flagset at quality q on a
// sample is kept.
func KeeperPath(dir, sampleName, flagset string, q float64) string {
	base := strings.TrimSuffix(sampleName, filepath.Ext(sampleName))
	return filepath.Join(dir, base, fmt.Sprintf("%s.q%s.mp4", flagset, ftoa(q)))
}

// replaceKeeper moves src to keep, removing keepers of the same flagset
// at other qualities.
func replaceKeeper(keep, src string) error {
	dir := filepath.Dir(keep)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	prefix := strings.SplitN(filepath.Base(keep), ".q", 2)[0] + ".q"
	old, _ := filepath.Glob(filepath.Join(dir, globEscape(prefix)+"*.mp4"))
	if err := os.Rename(src, keep); err != nil {
		return err
	}
	var errs []error
	for _, o := range old {
		if o != keep {
			errs = append(errs, os.Remove(o))
		}
	}
	return errors.Join(errs...)
}

func globEscape(s string) string {
	return strings.NewReplacer(`*`, `\*`, `?`, `\?`, `[`, `\[`, `\`, `\\`).Replace(s)
}
