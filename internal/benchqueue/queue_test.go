package benchqueue

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scottlaird/videoencoder/internal/bench"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

// fake is a Backend whose samples are the files in a config's samples
// directory, and whose log is the set of units it has run.
type fake struct {
	mu      sync.Mutex
	done    map[[2]string]bool // sample, flagset
	ran     []string           // "file-less" unit names, in run order
	reports []string
	// during, if set, runs inside each Run, before it finishes.
	during func(n int, sample, flagset string)
	// fail makes Run fail for these flagsets.
	fail map[string]bool
	// busy is returned by Busy, popping one per call.
	busy []string
	// sdrOnly flagsets skip samples named hdr*.
	sdrOnly map[string]bool
}

func newFakeBackend() *fake {
	return &fake{done: map[[2]string]bool{}, fail: map[string]bool{}, sdrOnly: map[string]bool{}}
}

func (f *fake) Status(_ context.Context, cfg *bench.Config) ([]bench.PairStatus, error) {
	entries, err := os.ReadDir(cfg.Samples)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []bench.PairStatus
	for _, e := range entries {
		for _, fs := range cfg.FlagsetNames() {
			out = append(out, bench.PairStatus{
				Sample: e.Name(), Flagset: fs,
				Done:    f.done[[2]string{e.Name(), fs}],
				Skipped: f.sdrOnly[fs] && strings.HasPrefix(e.Name(), "hdr"),
			})
		}
	}
	return out, nil
}

func (f *fake) Run(ctx context.Context, _ *bench.Config, sample, flagset string, observe func(bench.Event)) error {
	f.mu.Lock()
	f.ran = append(f.ran, flagset+" "+sample)
	n := len(f.ran)
	during := f.during
	f.mu.Unlock()
	observe(bench.Event{Kind: bench.Encoding, Round: 1, Source: sample, Sample: sample, Flagset: flagset, QualityFlag: "-crf", Quality: 25})
	if during != nil {
		during(n, sample, flagset)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.fail[flagset] {
		return errors.New("encoder exploded")
	}
	observe(bench.Event{Kind: bench.Scored, Round: 1, Source: sample, Sample: sample, Flagset: flagset, QualityFlag: "-crf", Quality: 25, VMAF: 95.5, Pass: true})
	observe(bench.Event{Kind: bench.Finished, Source: sample, Sample: sample, Flagset: flagset, QualityFlag: "-crf", Quality: 25, VMAF: 95.5, Kbps: 4000, Pass: true, Outcome: bench.Found})
	f.mu.Lock()
	f.done[[2]string{sample, flagset}] = true
	f.mu.Unlock()
	return nil
}

func (f *fake) Busy(context.Context) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.busy) == 0 {
		return ""
	}
	b := f.busy[0]
	f.busy = f.busy[1:]
	return b
}

func (f *fake) Report(_ context.Context, _ *bench.Config, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, filepath.Base(path))
	return nil
}

func (f *fake) ranUnits() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ran)
}

// setup makes a bench dir with samples a.mov and b.mov, a shared
// config with flagsets z, y and x (in that order), and an empty work
// directory, which it returns.
func setup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, s := range []string{"a.mov", "b.mov"} {
		write(t, filepath.Join(dir, "samples", s), "video")
	}
	write(t, filepath.Join(dir, "shared.yaml"), strings.ReplaceAll(config("z", "y", "x"), "../", ""))
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	return work
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// config is a bench config, for a file in the work directory or
// beside it, with the named flagsets in that order.
func config(flagsets ...string) string {
	s := "samples: ../samples\nworkdir: ../bench\nflagsets:\n"
	for _, f := range flagsets {
		s += "  " + f + ": {args: [-c:v, libx265, -preset, " + f + "]}\n"
	}
	return s
}

func newRunner(work string, b *fake) *Runner {
	return &Runner{Dir: work, Backend: b, Poll: time.Millisecond}
}

