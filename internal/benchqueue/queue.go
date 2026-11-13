package benchqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/scottlaird/videoencoder/internal/bench"
)

// Backend does the work a Runner schedules. Tests fake it.
type Backend interface {
	// Status reports cfg's pairs and whether the log answers each, as
	// bench.Bench.Status does.
	Status(ctx context.Context, cfg *bench.Config) ([]bench.PairStatus, error)
	// Run searches flagset on the sample file named sample, as
	// "encbench run -sample sample -flagset flagset" would, passing
	// the run's events to observe. Cancelling ctx abandons it.
	Run(ctx context.Context, cfg *bench.Config, sample, flagset string, observe func(bench.Event)) error
	// Busy says why runs should wait, so their timings stay clean, or
	// returns "" if nothing should hold them up.
	Busy(ctx context.Context) string
	// Report writes cfg's Markdown report to path.
	Report(ctx context.Context, cfg *bench.Config, path string) error
}

// Unit is one flagset on one sample file, from one work file.
type Unit struct {
	File, Sample, Flagset string
}

func (u Unit) String() string { return u.Flagset + " on " + u.Sample }

// Phase is what a Runner is doing.
type Phase int

const (
	// Starting: the first scan hasn't finished.
	Starting Phase = iota
	// Running a unit.
	Running
	// Waiting for State.Waiting to clear before starting a unit.
	Waiting
	// Paused: a unit could run, but Pause holds it.
	Paused
	// Idle: nothing left to run, while watching for more.
	Idle
	// Finished: Run has returned.
	Finished
)

// State is a snapshot of a Runner, for showing.
type State struct {
	Dir     string
	Phase   Phase
	Started time.Time
	// Waiting is why the runner is waiting, in Waiting.
	Waiting string
	// Pausing and Stopping say Pause or Stop was asked for, which
	// takes effect once the current unit ends.
	Pausing, Stopping bool
	// Err is a problem reading the directory itself.
	Err error
	// Current is the unit running, if any.
	Current *Current
	// Files are the work files in priority order.
	Files []FileState
	// Recent are the latest finished units, oldest first.
	Recent []Result
}

// FileState is how far a work file has got.
type FileState struct {
	Name string
	// Err is why the file can't run: it doesn't parse, say. It is
	// skipped until it changes.
	Err error
	// Pending means the file changed during the current unit, and its
	// counts wait until the unit ends.
	Pending bool
	// Done units are answered by the log; Failed ones ran without an
	// answer and won't run again until the file changes.
	Done, Failed int
	// Remaining are the units still to run, in order.
	Remaining []Unit
}

// Total is the number of units in the file.
func (f FileState) Total() int { return f.Done + f.Failed + len(f.Remaining) }

// Current is the unit running now.
type Current struct {
	Unit
	Started time.Time
	// Round is the search's latest round of encodes, and Sample the
	// version (a scaled copy, say) it's encoding.
	Round  int
	Sample string
	// Detail is the latest step, such as "crf 23.4 → VMAF 95.31 pass".
	Detail string
	// Answers are the versions finished so far, as in Result.
	Answers []string
}

// Result is a finished unit.
type Result struct {
	Unit
	Elapsed time.Duration
	// Answers say what each version of the sample settled on.
	Answers []string
	// Err is why the unit failed, or context.Canceled if abandoned.
	Err error
}

const maxRecent = 50

// Runner works through the units of the work files in Dir: the files
// in name order, and each file's units in its order, one unit at a
// time. It rescans Dir before each unit, so a file added with a lower
// name runs next, a deleted file's units drop out, and an edited file
// is parsed again. Units the log already answers are skipped, and so
// are units that ran without an answer, until their file changes.
type Runner struct {
	Dir     string
	Backend Backend
	// Watch keeps Run going when nothing is left to run, waiting for
	// new work; without it, Run returns.
	Watch bool
	// Reports writes each file's report once a unit of it has run and
	// none remain (see workFile.reportPath).
	Reports bool
	// Poll is how often to rescan while waiting or idle, and while a
	// unit runs (parsing only, so the queue shows edits). Default 5s.
	Poll time.Duration
	// Changed, if set, is called whenever the state changes. It must
	// not block.
	Changed func()
	Now     func() time.Time

	once      sync.Once
	mu        sync.Mutex
	state     State
	files     map[string]*entry
	attempted map[Unit]string // unit -> hash of the file it ran under
	dirty     map[string]bool // files with a unit run since their report
	pausing   bool
	stopping  bool
	wake      chan struct{}
}

