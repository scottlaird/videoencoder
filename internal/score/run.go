package score

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/scottlaird/videoencoder/internal/benchweb"
)

// Runner scores what its jobs select, encode by encode, until nothing
// is left (or, with Watch, forever, looking for new rows every Poll).
type Runner struct {
	// Jobs returns the jobs to work on; it's called on every pass, so
	// work files can come and go.
	Jobs  func() ([]*Job, error)
	Log   *Log
	Cache *Cache
	CVVDP *CVVDP
	// Host names this machine in the log; Tmp is for scratch files.
	Host, Tmp string
	// Batch is the most encodes scored in one run of cvvdp (default 8):
	// each run loads the model once, but a failure loses the batch.
	Batch int
	Watch bool
	Poll  time.Duration
	// Changed, if set, is called when State changes.
	Changed func()
	// Now is for tests.
	Now func() time.Time

	mu       sync.Mutex
	state    State
	failures map[string]int // by Key
}

// State is what a Runner is doing.
type State struct {
	Phase   string    `json:"phase"` // "planning", "scoring", "waiting" or "done"
	Pending int       `json:"pending"`
	Current *Group    `json:"current"`
	Scored  int       `json:"scored"`
	Failed  int       `json:"failed"`
	LastErr string    `json:"last_error,omitempty"`
	Updated time.Time `json:"updated"`
}

// Group is encodes of one sample, scored by one scorer against one
// reference, fetched from one queue.
type Group struct {
	From     string `json:"from"`
	Scorer   string `json:"scorer"`
	SampleID string `json:"sample_id"`
	Sample   string `json:"sample"`
	Encodes  int    `json:"encodes"`

	client  *Client
	cfg     *ScorerConfig
	refKind string // "sample" or "reference"
	refSize int64
	items   []item
}

type item struct {
	key, sha string
	size     int64
	row      *benchweb.ResultRow
}

// maxFailures is how many times a scoring may fail before the runner
// stops trying it until restarted.
const maxFailures = 3

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Snapshot returns the runner's state.
func (r *Runner) Snapshot() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.state
	if s.Current != nil {
		c := *s.Current
		s.Current = &c
	}
	return s
}

func (r *Runner) update(f func(*State)) {
	r.mu.Lock()
	f(&r.state)
	r.state.Updated = r.now()
	r.mu.Unlock()
	if r.Changed != nil {
		r.Changed()
	}
}