func run(t *testing.T, r *Runner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatalf("Run() = %v", err)
	}
}

func checkRan(t *testing.T, b *fake, want ...string) {
	t.Helper()
	if got := b.ranUnits(); !slices.Equal(got, want) {
		t.Errorf("ran\n  %q\nwant\n  %q", got, want)
	}
}

func TestOrder(t *testing.T) {
	work := setup(t)
	// Names, not times, set the order; flagsets keep the file's order
	// (not sorted), and each runs on every sample before the next.
	write(t, filepath.Join(work, "10-later.yaml"), config("q"))
	write(t, filepath.Join(work, "00-first.yaml"), config("m", "c"))
	// A job: its own flagset order, patterns expanding in the
	// config's order, and samples in its order.
	write(t, filepath.Join(work, "05-job.yml"), "config: ../shared.yaml\nflagsets: [x, '[yz]']\nsamples: [b.mov, a.mov]\n")
	// Not work files.
	write(t, filepath.Join(work, "notes.txt"), "hello")
	write(t, filepath.Join(work, ".00-hidden.yaml"), config("h"))
	write(t, filepath.Join(work, "report-00-first.md"), "old report")

	b := newFakeBackend()
	r := newRunner(work, b)
	run(t, r)
	checkRan(t, b,
		"m a.mov", "m b.mov", "c a.mov", "c b.mov",
		"x b.mov", "x a.mov", "z b.mov", "z a.mov", "y b.mov", "y a.mov",
		"q a.mov", "q b.mov")

	s := r.Snapshot()
	if s.Phase != Finished {
		t.Errorf("phase after Run = %v, want Finished", s.Phase)
	}
	var names []string
	for _, f := range s.Files {
		names = append(names, f.Name)
		if f.Done != f.Total() || f.Err != nil {
			t.Errorf("%s: %d/%d done, err %v; want all done", f.Name, f.Done, f.Total(), f.Err)
		}
	}
	if want := []string{"00-first.yaml", "05-job.yml", "10-later.yaml"}; !slices.Equal(names, want) {
		t.Errorf("files = %q, want %q", names, want)
	}
	if len(s.Recent) != 12 || !strings.Contains(s.Recent[0].Answers[0], "a.mov crf 25 · 4000 kbps · VMAF 95.50") {
		t.Errorf("recent results = %+v", s.Recent)
	}
}

func TestSkipsDoneAndSkippedUnits(t *testing.T) {
	work := setup(t)
	write(t, filepath.Join(work, "a.yaml"), config("x", "h264"))
	write(t, filepath.Join(work, "../samples/hdr.mov"), "video")
	b := newFakeBackend()
	b.done[[2]string{"a.mov", "x"}] = true
	b.sdrOnly["h264"] = true
	run(t, newRunner(work, b))
	checkRan(t, b, "x b.mov", "x hdr.mov", "h264 a.mov", "h264 b.mov")
}

func TestAddDuringRun(t *testing.T) {
	work := setup(t)
	write(t, filepath.Join(work, "10-b.yaml"), config("b1", "b2"))
	b := newFakeBackend()
	b.during = func(n int, _, _ string) {
		if n == 1 {
			// A lower name jumps the queue after the current unit;
			// a higher one waits its turn.
			write(t, filepath.Join(work, "05-a.yaml"), config("a1"))
			write(t, filepath.Join(work, "20-c.yaml"), config("c1"))
		}
	}
	run(t, newRunner(work, b))
	checkRan(t, b, "b1 a.mov", "a1 a.mov", "a1 b.mov", "b1 b.mov", "b2 a.mov", "b2 b.mov", "c1 a.mov", "c1 b.mov")
}

