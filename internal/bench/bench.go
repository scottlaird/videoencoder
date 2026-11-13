package bench

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
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
	// OutputProber, if set, reads each scored encode's stream details
	// (Row.Output); a failure only leaves them out.
	OutputProber OutputProber
	// Host, FFmpegVersion and CPU are recorded with each result, and
	// so is EncoderVersions' entry for its encoder (see EncoderVersion).
	Host            string
	FFmpegVersion   string
	CPU             string
	EncoderVersions map[string]string
	// SampleFilter and FlagsetFilter, if set, are path.Match patterns
	// limiting which samples and flagsets run.
	SampleFilter, FlagsetFilter string
	// DryRun reports what would be encoded without encoding anything.
	DryRun bool
	Now    func() time.Time
	// Observe, if set, hears about the run's encodes as they happen,
	// on the goroutine that called Run.
	Observe func(Event)

	scorings Scorings
	// seeds is Config.SeedResults's answers, if it's set.
	seeds map[pair]Row
	// listing makes loadSamples skip making copies, quietly.
	listing bool
}

type sample struct {
	name string // file name, used in results and keeper paths
	// source is the file in Samples this is (a scaled copy of).
	source string
	path   string
	id     string
	vd     probe.VideoData
	// srcVD is the source's, before any scaling.
	srcVD probe.VideoData
	// ref, if not nil, is what encodes of this sample are scored
	// against (see Config.References); otherwise the sample itself.
	ref *sample
	// prefilter is applied to both sides before scoring: a tone map to
	// SDR for HDR samples, since VMAF and CIEDE2000 are SDR metrics,
	// unless an HDR VMAF model judges them.
	prefilter string
	scoring   Scoring
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
	// retimed means the answer's timing re-run has been queued (or
	// wasn't needed), so the task can finish.
	retimed bool
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
	// alone says the encode ran by itself, so m is its own; batchSize
	// is how many shared the ffmpeg run, and batchID names it when
	// that's more than one.
	alone     bool
	batchSize int
	batchID   string
	// stage is where err came from: "encode" or "score".
	stage string
	// scoreTime is how long scoring took, and output the encode's
	// stream details.
	scoreTime time.Duration
	output    OutputInfo
	// sha256 is the encoded file's, in hex.
	sha256 string
	// retime is the logged row of an answer encoded with others, which
	// this encode repeats alone for its time; it isn't scored.
	retime *Row
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
//
// A failure that stops the run (rather than one encode, which only ends
// its search) is logged to failures.csv as well as returned.
func (b *Bench) Run(ctx context.Context) error {
	err := b.run(ctx)
	if err != nil && ctx.Err() == nil && !b.DryRun {
		sample, flagset := b.SampleFilter, b.FlagsetFilter
		b.logFailure(sample, flagset, math.NaN(), "run", err)
	}
	return err
}

func (b *Bench) run(ctx context.Context) error {
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
	b.scorings = Scorings{}
	for _, s := range samples {
		b.scorings[s.name] = s.scoring
	}
	if b.Cfg.SeedResults != "" {
		seeds, err := readResults(b.Cfg.SeedResults)
		if err != nil {
			return fmt.Errorf("seed_results: %w", err)
		}
		b.seeds = bestByPair(b.Cfg, seeds, b.scorings)
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
		if err := b.encodeAll(ctx, round, batch); err != nil {
			return err
		}
		err := b.scoreAll(ctx, batch, func(p *pending) error { return b.record(results, round, p) })
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return WriteBest(b.bestPath(), b.Cfg, results, b.scorings)
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
		s := &sample{name: name, source: name, path: filepath.Join(b.Cfg.Samples, name)}
		if s.id, err = fileid.Content(s.path); err != nil {
			return nil, err
		}
		if s.vd, err = b.Prober.Probe(ctx, s.path); err != nil {
			return nil, err
		}
		s.srcVD = s.vd
		if s.vd.Tone() == probe.HDR {
			s.prefilter = plan.ToneMapSDR
		}
		scaled, err := b.scaledSamples(ctx, s)
		if err != nil {
			return nil, err
		}
		if refName := b.Cfg.References[name]; refName != "" {
			if err := b.attachReferences(ctx, s, scaled, refName); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
		for _, c := range scaled {
			b.setScoring(c)
		}
		samples = append(samples, scaled...)
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("no sample videos in %s", b.Cfg.Samples)
	}
	return samples, nil
}

// attachReferences points each of s's scaled versions at the matching
// version of the reference file refName. Encodes are keyed by the pair,
// so a sample's results with and without a reference stay apart.
func (b *Bench) attachReferences(ctx context.Context, s *sample, scaled []*sample, refName string) error {
	ref := &sample{name: refName, path: filepath.Join(b.Cfg.Samples, refName)}
	var err error
	if ref.id, err = fileid.Content(ref.path); err != nil {
		return fmt.Errorf("reference: %w", err)
	}
	if ref.vd, err = b.Prober.Probe(ctx, ref.path); err != nil {
		return fmt.Errorf("reference: %w", err)
	}
	ref.srcVD = ref.vd
	if ref.vd.Width != s.vd.Width || ref.vd.Height != s.vd.Height {
		return fmt.Errorf("reference %s is %dx%d, sample is %dx%d", refName, ref.vd.Width, ref.vd.Height, s.vd.Width, s.vd.Height)
	}
	if d := ref.vd.Duration - s.vd.Duration; d > 100*time.Millisecond || d < -100*time.Millisecond {
		return fmt.Errorf("reference %s lasts %v, sample %v: they must have the same frames", refName, ref.vd.Duration, s.vd.Duration)
	}
	if ref.vd.Tone() == probe.HDR {
		ref.prefilter = plan.ToneMapSDR
	}
	refs, err := b.scaledSamples(ctx, ref)
	if err != nil {
		return err
	}
	if len(refs) != len(scaled) {
		return fmt.Errorf("reference %s scales differently", refName)
	}
	for i, c := range scaled {
		c.ref = refs[i]
		c.id = hashKey("ref/v1", c.id, refs[i].id)
	}
	return nil
}

// setScoring picks how s's encodes are judged, from the tone and
// height of what they're scored against.
func (b *Bench) setScoring(s *sample) {
	ref := s.reference()
	hdr := ref.vd.Tone() == probe.HDR
	s.scoring = b.Cfg.scoring(hdr, ref.vd.Height)
	if hdr && s.scoring.Model.HDR {
		ref.prefilter = ""
	}
}

// Scorings returns how each sample is judged under the config, for
// reports on the log. It makes no downscaled copies.
func (b *Bench) Scorings(ctx context.Context) (Scorings, error) {
	dry := *b
	dry.listing = true
	samples, err := dry.loadSamples(ctx)
	if err != nil {
		return nil, err
	}
	sc := Scorings{}
	for _, s := range samples {
		sc[s.name] = s.scoring
	}
	return sc, nil
}

// reference is what encodes of s are scored against.
func (s *sample) reference() *sample {
	if s.ref != nil {
		return s.ref
	}
	return s
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
			source:    s.source,
			id:        hashKey("scaled/v1", s.id, fmt.Sprint(h)),
			prefilter: s.prefilter,
			srcVD:     s.srcVD,
		}
		c.path = filepath.Join(b.Cfg.Workdir, "samples", c.name)
		c.vd = s.vd
		c.vd.Width, c.vd.Height = w, h
		if _, err := os.Stat(c.path); err != nil {
			if b.listing {
				out = append(out, c)
				continue
			}
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
// its bit depth, chroma and color tags.
func (b *Bench) downscale(ctx context.Context, src, dst string, w, h uint64) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	slog.Info("Making sample", "from", filepath.Base(src), "to", filepath.Base(dst))
	partial := strings.TrimSuffix(dst, ".mov") + ".partial.mov"
	_, err := b.Exec.Exec(ctx, []string{"ffmpeg", "-hide_banner", "-loglevel", "error", "-nostats", "-y",
		"-i", src, "-map", "0:v:0", "-vf", plan.Scale(w, h),
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
			if fs.SDROnly && s.vd.Tone() == probe.HDR {
				continue
			}
			fkey := flagsetKey(fs)
			done, untimed := settled(b.Cfg, fs, fkey, s, results)
			if done {
				continue
			}
			search := NewSearch(fs.Quality.steps(), b.seed(fs, fkey, s, results))
			if row, ok := b.seeds[pair{s.name, fkey}]; ok {
				search = NewSearchAt(fs.Quality.steps(), fs.Quality.qualityToIndex(row.Quality))
			}
			// An answer that only lacks its time: give the search what
			// the log knows, so it ends at once, on that answer, and
			// re-times it, instead of searching a path of its own.
			if untimed {
				for _, r := range results.Rows {
					if r.Sample == s.name && r.FlagsetKey == fkey && r.Scorer == s.scoring.Scorer {
						m := b.Cfg.margin(r.Metrics, s.scoring.Target)
						search.Observe(fs.Quality.qualityToIndex(r.Quality), m >= 0, m)
					}
				}
			}
			tasks = append(tasks, &task{
				sample:  s,
				name:    name,
				fs:      fs,
				fkey:    fkey,
				search:  search,
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
// a different path needs encodes the log doesn't have. An answer
// without a time of its own (see Row.Timed) isn't settled: untimed
// says so, and the search is given the log's measurements so that it
// ends on that answer and re-times it.
func settled(c *Config, fs *Flagset, fkey string, s *sample, results *Results) (done, untimed bool) {
	q := fs.Quality
	pass := map[int]bool{}
	timed := map[int]bool{}
	for _, r := range results.Rows {
		if r.Sample == s.name && r.FlagsetKey == fkey && r.Scorer == s.scoring.Scorer {
			// Rows from before the flagset's step changed, rounded onto
			// the grid, could share an index and settle a search that
			// isn't (see Quality.onGrid).
			if !q.onGrid(r.Quality) {
				continue
			}
			i := q.qualityToIndex(r.Quality)
			pass[i] = c.margin(r.Metrics, s.scoring.Target) >= 0
			timed[i] = timed[i] || r.Timed
		}
	}
	best := -1
	for i, ok := range pass {
		if ok && i > best {
			best = i
		}
	}
	answered := false
	switch {
	case best == q.steps():
		answered = true
	case best >= 0:
		failed, logged := pass[best+1]
		answered = logged && !failed
	default:
		// Nothing passes: settled once the best setting is known to
		// fail. There's no answer to time.
		failed, logged := pass[0]
		return logged && !failed, false
	}
	return answered && timed[best], answered && !timed[best]
}

// seed guesses where a search should start: the median answer of the
// same flagset on other samples, else of any flagset using the same
// encoder, else the middle of the range.
func (b *Bench) seed(fs *Flagset, fkey string, s *sample, results *Results) int {
	q := fs.Quality
	best := bestByPair(b.Cfg, results, b.scorings)
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
// where possible, and returns the encodes needed to go further: up to
// Flagset.Batch settings per task, together in the batch, or a task's
// timing re-run.
func (b *Bench) advance(tasks []*task, results *Results) []*pending {
	var batch []*pending
	queued := map[string]bool{}
	for _, t := range tasks {
		if t.search == nil {
			continue
		}
		for {
			pts, out := t.search.NextBatch(t.fs.Batch)
			if out != Searching {
				t.outcome = out
				if p := b.retime(t, results); p != nil {
					batch = append(batch, p)
					break
				}
				b.finish(t, results)
				break
			}
			var need []*pending
			logged := false
			for _, i := range pts {
				q := t.fs.Quality.indexToQuality(i)
				key := encodeKey(t.sample.scoring.Scorer, t.sample.id, t.fkey, q)
				if row, ok := results.Lookup(key); ok {
					m := b.Cfg.margin(row.Metrics, t.sample.scoring.Target)
					t.search.Observe(i, m >= 0, m)
					logged = true
					continue
				}
				need = append(need, &pending{t: t, i: i, quality: q, key: key, out: filepath.Join(b.scratchDir(), key+".mp4")})
			}
			// The log answered some: plan again with what it knew.
			if logged {
				continue
			}
			// Two flagsets with identical args would queue the same
			// encode; let the second wait a round for the first.
			for _, p := range need {
				if !queued[p.key] {
					queued[p.key] = true
					batch = append(batch, p)
				}
			}
			break
		}
	}
	return batch
}

// retime returns the encode that times t's answer, if the answer was
// encoded with others and so has no time of its own (see Row.Timed),
// or nil if the task can finish.
func (b *Bench) retime(t *task, results *Results) *pending {
	if t.retimed || (t.outcome != Found && t.outcome != AllPass) {
		return nil
	}
	t.retimed = true
	i := t.search.Answer()
	if i < 0 {
		return nil
	}
	q := t.fs.Quality.indexToQuality(i)
	key := encodeKey(t.sample.scoring.Scorer, t.sample.id, t.fkey, q)
	row, ok := results.Lookup(key)
	if !ok || row.Timed {
		return nil
	}
	return &pending{t: t, i: i, quality: q, key: key, out: filepath.Join(b.scratchDir(), key+".timing.mp4"), retime: &row}
}

// encodeArgv is the ffmpeg command for ps, which are all the same task's:
// one decode of the sample, and an output for each setting, so a batch
// shares the decode. One setting is the usual single encode.
func (b *Bench) encodeArgv(ps ...*pending) []string {
	argv := []string{"ffmpeg", "-hide_banner", "-loglevel", "error", "-nostats", "-y",
		"-i", ps[0].t.sample.path}
	for _, p := range ps {
		argv = append(argv, "-map", "0:v:0")
		argv = append(argv, p.t.fs.Args...)
		argv = append(argv, p.t.fs.Quality.Flag, ftoa(p.quality), "-an")
		argv = append(argv, containerArgs(p.t.fs)...)
		argv = append(argv, p.out)
	}
	return argv
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

// encodeAll runs the round's encodes one ffmpeg at a time, so each
// one's time is clean: a task's settings together, sharing a decode,
// and a timing re-run by itself.
func (b *Bench) encodeAll(ctx context.Context, round int, batch []*pending) error {
	for start := 0; start < len(batch); {
		end := start + 1
		for end < len(batch) && batch[end].t == batch[start].t && batch[start].retime == nil && batch[end].retime == nil {
			end++
		}
		group := batch[start:end]
		start = end

		p0 := group[0]
		qs := make([]float64, len(group))
		for i, p := range group {
			qs[i] = p.quality
		}
		kind := Encoding
		if p0.retime != nil {
			kind = Timing
			slog.Info("Timing", "sample", p0.t.sample.name, "flagset", p0.t.name, "quality", p0.quality)
		} else {
			slog.Info("Encoding", "sample", p0.t.sample.name, "flagset", p0.t.name, "quality", qs)
		}
		ev := Event{Kind: kind, Round: round, Source: p0.t.sample.source, Sample: p0.t.sample.name, Flagset: p0.t.name, QualityFlag: p0.t.fs.Quality.Flag, Quality: p0.quality, Qualities: qs}
		b.observe(ev)
		m, err := b.encode(ctx, ev, p0.t.sample.vd.Duration, b.encodeArgv(group...))
		if ctx.Err() != nil {
			return ctx.Err()
		}
		batchID := ""
		if len(group) > 1 {
			batchID = newID()
		}
		for _, p := range group {
			p.m, p.err, p.alone = m, err, len(group) == 1
			p.batchSize, p.batchID = len(group), batchID
			if p.err != nil {
				p.stage = "encode"
				continue
			}
			st, err := os.Stat(p.out)
			if err != nil {
				p.err, p.stage = err, "encode"
				continue
			}
			p.bytes = st.Size()
			if p.sha256, err = fileSHA256(p.out); err != nil {
				p.err, p.stage = err, "encode"
			}
		}
	}
	return nil
}

// progressLogEvery is how often encode logs a long encode's progress.
const progressLogEvery = time.Minute

// encode runs argv, an encode of a sample lasting total that ev
// announced, reporting its progress as Encoded events and, every
// progressLogEvery, in the log.
func (b *Bench) encode(ctx context.Context, ev Event, total time.Duration, argv []string) (Measurement, error) {
	enc, ok := b.Exec.(Encoder)
	if !ok {
		return b.Exec.Exec(ctx, argv)
	}
	ev.Kind = Encoded
	var logged time.Duration
	return enc.Encode(ctx, argv, total, func(p Progress) {
		ev.Progress = p
		b.observe(ev)
		if p.Elapsed-logged >= progressLogEvery {
			logged = p.Elapsed
			attrs := []any{"sample", ev.Sample, "flagset", ev.Flagset, "percent", int(p.Fraction() * 100), "elapsed", p.Elapsed.Round(time.Second)}
			if left, ok := p.Remaining(); ok {
				attrs = append(attrs, "remaining", left.Round(time.Second))
			}
			slog.Info("Encode progress", attrs...)
		}
	})
}

// scoreAll scores the batch in parallel and calls record for each
// encode as soon as its score (or failure) is known, so an interrupted
// round loses only the scorings still in flight. record runs on the
// calling goroutine.
func (b *Bench) scoreAll(ctx context.Context, batch []*pending, record func(*pending) error) error {
	var toScore []*pending
	for _, p := range batch {
		// A timing re-run repeats an encode already scored.
		if p.err != nil || p.retime != nil {
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
			start := time.Now()
			p.metrics, p.err = b.score(ctx, p)
			p.scoreTime = time.Since(start)
			if p.err != nil {
				p.stage = "score"
			} else if b.OutputProber != nil {
				out, err := b.OutputProber.ProbeOutput(ctx, p.out)
				if err != nil {
					slog.Warn("Couldn't probe encode", "sample", p.t.sample.name, "flagset", p.t.name, "quality", p.quality, "err", err)
				}
				p.output = out
			}
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
	ref := p.t.sample.reference()
	vd := ref.vd
	return Score(ctx, b.Exec, p.out, ref.path, vd.Width, vd.Height, b.Cfg.VMAFThreads, b.scratchDir(), p.key, ref.prefilter, vd.ColorTransfer, p.t.sample.scoring.Model)
}

func (b *Bench) record(results *Results, round int, p *pending) error {
	t := p.t
	ev := Event{Round: round, Source: t.sample.source, Sample: t.sample.name, Flagset: t.name, QualityFlag: t.fs.Quality.Flag, Quality: p.quality}
	if p.retime != nil {
		return b.recordTiming(results, p)
	}
	if p.err != nil {
		ev.Kind, ev.Err = Failed, p.err
		b.observe(ev)
		// A failed encode (say, an unsupported option) would fail at
		// every quality, so give up on this task rather than retrying.
		slog.Error("Encode or scoring failed; skipping flagset for this sample", "sample", t.sample.name, "flagset", t.name, "err", p.err)
		b.logFailure(t.sample.name, t.name, p.quality, p.stage, p.err)
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
		Sample:         t.sample.name,
		Reference:      t.sample.reference().name,
		Encoder:        t.fs.Encoder(),
		Flagset:        t.name,
		Args:           joinArgs(t.fs.Args),
		QualityFlag:    t.fs.Quality.Flag,
		Quality:        p.quality,
		Key:            p.key,
		FlagsetKey:     t.fkey,
		Bytes:          p.bytes,
		Kbps:           kbps(p.bytes, t.sample.vd.Duration),
		WallS:          p.m.Wall.Seconds(),
		UserS:          p.m.User.Seconds(),
		SysS:           p.m.Sys.Seconds(),
		MaxRSSKB:       p.m.MaxRSSKB,
		Metrics:        m,
		Scorer:         t.sample.scoring.Scorer,
		FFmpegVersion:  b.FFmpegVersion,
		Host:           b.Host,
		CPU:            b.CPU,
		EncoderVersion: b.EncoderVersions[t.fs.Encoder()],
		Time:           b.now(),
		Timed:          p.alone,
	}
	margin := b.Cfg.margin(m, t.sample.scoring.Target)
	pass := margin >= 0
	b.describe(&row, t.sample)
	row.Output, row.OutSHA256 = p.output, p.sha256
	row.Pass = &pass
	row.ScoreS = p.scoreTime.Seconds()
	row.BatchSize, row.BatchID = int64(p.batchSize), p.batchID
	if err := results.Append(row); err != nil {
		return err
	}
	slog.Info("Scored", "sample", t.sample.name, "flagset", t.name, "quality", p.quality,
		"kbps", int(row.Kbps), "vmaf", m.VMAF, "ciede2000", m.CIEDE2000, "color_shift", m.ColorShiftP99, "pass", margin >= 0)
	t.search.Observe(p.i, margin >= 0, margin)
	ev.Kind, ev.Kbps, ev.VMAF, ev.Pass = Scored, row.Kbps, m.VMAF, margin >= 0
	b.observe(ev)
	return nil
}

// recordTiming logs the answer's row again with p's time, which is the
// encode's own. The encode is the same as the scored one; a size that
// differs means the encoder isn't deterministic across runs (some vary
// with thread count or timing), which the log notes but keeps.
func (b *Bench) recordTiming(results *Results, p *pending) error {
	t := p.t
	defer os.Remove(p.out)
	if p.err != nil {
		slog.Warn("Timing re-run failed; the answer keeps its batched time", "sample", t.sample.name, "flagset", t.name, "quality", p.quality, "err", p.err)
		return nil
	}
	row := *p.retime
	if p.bytes != row.Bytes {
		slog.Warn("Timing re-run came out a different size; its time is kept, the scored encode's size and scores stand",
			"sample", t.sample.name, "flagset", t.name, "quality", p.quality, "scored_bytes", row.Bytes, "timing_bytes", p.bytes)
	}
	row.WallS, row.UserS, row.SysS, row.MaxRSSKB = p.m.Wall.Seconds(), p.m.User.Seconds(), p.m.Sys.Seconds(), p.m.MaxRSSKB
	row.FFmpegVersion, row.Host, row.CPU = b.FFmpegVersion, b.Host, b.CPU
	row.EncoderVersion = b.EncoderVersions[t.fs.Encoder()]
	row.Time = b.now()
	row.Timed = true
	// The scored row's sample, output and scores stand; the rest is
	// this run's.
	b.describe(&row, t.sample)
	row.ScoreS, row.BatchSize, row.BatchID = 0, 1, ""
	if p.sha256 != row.OutSHA256 && row.OutSHA256 != "" && p.bytes == row.Bytes {
		slog.Warn("Timing re-run has the same size but different contents", "sample", t.sample.name, "flagset", t.name, "quality", p.quality)
	}
	row.OutSHA256 = p.sha256
	if err := results.Append(row); err != nil {
		return err
	}
	slog.Info("Timed", "sample", t.sample.name, "flagset", t.name, "quality", p.quality, "wall_s", row.WallS, "user_s", row.UserS)
	return nil
}

// describe fills in the row's run and sample details: a fresh RowID,
// this build and config, and s and how it's scored.
func (b *Bench) describe(row *Row, s *sample) {
	row.RowID = newID()
	row.EncbenchVersion, row.Config, row.Job = BuildVersion(), b.Cfg.Path, b.Cfg.Job
	row.Source, row.SampleID = s.source, s.id
	row.Width, row.Height = int64(s.vd.Width), int64(s.vd.Height)
	row.FPS, row.DurationS = s.vd.FPS, s.vd.Duration.Seconds()
	row.SourceWidth, row.SourceHeight = int64(s.srcVD.Width), int64(s.srcVD.Height)
	row.ColorTransfer, row.ColorPrimaries = s.vd.ColorTransfer, s.vd.ColorPrimaries
	sc := s.scoring
	row.VMAFModel, row.TargetVMAF = sc.Model.LogName(s.reference().vd.Height), sc.Target
}

// logFailure adds a failure to the workdir's failures.csv, logging
// (not returning) any error doing so.
func (b *Bench) logFailure(sampleName, flagset string, quality float64, stage string, err error) {
	f := Failure{Time: b.now(), Host: b.Host, Job: b.Cfg.Job, Config: b.Cfg.Path, Sample: sampleName,
		Flagset: flagset, Quality: quality, Stage: stage, Err: err.Error(),
		FFmpegVersion: b.FFmpegVersion, EncbenchVersion: BuildVersion()}
	if err := AppendFailure(b.Cfg.Workdir, f); err != nil {
		slog.Warn("Couldn't log failure", "err", err)
	}
}

// fileSHA256 returns the SHA-256 of the file at path, in hex.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// newID returns a random 16-hex-digit ID.
func newID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
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
	ev := Event{Kind: Finished, Source: t.sample.source, Sample: t.sample.name, Flagset: t.name, QualityFlag: t.fs.Quality.Flag, Outcome: t.outcome}
	if best < 0 {
		b.observe(ev)
		return
	}
	q := t.fs.Quality.indexToQuality(best)
	ev.Quality = q
	if row, ok := results.Lookup(encodeKey(t.sample.scoring.Scorer, t.sample.id, t.fkey, q)); ok {
		ev.Kbps, ev.VMAF, ev.Pass = row.Kbps, row.Metrics.VMAF, true
	}
	b.observe(ev)
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
