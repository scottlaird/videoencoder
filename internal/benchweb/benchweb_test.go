package benchweb

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scottlaird/videoencoder/internal/bench"
	"github.com/scottlaird/videoencoder/internal/benchqueue"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

func TestLogRing(t *testing.T) {
	changes := 0
	l := NewLogRing(3)
	l.Changed = func() { changes++ }
	for _, w := range []string{"one\ntw", "o\n", "progress 1%\rprogress 2%\r", "three\nfour\nfive  \n", "partial"} {
		l.Write([]byte(w))
	}
	lines, status := l.Lines()
	if want := []string{"three", "four", "five"}; !slices.Equal(lines, want) {
		t.Errorf("lines = %q, want %q (the last 3, trailing spaces trimmed)", lines, want)
	}
	if status != "" {
		t.Errorf("status = %q, want none: a full line replaces it", status)
	}
	if changes != 4 {
		t.Errorf("Changed called %d times, want 4 (once per write that added or replaced a line)", changes)
	}
	l.Write([]byte("\rencoding 50%\r"))
	if _, status := l.Lines(); status != "encoding 50%" {
		t.Errorf("status = %q, want the latest status line", status)
	}
}

// fake is a Backend whose samples are the files in a config's samples
// directory. Each Run waits for a value on release.
type fake struct {
	release chan struct{}
	mu      sync.Mutex
	done    map[[2]string]bool
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
			out = append(out, bench.PairStatus{Sample: e.Name(), Flagset: fs, Done: f.done[[2]string{e.Name(), fs}]})
		}
	}
	return out, nil
}

func (f *fake) Run(ctx context.Context, _ *bench.Config, sample, flagset string, observe func(bench.Event)) error {
	observe(bench.Event{Kind: bench.Encoding, Round: 1, Source: sample, Sample: sample, Flagset: flagset, QualityFlag: "-crf", Quality: 25})
	select {
	case <-f.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	observe(bench.Event{Kind: bench.Finished, Source: sample, Sample: sample, Flagset: flagset, QualityFlag: "-crf", Quality: 25, VMAF: 95.5, Kbps: 4000, Pass: true, Outcome: bench.Found})
	f.mu.Lock()
	f.done[[2]string{sample, flagset}] = true
	f.mu.Unlock()
	return nil
}

func (f *fake) Busy(context.Context) string                         { return "" }
func (f *fake) Report(context.Context, *bench.Config, string) error { return nil }

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// start runs a queue of one work file, flagsets x and y on samples
// a.mov and b.mov, until its first unit is running, and serves it.
func start(t *testing.T) (*Server, *httptest.Server, *fake, chan error) {
	t.Helper()
	dir := t.TempDir()
	for _, s := range []string{"a.mov", "b.mov"} {
		write(t, filepath.Join(dir, "samples", s), "video")
	}
	work := filepath.Join(dir, "work")
	write(t, filepath.Join(work, "10-test.yaml"),
		"samples: ../samples\nworkdir: ../bench\nflagsets:\n  x: {args: [-c:v, libx265, -preset, fast]}\n  y: {args: [-c:v, libx265, -preset, slow]}\n")
	b := &fake{release: make(chan struct{}), done: map[[2]string]bool{}}
	r := &benchqueue.Runner{Dir: work, Backend: b, Poll: time.Millisecond}
	srv := &Server{Runner: r, Title: "encbench queue test", Log: NewLogRing(10)}
	r.Changed = srv.Notify
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		hs.Close()
		cancel()
		<-done
	})
	waitFor(t, "the first unit to start", func() bool { return r.Snapshot().Current != nil })
	return srv, hs, b, done
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func getState(t *testing.T, url string) State {
	t.Helper()
	resp, err := http.Get(url + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s State
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func post(t *testing.T, url string) State {
	t.Helper()
	resp, err := http.Post(url, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: %s", url, resp.Status)
	}
	var s State
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestState(t *testing.T) {
	srv, hs, b, done := start(t)
	srv.Log.Write([]byte("INFO Encoding\n"))

	s := getState(t, hs.URL)
	if s.Title != "encbench queue test" || s.Phase != "running" || s.Done != 0 || s.Total != 4 {
		t.Errorf("title %q, phase %q, %d/%d; want encbench queue test, running, 0/4", s.Title, s.Phase, s.Done, s.Total)
	}
	if c := s.Current; c == nil || c.Flagset != "x" || c.Sample != "a.mov" || c.File != "10-test.yaml" || c.Round != 1 {
		t.Errorf("current = %+v, want x on a.mov from 10-test.yaml, round 1", c)
	}
	if len(s.Files) != 1 || !s.Files[0].Running || s.Files[0].Remaining != 4 || s.Files[0].Total != 4 {
		t.Errorf("files = %+v, want 10-test.yaml running with 4 of 4 left", s.Files)
	}
	var next []string
	for _, u := range s.Next {
		next = append(next, u.Flagset+" "+u.Sample)
	}
	if want := []string{"x b.mov", "y a.mov", "y b.mov"}; !slices.Equal(next, want) || s.NextTotal != 3 {
		t.Errorf("next = %q (of %d), want %q: the units after the current one", next, s.NextTotal, want)
	}
	if !slices.Equal(s.Log, []string{"INFO Encoding"}) {
		t.Errorf("log = %q", s.Log)
	}

	// Pause takes effect after the unit; it shows as pausing until then.
	if s := post(t, hs.URL+"/api/pause"); !s.Pausing || s.Phase != "running" {
		t.Errorf("after pause: pausing %v, phase %q; want pausing while the unit runs", s.Pausing, s.Phase)
	}
	b.release <- struct{}{}
	waitFor(t, "the queue to pause", func() bool { return srv.Runner.Snapshot().Phase == benchqueue.Paused })
	s = getState(t, hs.URL)
	if s.Current != nil || s.Done != 1 || len(s.Recent) != 1 || s.Recent[0].Flagset != "x" || len(s.Recent[0].Answers) == 0 {
		t.Errorf("paused state: current %+v, done %d, recent %+v; want none running, 1 done, x's answer in recent", s.Current, s.Done, s.Recent)
	}

	// Resume runs the rest.
	if s := post(t, hs.URL+"/api/resume"); s.Pausing {
		t.Error("after resume: still pausing")
	}
	for range 3 {
		b.release <- struct{}{}
	}
	if err := <-done; err != nil {
		t.Fatalf("Run() = %v", err)
	}
	done <- nil // for the cleanup
	if s := getState(t, hs.URL); s.Phase != "finished" || s.Done != 4 || len(s.Recent) != 4 {
		t.Errorf("finished state: phase %q, done %d, %d results; want finished, 4, 4", s.Phase, s.Done, len(s.Recent))
	}
}

func TestEvents(t *testing.T) {
	_, hs, _, _ := start(t)
	resp, err := http.Get(hs.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	events := make(chan State)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var s State
				if json.Unmarshal([]byte(data), &s) == nil {
					events <- s
				}
			}
		}
	}()
	next := func() State {
		t.Helper()
		select {
		case s := <-events:
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
			return State{}
		}
	}
	if s := next(); s.Phase != "running" || s.Pausing {
		t.Errorf("first event: phase %q, pausing %v; want the state on connecting", s.Phase, s.Pausing)
	}
	post(t, hs.URL+"/api/pause")
	for s := next(); !s.Pausing; s = next() {
	}
}

func TestPageAndMethods(t *testing.T) {
	_, hs, _, _ := start(t)
	resp, err := http.Get(hs.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 512)
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(body[:n]), "<title>encbench queue</title>") {
		t.Errorf("GET /: %s %q", resp.Status, resp.Header.Get("Content-Type"))
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/pause"},
		{"GET", "/api/resume"},
		{"POST", "/api/state"},
	} {
		req, _ := http.NewRequest(c.method, hs.URL+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %s, want 405", c.method, c.path, resp.Status)
		}
	}
	if resp, _ := http.Get(hs.URL + "/nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /nope: %s, want 404", resp.Status)
	}
}