func TestDeleteDuringRun(t *testing.T) {
	work := setup(t)
	write(t, filepath.Join(work, "1.yaml"), config("one"))
	write(t, filepath.Join(work, "2.yaml"), config("two"))
	b := newFakeBackend()
	b.during = func(n int, _, _ string) {
		if n == 1 {
			os.Remove(filepath.Join(work, "1.yaml"))
		}
	}
	r := newRunner(work, b)
	run(t, r)
	checkRan(t, b, "one a.mov", "two a.mov", "two b.mov")
	if s := r.Snapshot(); len(s.Files) != 1 || s.Files[0].Name != "2.yaml" {
		t.Errorf("files after delete = %+v, want 2.yaml alone", s.Files)
	}
}

func TestEditDuringRun(t *testing.T) {
	work := setup(t)
	path := filepath.Join(work, "1.yaml")
	write(t, path, config("one", "two"))
	b := newFakeBackend()
	// "new" is already answered in the log, so it needn't run.
	b.done[[2]string{"a.mov", "new"}] = true
	b.during = func(n int, _, _ string) {
		if n == 1 {
			// Drop "two", put "new" first and "three" after "one".
			write(t, path, config("new", "one", "three"))
		}
	}
	run(t, newRunner(work, b))
	checkRan(t, b, "one a.mov", "new b.mov", "one b.mov", "three a.mov", "three b.mov")
}

func TestParseErrors(t *testing.T) {
	work := setup(t)
	bad := filepath.Join(work, "0-bad.yaml")
	write(t, bad, "samples: ../samples\nflagsets: {x: [unclosed\n")
	write(t, filepath.Join(work, "1-typo.yaml"), "config: ../shared.yaml\nflagsets: [nope]\n")
	write(t, filepath.Join(work, "2-sample-typo.yaml"), "config: ../shared.yaml\nsamples: [c.mov]\n")
	write(t, filepath.Join(work, "3-good.yaml"), config("g"))
	b := newFakeBackend()
	r := newRunner(work, b)
	run(t, r)
	checkRan(t, b, "g a.mov", "g b.mov")

	errs := map[string]string{}
	for _, f := range r.Snapshot().Files {
		if f.Err != nil {
			errs[f.Name] = f.Err.Error()
		}
	}
	for name, want := range map[string]string{"0-bad.yaml": "parsing", "1-typo.yaml": `"nope" matches no flagset`, "2-sample-typo.yaml": `"c.mov" matches no sample`} {
		if !strings.Contains(errs[name], want) {
			t.Errorf("%s: error %q, want one mentioning %q", name, errs[name], want)
		}
	}
	if len(errs) != 3 {
		t.Errorf("errors = %q, want three", errs)
	}

	// Fixed, it runs; broken mid-run, its other units stop.
	write(t, bad, config("fixed", "more"))
	b.during = func(n int, _, _ string) {
		if n == 3 {
			write(t, bad, "flagsets: [\n")
		}
	}
	run(t, r)
	checkRan(t, b, "g a.mov", "g b.mov", "fixed a.mov")
	if f := r.Snapshot().Files[0]; f.Err == nil {
		t.Errorf("%s broken mid-run shows no error", f.Name)
	}
}

func TestFailedUnits(t *testing.T) {
	work := setup(t)
	path := filepath.Join(work, "1.yaml")
	write(t, path, config("broken", "ok"))
	b := newFakeBackend()
	b.fail["broken"] = true
	r := newRunner(work, b)
	run(t, r)
	// Failures aren't retried in a loop, and don't stop the rest.
	checkRan(t, b, "broken a.mov", "broken b.mov", "ok a.mov", "ok b.mov")
	s := r.Snapshot()
	if f := s.Files[0]; f.Failed != 2 || f.Done != 2 || len(f.Remaining) != 0 {
		t.Errorf("counts = %d done, %d failed, %d remaining; want 2, 2, 0", f.Done, f.Failed, len(f.Remaining))
	}
	if s.Recent[0].Err == nil {
		t.Errorf("failed unit's result has no error")
	}

	// Run again unchanged: nothing to do. Edited: the failures retry.
	run(t, r)
	checkRan(t, b, "broken a.mov", "broken b.mov", "ok a.mov", "ok b.mov")
	b.fail["broken"] = false
	write(t, path, "# fixed the encoder\n"+config("broken", "ok"))
	run(t, r)
	checkRan(t, b, "broken a.mov", "broken b.mov", "ok a.mov", "ok b.mov", "broken a.mov", "broken b.mov")
}