// Run works until there's nothing left to score (or ctx ends, with
// Watch).
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.failures == nil {
		r.failures = map[string]int{}
	}
	r.mu.Unlock()
	poll := cmp.Or(r.Poll, time.Minute)
	for {
		r.update(func(s *State) { s.Phase, s.Current = "planning", nil })
		groups, err := r.plan(ctx)
		if err != nil {
			if !r.Watch {
				return err
			}
			slog.Error("Planning failed", "err", err)
			r.update(func(s *State) { s.LastErr = err.Error() })
		}
		pending := 0
		for _, g := range groups {
			pending += len(g.items)
		}
		r.update(func(s *State) { s.Pending = pending })
		for _, g := range groups {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.scoreGroup(ctx, g)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !r.Watch {
			if len(groups) == 0 {
				r.update(func(s *State) { s.Phase, s.Current = "done", nil })
				return nil
			}
			continue // scores may have unblocked nothing, but look again until a pass is empty
		}
		if len(groups) > 0 {
			continue
		}
		r.update(func(s *State) { s.Phase, s.Current = "waiting", nil })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// plan lists what's left to score, grouped for scoring.
func (r *Runner) plan(ctx context.Context) ([]*Group, error) {
	jobs, err := r.Jobs()
	if err != nil {
		return nil, err
	}
	type host struct {
		client *Client
		res    *benchweb.Results
		idx    *benchweb.FileIndex
		err    error
	}
	hosts := map[string]*host{}
	byKey := map[string]*Group{}
	planned := map[string]bool{}
	var groups []*Group
	var errs []error
	for _, j := range jobs {
		for _, base := range j.Hosts {
			h, ok := hosts[base+"\x00"+j.Token]
			if !ok {
				h = &host{client: &Client{Base: base, Token: j.Token}}
				if h.res, h.err = h.client.Results(ctx); h.err == nil {
					h.idx, h.err = h.client.Index(ctx)
				}
				hosts[base+"\x00"+j.Token] = h
				if h.err != nil {
					slog.Warn("Skipping queue", "host", base, "err", h.err)
					errs = append(errs, h.err)
				}
			}
			if h.err != nil {
				continue
			}
			encodes := map[string]int64{}
			for _, e := range h.idx.Encodes {
				encodes[e.SHA256] = e.Bytes
			}
			samples := map[string]benchweb.SampleEntry{}
			for _, s := range h.idx.Samples {
				samples[s.SampleID] = s
			}
			for i := range h.res.Rows {
				row := &h.res.Rows[i]
				size, ok := encodes[row.OutSHA256]
				smp, sok := samples[row.SampleID]
				if !ok || !sok || !j.Select.picks(row) {
					continue
				}
				for k := range j.Scorers {
					cfg := &j.Scorers[k]
					if !cfg.applies(row) {
						continue
					}
					name, err := r.CVVDP.ScorerName(cfg)
					if err != nil {
						return nil, err
					}
					key := Key(name, row.SampleID, row.OutSHA256)
					r.mu.Lock()
					failed := r.failures[key] >= maxFailures
					r.mu.Unlock()
					if planned[key] || failed || r.Log.Has(key) {
						continue
					}
					planned[key] = true
					gk := base + "\x00" + name + "\x00" + row.SampleID
					g := byKey[gk]
					if g == nil {
						g = &Group{From: base, Scorer: name, SampleID: row.SampleID, Sample: row.Sample,
							client: h.client, cfg: cfg, refKind: "sample", refSize: smp.Bytes}
						if smp.ReferenceBytes > 0 {
							g.refKind, g.refSize = "reference", smp.ReferenceBytes
						}
						byKey[gk] = g
						groups = append(groups, g)
					}
					g.items = append(g.items, item{key: key, sha: row.OutSHA256, size: size, row: row})
					g.Encodes++
				}
			}
		}
	}
	if len(groups) == 0 && len(errs) > 0 && len(errs) == len(hosts) {
		return nil, fmt.Errorf("no queue answered: %w", errs[0])
	}
	return groups, nil
}

// scoreGroup scores a group's encodes, a batch at a time, logging
// each batch's scores as it finishes.
func (r *Runner) scoreGroup(ctx context.Context, g *Group) {
	r.update(func(s *State) { s.Phase, s.Current = "scoring", g })
	fail := func(items []item, err error) {
		slog.Error("Scoring failed", "from", g.From, "sample", g.Sample, "scorer", g.Scorer, "encodes", len(items), "err", err)
		r.mu.Lock()
		for _, it := range items {
			r.failures[it.key]++
		}
		r.mu.Unlock()
		r.update(func(s *State) { s.Failed += len(items); s.Pending -= len(items); s.LastErr = err.Error() })
	}
	ref, err := r.Cache.Sample(ctx, g.client, g.refKind, g.SampleID, g.refSize)
	if err != nil {
		fail(g.items, err)
		return
	}
	defer r.Cache.Release(ref)
	for batch := range slices.Chunk(g.items, cmp.Or(r.Batch, 8)) {
		if ctx.Err() != nil {
			return
		}
		var tests []string
		var ok []item
		for _, it := range batch {
			p, err := r.Cache.Encode(ctx, g.client, it.sha, it.size)
			if err != nil {
				fail([]item{it}, err)
				continue
			}
			tests = append(tests, p)
			ok = append(ok, it)
		}
		if len(tests) == 0 {
			continue
		}
		start := r.now()
		jods, err := r.CVVDP.Score(ctx, g.cfg, ref, tests, r.Tmp)
		r.Cache.Release(tests...)
		if err != nil {
			if ctx.Err() == nil {
				fail(ok, err)
			}
			continue
		}
		each := r.now().Sub(start).Seconds() / float64(len(ok))
		var scores []Score
		for i, it := range ok {
			scores = append(scores, Score{Key: it.key, Scorer: g.Scorer, Metric: "jod", Value: jods[i],
				SampleID: g.SampleID, SourceID: it.row.SourceID, OutSHA256: it.sha, ScoreS: each,
				Host: r.Host, Device: g.cfg.Device, Time: r.now().UTC(), From: g.From,
				Sample: it.row.Sample, Flagset: it.row.Flagset, Quality: it.row.Quality})
		}
		if err := r.Log.Append(scores...); err != nil {
			fail(ok, err)
			continue
		}
		slog.Info("Scored", "sample", g.Sample, "scorer", g.Scorer, "encodes", len(ok), "s_each", fmt.Sprintf("%.1f", each))
		r.update(func(s *State) { s.Scored += len(ok); s.Pending -= len(ok) })
	}
}