// entry is what the runner knows of a work file.
type entry struct {
	wf *workFile
	// parseErr is why wf couldn't be loaded, statusErr why its units
	// couldn't be listed.
	parseErr, statusErr error
	// statusKey is what statuses were computed for; "" means not yet.
	statusKey string
	statuses  []bench.PairStatus
	units     []Unit
}

func (r *Runner) init() {
	r.once.Do(func() {
		r.files = map[string]*entry{}
		r.attempted = map[Unit]string{}
		r.dirty = map[string]bool{}
		r.wake = make(chan struct{}, 1)
		if r.Poll == 0 {
			r.Poll = 5 * time.Second
		}
		r.state = State{Dir: r.Dir, Started: r.now()}
	})
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Snapshot returns the runner's current state.
func (r *Runner) Snapshot() State {
	r.init()
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.state
	if s.Current != nil {
		c := *s.Current
		c.Answers = append([]string(nil), c.Answers...)
		s.Current = &c
	}
	s.Files = append([]FileState(nil), s.Files...)
	s.Recent = append([]Result(nil), s.Recent...)
	return s
}

// ResultsLogs returns the results logs (each config's
// workdir/results.csv) that the work files loaded so far write to,
// sorted and without repeats. Files that failed to parse don't count.
func (r *Runner) ResultsLogs() []string {
	r.init()
	r.mu.Lock()
	defer r.mu.Unlock()
	var logs []string
	for _, e := range r.files {
		if e.wf != nil && e.parseErr == nil {
			logs = append(logs, filepath.Join(e.wf.cfg.Workdir, "results.csv"))
		}
	}
	slices.Sort(logs)
	return slices.Compact(logs)
}

// SetPaused pauses the runner once the current unit ends, or resumes
// it.
func (r *Runner) SetPaused(p bool) {
	r.init()
	r.mu.Lock()
	r.pausing = p
	r.state.Pausing = p
	r.mu.Unlock()
	r.poke()
}

// Stop makes Run return once the current unit ends. To abandon the
// unit instead, cancel Run's context.
func (r *Runner) Stop() {
	r.init()
	r.mu.Lock()
	r.stopping = true
	r.state.Stopping = true
	r.mu.Unlock()
	r.poke()
}

func (r *Runner) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
	r.changed()
}

func (r *Runner) changed() {
	if r.Changed != nil {
		r.Changed()
	}
}

// Run works through the queue until Stop, until ctx is done (which
// abandons the current unit), or, without Watch, until nothing is left
// to run. It returns ctx's error if ctx ended it, else nil; failed
// units are logged and shown, not returned.
func (r *Runner) Run(ctx context.Context) error {
	r.init()
	defer func() {
		r.mu.Lock()
		r.state.Phase = Finished
		r.state.Current = nil
		r.mu.Unlock()
		r.changed()
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.scan(ctx, true)
		r.writeReports(ctx)
		r.mu.Lock()
		stopping, pausing := r.stopping, r.pausing
		r.mu.Unlock()
		if stopping {
			return nil
		}
		u, ok := r.next()
		switch {
		case !ok && !r.Watch:
			return nil
		case !ok:
			r.setPhase(Idle, "")
		case pausing:
			r.setPhase(Paused, "")
		default:
			if why := r.Backend.Busy(ctx); why != "" {
				r.setPhase(Waiting, why)
			} else {
				r.runUnit(ctx, u)
				continue
			}
		}
		select {
		case <-ctx.Done():
		case <-r.wake:
		case <-time.After(r.Poll):
		}
	}
}

func (r *Runner) setPhase(p Phase, waiting string) {
	r.mu.Lock()
	changed := r.state.Phase != p || r.state.Waiting != waiting
	r.state.Phase, r.state.Waiting = p, waiting
	r.mu.Unlock()
	if changed && p == Waiting {
		slog.Info("Waiting", "reason", waiting)
	}
	if changed {
		r.changed()
	}
}