func TestWaitsWhileBusy(t *testing.T) {
	work := setup(t)
	write(t, filepath.Join(work, "1.yaml"), config("x"))
	b := newFakeBackend()
	b.busy = []string{"Resolve is rendering", "Resolve is rendering"}
	r := newRunner(work, b)
	var mu sync.Mutex
	var waits []string
	r.Changed = func() {
		s := r.Snapshot()
		if s.Phase == Waiting {
			mu.Lock()
			if len(waits) == 0 || waits[len(waits)-1] != s.Waiting {
				waits = append(waits, s.Waiting)
			}
			mu.Unlock()
		}
	}
	run(t, r)
	checkRan(t, b, "x a.mov", "x b.mov")
	if want := []string{"Resolve is rendering"}; !slices.Equal(waits, want) {
		t.Errorf("waited for %q, want %q", waits, want)
	}
}

func TestPauseAndStop(t *testing.T) {
	work := setup(t)
	write(t, filepath.Join(work, "1.yaml"), config("x", "y"))
	b := newFakeBackend()
	r := newRunner(work, b)
	paused := make(chan struct{})
	b.during = func(n int, _, _ string) {
		switch n {
		case 1:
			r.SetPaused(true)
		case 2:
			r.Stop()
		}
	}
	r.Changed = func() {
		if s := r.Snapshot(); s.Phase == Paused {
			select {
			case paused <- struct{}{}:
			default:
			}
		}
	}
	errc := make(chan error)
	go func() { errc <- r.Run(context.Background()) }()
	select {
	case <-paused:
	case <-time.After(10 * time.Second):
		t.Fatal("never paused")
	}
	// The unit running when pause came finished; no other started.
	time.Sleep(20 * time.Millisecond)
	checkRan(t, b, "x a.mov")
	r.SetPaused(false)
	// Stop during the second unit lets it finish, then returns.
	if err := <-errc; err != nil {
		t.Fatalf("Run() = %v", err)
	}
	checkRan(t, b, "x a.mov", "x b.mov")
	if !b.done[[2]string{"b.mov", "x"}] {
		t.Error("unit running at Stop didn't finish")
	}
}

func TestAbandon(t *testing.T) {
	work := setup(t)
	write(t, filepath.Join(work, "1.yaml"), config("x"))
	b := newFakeBackend()
	ctx, cancel := context.WithCancel(context.Background())
	b.during = func(n int, _, _ string) {
		if n == 1 {
			cancel()
		}
	}
	r := newRunner(work, b)
	if err := r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	s := r.Snapshot()
	if len(s.Recent) != 1 || !errors.Is(s.Recent[0].Err, context.Canceled) {
		t.Errorf("recent = %+v, want the abandoned unit", s.Recent)
	}
	// Abandoned isn't failed: the next run starts it again.
	b.during = nil
	run(t, r)
	checkRan(t, b, "x a.mov", "x a.mov", "x b.mov")
}

func TestWatch(t *testing.T) {
	work := setup(t)
	b := newFakeBackend()
	r := newRunner(work, b)
	r.Watch = true
	idle := make(chan struct{}, 1)
	r.Changed = func() {
		if r.Snapshot().Phase == Idle {
			select {
			case idle <- struct{}{}:
			default:
			}
		}
	}
	errc := make(chan error)
	go func() { errc <- r.Run(context.Background()) }()
	<-idle // an empty directory doesn't end a watch
	b.during = func(n int, _, _ string) {
		if n == 2 {
			r.Stop()
		}
	}
	write(t, filepath.Join(work, "1.yaml"), config("x"))
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Run() = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run didn't pick up the new file and stop")
	}
	checkRan(t, b, "x a.mov", "x b.mov")
}

