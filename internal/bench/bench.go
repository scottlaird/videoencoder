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
		samples = append(samples, s)
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("no sample videos in %s", b.Cfg.Samples)
	}
	return samples, nil
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
	argv = append(argv, p.t.fs.Quality.Flag, ftoa(p.quality), "-an", p.out)
	return argv
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
	logPath := filepath.Join(b.scratchDir(), p.key+".vmaf.json")
	colourLogPath := filepath.Join(b.scratchDir(), p.key+".ciede.json")
	defer os.Remove(logPath)
	defer os.Remove(colourLogPath)
	vd := p.t.sample.vd
	argv := scoreArgv(p.out, p.t.sample.path, vd.Width, vd.Height, b.Cfg.VMAFThreads, logPath, colourLogPath)
	if _, err := b.Exec.Exec(ctx, argv); err != nil {
		return Metrics{}, fmt.Errorf("scoring: %w", err)
	}
	return readLogs(logPath, colourLogPath)
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
		FFmpegVersion: b.FFmpegVersion,
		Host:          b.Host,
		Time:          b.now(),
	}
	if err := results.Append(row); err != nil {
		return err
	}
	margin := b.Cfg.margin(m)
	slog.Info("Scored", "sample", t.sample.name, "flagset", t.name, "quality", p.quality,
		"kbps", int(row.Kbps), "vmaf", m.VMAF, "ciede2000", m.CIEDE2000, "pass", margin >= 0)
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