// scan rereads Dir. With statuses false, as while a unit runs, it
// only parses files, leaving counts alone, so it starts no ffprobes
// and doesn't reread the log mid-encode.
func (r *Runner) scan(ctx context.Context, statuses bool) {
	entries, dirErr := os.ReadDir(r.Dir)
	seen := map[string]bool{}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !isWorkFile(e.Name()) {
			continue
		}
		names = append(names, e.Name())
		seen[e.Name()] = true
	}

	// Parse without the lock; the file system may be slow.
	parsed := map[string]*workFile{}
	errs := map[string]error{}
	for _, n := range names {
		wf, err := loadWorkFile(filepath.Join(r.Dir, n))
		parsed[n], errs[n] = wf, err
	}

	var logs logBuf
	r.mu.Lock()
	r.state.Err = dirErr
	for n := range r.files {
		if !seen[n] {
			logs.add(slog.LevelInfo, "Work file removed", "file", n)
			delete(r.files, n)
		}
	}
	var stale []string
	for _, n := range names {
		e := r.files[n]
		if e == nil {
			e = &entry{}
			r.files[n] = e
			if errs[n] == nil {
				logs.add(slog.LevelInfo, "Work file added", "file", n, "flagsets", len(parsed[n].flagsets))
			}
		}
		if err := errs[n]; err != nil {
			logs.skip(n, e.err(), err)
			*e = entry{parseErr: err}
			continue
		}
		if e.parseErr != nil {
			logs.add(slog.LevelInfo, "Work file fixed", "file", n)
			e.parseErr = nil
		} else if e.wf != nil && e.wf.hash != parsed[n].hash {
			logs.add(slog.LevelInfo, "Work file changed", "file", n)
		}
		if e.wf == nil || e.wf.hash != parsed[n].hash {
			e.statusKey = ""
		}
		e.wf = parsed[n]
		if statuses && e.statusKey != statusKey(e.wf) {
			stale = append(stale, n)
		}
	}
	r.mu.Unlock()

	// Statuses probe samples and read the log: slow, so also unlocked.
	type result struct {
		key string
		ps  []bench.PairStatus
		err error
	}
	fresh := map[string]result{}
	for _, n := range stale {
		wf := parsed[n]
		key := statusKey(wf)
		ps, err := r.Backend.Status(ctx, wf.cfg)
		fresh[n] = result{key, ps, err}
	}

	r.mu.Lock()
	for n, res := range fresh {
		e := r.files[n]
		if e == nil || e.wf != parsed[n] {
			continue
		}
		if ctx.Err() != nil {
			continue // try again next time
		}
		units, err := res.ps, error(nil)
		var us []Unit
		if res.err != nil {
			err = res.err
		} else {
			us, err = orderUnits(e.wf, units)
		}
		logs.skip(n, e.statusErr, err)
		e.statusKey, e.statusErr = res.key, err
		e.statuses, e.units = nil, nil
		if err == nil {
			e.statuses, e.units = res.ps, us
		}
	}
	r.rebuild(names)
	r.mu.Unlock()
	logs.flush()
	r.changed()
}

func (e *entry) err() error {
	if e.parseErr != nil {
		return e.parseErr
	}
	return e.statusErr
}

// logBuf holds log records made under r.mu until it's released, since
// a handler (the terminal UI's) may wait on a redraw that needs it.
type logBuf []func()

func (l *logBuf) add(level slog.Level, msg string, args ...any) {
	*l = append(*l, func() { slog.Log(context.Background(), level, msg, args...) })
}

// skip logs a file's error, once: when it differs from the last.
func (l *logBuf) skip(name string, old, err error) {
	if err != nil && (old == nil || old.Error() != err.Error()) {
		l.add(slog.LevelError, "Work file skipped until it changes", "file", name, "err", err)
	}
}

func (l logBuf) flush() {
	for _, f := range l {
		f()
	}
}