func TestReports(t *testing.T) {
	work := setup(t)
	write(t, filepath.Join(work, "1-a.yaml"), config("x"))
	write(t, filepath.Join(work, "2-job.yaml"), "config: ../shared.yaml\nflagsets: [z]\nreport: ../custom.md\n")
	// Finished before this run: no report, since nothing ran.
	write(t, filepath.Join(work, "3-old.yaml"), config("old"))
	b := newFakeBackend()
	b.done[[2]string{"a.mov", "old"}] = true
	b.done[[2]string{"b.mov", "old"}] = true
	r := newRunner(work, b)
	r.Reports = true
	run(t, r)
	if want := []string{"report-1-a.md", "custom.md"}; !slices.Equal(b.reports, want) {
		t.Errorf("reports = %q, want %q", b.reports, want)
	}
	run(t, r)
	if len(b.reports) != 2 {
		t.Errorf("rerun with nothing new: reports %q", b.reports)
	}
}

func TestResolveRendering(t *testing.T) {
	head := "COMMAND   PID  USER   FD   TYPE DEVICE SIZE/OFF NODE NAME\n"
	for _, tt := range []struct {
		lsof string
		want bool
	}{
		{"", false},
		{head + "Resolve 123 scott  45r  REG 1,15 100 2 /Volumes/Media/clip.mov\n", false},
		{head + "Resolve 123 scott  46w  REG 1,15 100 2 /Volumes/Render/out.mov\n", true},
		{head + "Resolve 123 scott  47u  REG 1,15 100 2 /Volumes/Render/My Render.mov\n", true},
		{head + "Resolve 123 scott  48w  REG 1,15 100 2 /Users/scott/log.txt\n", false},
	} {
		if got := resolveRendering(tt.lsof); got != tt.want {
			t.Errorf("resolveRendering(%q) = %v, want %v", tt.lsof, got, tt.want)
		}
	}
}

func TestOtherPID(t *testing.T) {
	for _, tt := range []struct {
		pgrep string
		want  int
	}{{"", 0}, {"42\n", 0}, {"42\n77\n", 77}, {"77\n42\n", 77}} {
		if got := otherPID(tt.pgrep, 42); got != tt.want {
			t.Errorf("otherPID(%q, 42) = %d, want %d", tt.pgrep, got, tt.want)
		}
	}
}

func TestGlobEscape(t *testing.T) {
	for _, name := range []string{"20251206-heron3 test 30s.mov", "a[1]*?.mov", `back\slash.mov`} {
		if ok, err := filepath.Match(globEscape(name), name); !ok || err != nil {
			t.Errorf("Match(globEscape(%q)) = %v, %v; want a match", name, ok, err)
		}
		if ok, _ := filepath.Match(globEscape(name), name+"x"); ok {
			t.Errorf("globEscape(%q) matches more than itself", name)
		}
	}
}

func TestProgressText(t *testing.T) {
	for _, tc := range []struct {
		p    bench.Progress
		want string
	}{
		{bench.Progress{Encoded: 3 * time.Second, Total: 20 * time.Second, Elapsed: 2 * time.Minute}, "15% · 2:00, about 11:20 left"},
		{bench.Progress{Encoded: 0, Total: 20 * time.Second, Elapsed: time.Second}, "0% · 0:01"},
		{bench.Progress{Encoded: time.Second, Total: 30 * time.Second, Elapsed: 5 * time.Minute}, "3% · 5:00, about 2:25:00 left"},
	} {
		if got := progressText(tc.p); got != tc.want {
			t.Errorf("progressText(%+v) = %q, want %q", tc.p, got, tc.want)
		}
	}
}