func getResults(t *testing.T, url string) Results {
	t.Helper()
	resp, err := http.Get(url + "/api/results")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/results: %s", resp.Status)
	}
	var res Results
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestResults(t *testing.T) {
	srv, hs, _, _ := start(t)
	logPath := filepath.Join(filepath.Dir(srv.Runner.Dir), "bench", "results.csv")

	got := getResults(t, hs.URL)
	if got.Format != ResultsFormat || len(got.Rows) != 0 || !slices.Equal(got.Logs, []string{logPath}) {
		t.Errorf("before any encode: format %d, %d rows, logs %q; want format %d, no rows, logs [%q]",
			got.Format, len(got.Rows), got.Logs, ResultsFormat, logPath)
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	log, err := bench.LoadResults(logPath)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 11, 12, 1, 2, 3, 0, time.UTC)
	for _, q := range []float64{24, 26.5} {
		m := bench.Metrics{VMAF: 95.5, VMAFNeg: math.NaN(), ColorShiftP99: math.NaN(), ColorShiftP999: math.NaN(),
			ColorFlickerP99: math.NaN(), ColorFlickerP999: math.NaN(), CAMBI: 1.5, CAMBIFR: math.NaN(), CAMBIFRMax: math.NaN()}
		if err := log.Append(bench.Row{Sample: "a.mov", Reference: "a.mov", Encoder: "libx265", Flagset: "x",
			Args: "-c:v libx265 -preset fast", QualityFlag: "-crf", Quality: q, Key: fmt.Sprint("k", q),
			Bytes: 1000, Kbps: 800, WallS: 2.5, Metrics: m, Scorer: "v6", Host: "h", Time: when, Timed: true,
			Width: int64(q * 80), Job: "10-test.yaml"}); err != nil {
			t.Fatal(err)
		}
	}
	// A row partway through being appended.
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("b.mov,libx265,y,")
	f.Close()

	got = getResults(t, hs.URL)
	if len(got.Rows) != 2 {
		t.Fatalf("got %d rows, want 2 (the partial row left out)", len(got.Rows))
	}
	r := got.Rows[1]
	if r.Log != 0 || r.Sample != "a.mov" || r.Flagset != "x" || r.Quality != 26.5 || r.Bytes != 1000 ||
		r.WallS != 2.5 || !r.Timed || !r.Time.Equal(when) {
		t.Errorf("row = %+v, want a.mov/x at 26.5 as appended", r)
	}
	if r.VMAF == nil || *r.VMAF != 95.5 || r.CAMBI == nil || *r.CAMBI != 1.5 {
		t.Errorf("vmaf %v, cambi %v; want 95.5 and 1.5", r.VMAF, r.CAMBI)
	}
	if r.Width == nil || *r.Width != 2120 || r.Height != nil || r.Pass != nil || r.Job != "10-test.yaml" {
		t.Errorf("width %v height %v pass %v job %q; want 2120, null, null, 10-test.yaml", r.Width, r.Height, r.Pass, r.Job)
	}
	if r.VMAFNeg != nil || r.CAMBIFR != nil {
		t.Errorf("vmaf_neg %v, cambi_fr %v; want null for NaN", r.VMAFNeg, r.CAMBIFR)
	}
}