// statusKey changes whenever a file's statuses might: when it, its
// log or its samples do.
func statusKey(wf *workFile) string {
	key := wf.hash
	for _, p := range []string{filepath.Join(wf.cfg.Workdir, "results.csv"), wf.cfg.Samples} {
		if st, err := os.Stat(p); err == nil {
			key += fmt.Sprintf("|%d.%d", st.Size(), st.ModTime().UnixNano())
		}
	}
	return key
}

// orderUnits lists wf's units in run order: flagsets outer, samples
// inner, leaving out pairs with nothing to search.
func orderUnits(wf *workFile, ps []bench.PairStatus) ([]Unit, error) {
	var samples []string
	have := map[[2]string]bool{}
	for _, p := range ps {
		if len(samples) == 0 || samples[len(samples)-1] != p.Sample {
			samples = append(samples, p.Sample)
		}
		if !p.Skipped {
			have[[2]string{p.Sample, p.Flagset}] = true
		}
	}
	if wf.samples != nil {
		var err error
		if samples, err = expand(wf.samples, samples, "sample"); err != nil {
			return nil, err
		}
	}
	var units []Unit
	for _, f := range wf.flagsets {
		for _, s := range samples {
			if have[[2]string{s, f}] {
				units = append(units, Unit{File: wf.name, Sample: s, Flagset: f})
			}
		}
	}
	return units, nil
}

// rebuild recomputes State.Files from the entries. r.mu must be held.
func (r *Runner) rebuild(names []string) {
	var files []FileState
	for _, n := range names {
		e := r.files[n]
		if e == nil {
			continue
		}
		fs := FileState{Name: n, Err: e.err()}
		if fs.Err == nil && e.statusKey == "" {
			fs.Pending = true
		}
		if fs.Err == nil && e.statusKey != "" {
			done := map[[2]string]bool{}
			for _, p := range e.statuses {
				done[[2]string{p.Sample, p.Flagset}] = p.Done
			}
			for _, u := range e.units {
				switch {
				case done[[2]string{u.Sample, u.Flagset}]:
					fs.Done++
				case r.attempted[u] == e.wf.hash:
					fs.Failed++
				default:
					fs.Remaining = append(fs.Remaining, u)
				}
			}
		}
		files = append(files, fs)
	}
	r.state.Files = files
}

// next returns the first unit still to run.
func (r *Runner) next() (Unit, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.state.Files {
		if f.Err == nil && !f.Pending && len(f.Remaining) > 0 {
			return f.Remaining[0], true
		}
	}
	return Unit{}, false
}

