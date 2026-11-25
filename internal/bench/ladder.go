package bench

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/scottlaird/videoencoder/internal/plan"
)

// ladderRun is one ladder on one sample file.
type ladderRun struct {
	name string
	// source is the sample file's name, and path, width and height
	// describe it before any scaling.
	source        string
	path          string
	width, height uint64
	inputArgs     []string
	rungs         []*ladderRung
	key           string
}

// ladderRung is a rung the sample is tall enough for, with its setting.
type ladderRung struct {
	name    string // the flagset's
	fs      *Flagset
	height  uint64
	quality float64
	// sample is the sample's version at height, which the rung is named
	// and scored as.
	sample *sample
	// fkey is the rung's FlagsetKey in the log: its own, so searches and
	// answers, which look for the flagset's, don't count it.
	fkey string
}

// scaledWidth is the even width a w x h picture scales to at height h2.
func scaledWidth(w, h, h2 uint64) uint64 {
	return uint64(float64(w)*float64(h2)/float64(h)+0.5) &^ 1
}

// ladderAnswers reads the log whose answers set l's settings: see
// Ladder.Answers.
func (b *Bench) ladderAnswers(l *Ladder, samples []*sample, results *Results) (map[pair]Row, error) {
	path := l.Answers
	if path == "" {
		path = b.Cfg.SeedResults
	}
	log := results
	if path != "" {
		var err error
		if log, err = readResults(path); err != nil {
			return nil, fmt.Errorf("ladder answers: %w", err)
		}
	}
	sc := Scorings{}
	for _, s := range samples {
		sc[s.name] = s.scoring
	}
	return bestByPair(b.Cfg, log, sc), nil
}

// planLadder sets out ladder name on the sample file source, whose
// versions (from loadSamples) are among samples. With no rungs the
// sample is tall enough for, it returns nil.
func (b *Bench) planLadder(name, source string, samples []*sample, answers map[pair]Row) (*ladderRun, error) {
	l := b.Cfg.Ladders[name]
	var versions []*sample
	for _, s := range samples {
		if s.source == source {
			versions = append(versions, s)
		}
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("no sample %s", source)
	}
	src := versions[0].srcVD
	lr := &ladderRun{name: name, source: source, path: filepath.Join(b.Cfg.Samples, source), width: src.Width, height: src.Height}
	// The scaler is part of what a rung encodes, so a change to it runs
	// the ladder again.
	keyParts := []string{"ladder/v1", "scale/" + plan.Scale(1, 1)}
	var missing []string
	for _, r := range l.Rungs {
		if r.Height > src.Height {
			continue
		}
		version := func(h uint64) *sample {
			for _, v := range versions {
				if v.vd.Height == h {
					return v
				}
			}
			return nil
		}
		s := version(r.Height)
		if s == nil {
			return nil, fmt.Errorf("%s has no version %dp high", source, r.Height)
		}
		// The answer the rung's setting comes from: its own, or with
		// QualityFrom another height's.
		from := s
		if r.QualityFrom != 0 {
			if from = version(r.QualityFrom); from == nil {
				return nil, fmt.Errorf("%s has no version %dp high", source, r.QualityFrom)
			}
		}
		fs := b.Cfg.Flagsets[r.Flagset]
		fkey := flagsetKey(fs)
		row, ok := answers[pair{from.name, fkey}]
		if !ok {
			missing = append(missing, fmt.Sprintf("%s on %s", r.Flagset, from.name))
			continue
		}
		q := row.Quality
		if r.QualityFrom != 0 {
			q = min(max(q+r.QualityOffset, fs.Quality.Min), fs.Quality.Max)
		}
		lr.inputArgs = fs.InputArgs
		lr.rungs = append(lr.rungs, &ladderRung{name: r.Flagset, fs: fs, height: r.Height, quality: q, sample: s})
		keyParts = append(keyParts, s.id, s.scoring.Scorer, fkey, fmt.Sprint(r.Height), ftoa(q))
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("ladder %s: no answer for %s", name, strings.Join(missing, ", "))
	}
	if len(lr.rungs) == 0 {
		return nil, nil
	}
	lr.key = hashKey(keyParts...)
	for _, r := range lr.rungs {
		r.fkey = hashKey("ladder/rung/v1", lr.key, flagsetKey(r.fs), fmt.Sprint(r.height))
	}
	return lr, nil
}