func (r *Runner) runUnit(ctx context.Context, u Unit) {
	r.mu.Lock()
	e := r.files[u.File]
	wf := e.wf
	cur := &Current{Unit: u, Started: r.now()}
	r.state.Current = cur
	r.state.Phase, r.state.Waiting = Running, ""
	r.mu.Unlock()
	r.changed()
	slog.Info("Starting", "file", u.File, "flagset", u.Flagset, "sample", u.Sample)

	// Keep parsing while the unit runs, so the queue shows edits and
	// bad files.
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-time.After(r.Poll):
				r.scan(ctx, false)
			}
		}
	})

	var failures []string
	// step is the running encode's Detail, before its progress.
	var step string
	err := r.Backend.Run(ctx, wf.cfg, u.Sample, u.Flagset, func(ev bench.Event) {
		r.mu.Lock()
		switch ev.Kind {
		case bench.Encoding:
			cur.Round, cur.Sample = ev.Round, ev.Sample
			qs := []string{ftoa(ev.Quality)}
			if len(ev.Qualities) > 1 {
				qs = qs[:0]
				for _, q := range ev.Qualities {
					qs = append(qs, ftoa(q))
				}
			}
			step = fmt.Sprintf("encoding %s %s", flagName(ev.QualityFlag), strings.Join(qs, ", "))
			cur.Detail = step
		case bench.Timing:
			cur.Round, cur.Sample = ev.Round, ev.Sample
			step = fmt.Sprintf("timing %s %s by itself", flagName(ev.QualityFlag), ftoa(ev.Quality))
			cur.Detail = step
		case bench.Encoded:
			cur.Detail = step + " · " + progressText(ev.Progress)
		case bench.Scored:
			verdict := "fail"
			if ev.Pass {
				verdict = "pass"
			}
			cur.Sample = ev.Sample
			cur.Detail = fmt.Sprintf("%s %s → VMAF %.2f %s", flagName(ev.QualityFlag), ftoa(ev.Quality), ev.VMAF, verdict)
		case bench.Failed:
			failures = append(failures, fmt.Sprintf("%s: %v", ev.Sample, ev.Err))
			cur.Answers = append(cur.Answers, fmt.Sprintf("%s failed: %v", ev.Sample, ev.Err))
		case bench.Finished:
			cur.Answers = append(cur.Answers, answer(ev))
		}
		r.mu.Unlock()
		r.changed()
	})
	close(done)
	wg.Wait()

	if err == nil && len(failures) > 0 {
		err = errors.New(strings.Join(failures, "; "))
	}
	elapsed := r.now().Sub(cur.Started)
	switch {
	case ctx.Err() != nil:
		err = context.Canceled
		slog.Warn("Abandoned", "file", u.File, "flagset", u.Flagset, "sample", u.Sample)
	case err != nil:
		slog.Error("Failed; skipped until its file changes", "file", u.File, "flagset", u.Flagset, "sample", u.Sample, "err", err)
	default:
		slog.Info("Done", "file", u.File, "flagset", u.Flagset, "sample", u.Sample, "elapsed", elapsed.Round(time.Second))
	}

	r.mu.Lock()
	if ctx.Err() == nil {
		r.attempted[u] = wf.hash
		r.dirty[u.File] = true
	}
	// The log has moved on: every file's counts are stale.
	for _, e := range r.files {
		e.statusKey = ""
	}
	res := Result{Unit: u, Elapsed: elapsed, Answers: cur.Answers, Err: err}
	r.state.Recent = append(r.state.Recent, res)
	if len(r.state.Recent) > maxRecent {
		r.state.Recent = r.state.Recent[len(r.state.Recent)-maxRecent:]
	}
	r.state.Current = nil
	r.mu.Unlock()
	r.changed()
}

// writeReports writes the report of each file that has run a unit
// since its last report and has none left.
func (r *Runner) writeReports(ctx context.Context) {
	if !r.Reports {
		return
	}
	type todo struct {
		name string
		wf   *workFile
	}
	var due []todo
	r.mu.Lock()
	for _, f := range r.state.Files {
		e := r.files[f.Name]
		if r.dirty[f.Name] && f.Err == nil && !f.Pending && len(f.Remaining) == 0 && e != nil && e.wf != nil {
			due = append(due, todo{f.Name, e.wf})
			delete(r.dirty, f.Name)
		}
	}
	r.mu.Unlock()
	for _, d := range due {
		p := d.wf.reportPath(r.Dir)
		if err := r.Backend.Report(ctx, d.wf.cfg, p); err != nil {
			slog.Error("Couldn't write report", "file", d.name, "err", err)
			continue
		}
		slog.Info("Wrote report", "file", d.name, "report", p)
	}
}

// flagName is a quality flag without its dash: "crf", "q:v".
// progressText says how far an encode has got: "15% · 2:00, about
// 12:30 left".
func progressText(p bench.Progress) string {
	s := fmt.Sprintf("%d%% · %s", int(p.Fraction()*100), clock(p.Elapsed))
	if left, ok := p.Remaining(); ok {
		s += ", about " + clock(left) + " left"
	}
	return s
}

// clock writes d as m:ss, or h:mm:ss from an hour.
func clock(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func flagName(f string) string { return strings.TrimLeft(f, "-") }

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// answer says what one version of a unit's sample settled on.
func answer(ev bench.Event) string {
	switch {
	case ev.Pass:
		return fmt.Sprintf("%s %s %s · %.0f kbps · VMAF %.2f", ev.Sample, flagName(ev.QualityFlag), ftoa(ev.Quality), ev.Kbps, ev.VMAF)
	case ev.Outcome == bench.NonePass:
		return ev.Sample + " none pass"
	default:
		return fmt.Sprintf("%s %v", ev.Sample, ev.Outcome)
	}
}