// done reports whether the log has the run's encodes.
func (lr *ladderRun) done(results *Results) bool {
	for _, r := range results.Rows {
		if r.LadderKey == lr.key {
			return true
		}
	}
	return false
}

// ladderArgv is the one ffmpeg that encodes every rung of lr, into outs:
// the sample decoded once, scaled once for each height below its own,
// and split among that height's rungs.
func ladderArgv(lr *ladderRun, outs []string) []string {
	var heights []uint64
	byHeight := map[uint64][]int{}
	for i, r := range lr.rungs {
		if _, ok := byHeight[r.height]; !ok {
			heights = append(heights, r.height)
		}
		byHeight[r.height] = append(byHeight[r.height], i)
	}
	var graph []string
	in := make([]string, len(heights))
	if len(heights) == 1 {
		in[0] = "[0:v:0]"
	} else {
		split := fmt.Sprintf("[0:v:0]split=%d", len(heights))
		for i := range heights {
			in[i] = fmt.Sprintf("[h%d]", i)
			split += in[i]
		}
		graph = append(graph, split)
	}
	for i, h := range heights {
		var filters []string
		if h < lr.height {
			filters = append(filters, plan.Scale(scaledWidth(lr.width, lr.height, h), h))
		}
		if n := len(byHeight[h]); n > 1 {
			filters = append(filters, fmt.Sprintf("split=%d", n))
		}
		if len(filters) == 0 {
			filters = []string{"null"}
		}
		chain := in[i] + strings.Join(filters, ",")
		for _, r := range byHeight[h] {
			chain += fmt.Sprintf("[o%d]", r)
		}
		graph = append(graph, chain)
	}
	argv := []string{"ffmpeg", "-hide_banner", "-loglevel", "error", "-nostats", "-y"}
	argv = append(argv, lr.inputArgs...)
	argv = append(argv, "-i", lr.path, "-filter_complex", strings.Join(graph, ";"))
	for i, r := range lr.rungs {
		argv = append(argv, "-map", fmt.Sprintf("[o%d]", i))
		argv = append(argv, r.fs.Args...)
		argv = append(append(argv, r.fs.Quality.argv(r.quality)...), "-an")
		argv = append(argv, containerArgs(r.fs)...)
		argv = append(argv, outs[i])
	}
	return argv
}

// runLadders runs each ladder the filters allow on each sample file
// whose log lacks it. A ladder that fails on one sample is logged as a
// failure and the rest go on; the failures are returned together.
func (b *Bench) runLadders(ctx context.Context, samples []*sample, results *Results) error {
	var errs []error
	for _, name := range b.Cfg.LadderNames() {
		if !matches(b.FlagsetFilter, name) {
			continue
		}
		answers, err := b.ladderAnswers(b.Cfg.Ladders[name], samples, results)
		if err != nil {
			return err
		}
		for _, source := range sources(samples) {
			lr, err := b.planLadder(name, source, samples, answers)
			if err != nil {
				if !b.DryRun {
					b.logFailure(source, name, math.NaN(), "ladder", err)
				}
				errs = append(errs, err)
				continue
			}
			if lr == nil || lr.done(results) {
				continue
			}
			if b.DryRun {
				for _, r := range lr.rungs {
					slog.Info("Would encode in ladder", "ladder", name, "sample", r.sample.name, "flagset", r.name, "quality", r.quality)
				}
				continue
			}
			if err := b.runLadder(ctx, lr, results); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// sources lists the sample files samples are versions of, in order.
func sources(samples []*sample) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range samples {
		if !seen[s.source] {
			seen[s.source] = true
			out = append(out, s.source)
		}
	}
	return out
}

// runLadder encodes lr's rungs in one ffmpeg, scores each, and logs them.
func (b *Bench) runLadder(ctx context.Context, lr *ladderRun, results *Results) error {
	var batch []*pending
	outs := make([]string, len(lr.rungs))
	qs := make([]float64, len(lr.rungs))
	for i, r := range lr.rungs {
		t := &task{sample: r.sample, name: r.name, fs: r.fs, fkey: r.fkey, encoded: map[int]string{}}
		p := &pending{t: t, quality: r.quality, key: encodeKey(r.sample.scoring.Scorer, r.sample.id, r.fkey, r.quality)}
		p.out = filepath.Join(b.scratchDir(), p.key+".mp4")
		outs[i], qs[i] = p.out, r.quality
		batch = append(batch, p)
	}
	defer func() {
		for _, o := range outs {
			os.Remove(o)
		}
	}()
	slog.Info("Encoding ladder", "ladder", lr.name, "sample", lr.source, "rungs", len(lr.rungs))
	ev := Event{Kind: Encoding, Round: 1, Source: lr.source, Sample: lr.source, Flagset: lr.name, QualityFlag: "ladder", Quality: qs[0], Qualities: qs}
	b.observe(ev)
	dur := lr.rungs[0].sample.vd.Duration
	m, err := b.encode(ctx, ev, dur, ladderArgv(lr, outs))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		err = fmt.Errorf("ladder %s on %s: %w", lr.name, lr.source, err)
		b.logFailure(lr.source, lr.name, math.NaN(), "encode", err)
		b.observe(Event{Kind: Failed, Source: lr.source, Sample: lr.source, Flagset: lr.name, Err: err})
		return err
	}
	slog.Info("Encoded ladder", "ladder", lr.name, "sample", lr.source, "wall_s", m.Wall.Seconds(), "user_s", m.User.Seconds())
	id := newID()
	for _, p := range batch {
		p.m, p.batchSize, p.batchID = m, len(batch), id
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
	var errs []error
	err = b.scoreAll(ctx, 1, batch, nil, func(p *pending) error {
		if p.err != nil {
			err := fmt.Errorf("ladder %s: %s on %s: %w", lr.name, p.t.name, p.t.sample.name, p.err)
			b.logFailure(p.t.sample.name, p.t.name, p.quality, p.stage, err)
			b.observe(Event{Kind: Failed, Source: lr.source, Sample: p.t.sample.name, Flagset: p.t.name, Err: err})
			errs = append(errs, err)
			return nil
		}
		return b.recordRung(results, lr, p)
	})
	return errors.Join(append(errs, err)...)
}

// recordRung logs a ladder's scored rung.
func (b *Bench) recordRung(results *Results, lr *ladderRun, p *pending) error {
	t := p.t
	row := b.encodeRow(p)
	row.Ladder, row.LadderKey = lr.name, lr.key
	if err := results.Append(row); err != nil {
		return err
	}
	m := p.metrics
	pass := b.Cfg.margin(m, t.sample.scoring.Target) >= 0
	slog.Info("Scored ladder rung", "ladder", lr.name, "sample", t.sample.name, "flagset", t.name, "quality", p.quality,
		"kbps", int(row.Kbps), "vmaf", m.VMAF, "pass", pass)
	ev := Event{Source: lr.source, Sample: t.sample.name, Flagset: t.name, QualityFlag: t.fs.Quality.Flag, Quality: p.quality,
		Kbps: row.Kbps, VMAF: m.VMAF}
	ev.Kind, ev.Pass = Scored, pass
	b.observe(ev)
	// Finished, with the rung's encode, which is its answer.
	ev.Kind, ev.Pass, ev.Outcome = Finished, true, Found
	b.observe(ev)
	return nil
}
