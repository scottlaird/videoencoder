package score

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/scottlaird/videoencoder/internal/benchweb"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}

func shaOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func ptr[T any](v T) *T { return &v }

// queue is a fake encbench queue web view.
type queue struct {
	rows    []benchweb.ResultRow
	encodes map[string]string // sha -> content
	samples map[string]string // sample id -> content
	refs    map[string]string // sample id -> reference content
	corrupt bool              // serve wrong bytes for encodes

	mu      sync.Mutex
	fetches map[string]int
}

func (q *queue) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/results", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(benchweb.Results{Format: benchweb.ResultsFormat, Rows: q.rows})
	})
	mux.HandleFunc("GET /api/files/index", func(w http.ResponseWriter, _ *http.Request) {
		idx := benchweb.FileIndex{Format: benchweb.FilesFormat}
		for sha, c := range q.encodes {
			idx.Encodes = append(idx.Encodes, benchweb.EncodeFile{SHA256: sha, Bytes: int64(len(c)), Kept: "object"})
		}
		for id, c := range q.samples {
			idx.Samples = append(idx.Samples, benchweb.SampleEntry{SampleID: id, Name: id + ".mov", Bytes: int64(len(c)), ReferenceBytes: int64(len(q.refs[id]))})
		}
		json.NewEncoder(w).Encode(idx)
	})
	serve := func(kind string, files map[string]string) {
		mux.HandleFunc("GET /api/files/"+kind+"/{id}", func(w http.ResponseWriter, r *http.Request) {
			c, ok := files[r.PathValue("id")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			q.mu.Lock()
			q.fetches[kind+"/"+r.PathValue("id")]++
			q.mu.Unlock()
			if kind == "encode" && q.corrupt {
				c = strings.ToUpper(c)
			}
			w.Header().Set("Content-Disposition", `inline; filename="x.mov"`)
			fmt.Fprint(w, c)
		})
	}
	serve("encode", q.encodes)
	serve("sample", q.samples)
	serve("reference", q.refs)
	return mux
}

// fakeCVVDP scores a test as its length over 10 below 10 JOD, as
// cvvdp's --result file would, and records each run.
type fakeCVVDP struct {
	mu   sync.Mutex
	runs [][]string
	fail error
	// needCC fails runs without --ffmpeg-cc, as cvvdp does for a
	// pixel format its GPU color conversion can't handle.
	needCC bool
}

func (f *fakeCVVDP) Run(_ context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	f.runs = append(f.runs, argv)
	f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	if f.needCC && !slices.Contains(argv, "--ffmpeg-cc") {
		return nil, errors.New("cvvdp: GPU-accelerated decoding cannot handle chroma subsampling j42. Run with `--ffmpeg-cc` command-line argument.")
	}
	res := argv[slices.Index(argv, "--result")+1]
	ref := argv[slices.Index(argv, "--ref")+1]
	tests := argv[slices.Index(argv, "--test")+1:]
	out := "test, reference, cvvdp\n"
	for _, t := range tests {
		data, err := os.ReadFile(t)
		if err != nil {
			return nil, err
		}
		out += fmt.Sprintf("%s, %s, %v\n", t, ref, 10-float64(len(data))/10)
	}
	return nil, os.WriteFile(res, []byte(out), 0o644)
}

func row(sample, id, flagset string, q float64, content string, height int64) benchweb.ResultRow {
	return benchweb.ResultRow{Sample: sample, SampleID: id, SourceID: "src" + id[3:], Flagset: flagset, Encoder: "libx265",
		Quality: q, OutSHA256: shaOf(content), Height: ptr(height)}
}

func writeJob(t *testing.T, dir, name, text string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func newRunner(t *testing.T, ex Executor, jobPaths ...string) *Runner {
	t.Helper()
	dir := t.TempDir()
	log, err := OpenLog(filepath.Join(dir, "scores.csv"))
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "tmp"), 0o755)
	return &Runner{
		Jobs: func() ([]*Job, error) {
			var js []*Job
			for _, p := range jobPaths {
				j, err := LoadJob(p)
				if err != nil {
					return nil, err
				}
				js = append(js, j)
			}
			return js, nil
		},
		Log:   log,
		Cache: &Cache{Dir: filepath.Join(dir, "cache")},
		CVVDP: &CVVDP{Exec: ex, Version: "0.5.7"},
		Host:  "test",
		Tmp:   filepath.Join(dir, "tmp"),
		Batch: 2,
	}
}

func TestRun(t *testing.T) {
	a := &queue{
		rows: []benchweb.ResultRow{
			row("a@1080p.mov", "000000000000000a", "x", 20, "enc one", 1080),
			row("a@1080p.mov", "000000000000000a", "x", 22, "enc two!", 1080),
			row("a@1080p.mov", "000000000000000a", "y", 20, "enc three", 1080),
			row("a@1080p.mov", "000000000000000a", "x", 24, "not kept", 1080), // no file
			row("b@2160p.mov", "000000000000000b", "x", 20, "four k", 2160),   // not selected
		},
		encodes: map[string]string{shaOf("enc one"): "enc one", shaOf("enc two!"): "enc two!", shaOf("enc three"): "enc three", shaOf("four k"): "four k"},
		samples: map[string]string{"000000000000000a": "sample a", "000000000000000b": "sample b"},
		refs:    map[string]string{"000000000000000a": "reference a"},
		fetches: map[string]int{},
	}
	// The second queue made one of the same encodes, of the same sample.
	b := &queue{
		rows:    []benchweb.ResultRow{row("a@1080p.mov", "000000000000000a", "x-copy", 20, "enc one", 1080)},
		encodes: map[string]string{shaOf("enc one"): "enc one"},
		samples: map[string]string{"000000000000000a": "sample a"},
		refs:    map[string]string{"000000000000000a": "reference a"},
		fetches: map[string]int{},
	}
	sa, sb := httptest.NewServer(a.handler()), httptest.NewServer(b.handler())
	defer sa.Close()
	defer sb.Close()
	dir := t.TempDir()
	job := writeJob(t, dir, "fhd.yaml", fmt.Sprintf("hosts: [%s, %s]\nselect: {heights: [1080]}\nscorers:\n  - {metric: cvvdp, display: standard_fhd, gpu_mem: 6}\n", sa.URL, sb.URL))
	ex := &fakeCVVDP{}
	r := newRunner(t, ex, job)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	scores := r.Log.Rows()
	if len(scores) != 3 {
		t.Fatalf("got %d scores, want 3 (each kept 1080p encode once): %+v", len(scores), scores)
	}
	for _, s := range scores {
		if s.Scorer != "cvvdp 0.5.7 standard_fhd" || s.Metric != "jod" || s.SampleID != "000000000000000a" || s.Host != "test" {
			t.Errorf("score = %+v", s)
		}
		if want := Key(s.Scorer, s.SampleID, s.OutSHA256); s.Key != want {
			t.Errorf("key %s, want %s", s.Key, want)
		}
	}
	byContent := map[string]float64{}
	for sha, c := range a.encodes {
		byContent[sha] = 10 - float64(len(c))/10
	}
	for _, s := range scores {
		if s.Value != byContent[s.OutSHA256] {
			t.Errorf("%s scored %v, want %v", s.Flagset, s.Value, byContent[s.OutSHA256])
		}
	}
	if len(ex.runs) != 2 {
		t.Errorf("%d cvvdp runs, want 2 (3 encodes, batches of 2)", len(ex.runs))
	}
	for _, argv := range ex.runs {
		ref := argv[slices.Index(argv, "--ref")+1]
		if data, _ := os.ReadFile(ref); string(data) != "reference a" || !slices.Contains(argv, "--gpu-mem") {
			t.Errorf("cvvdp %q: scored against %q, want the reference, with --gpu-mem", argv, data)
		}
	}
	if a.fetches["reference/000000000000000a"] != 1 || b.fetches["encode/"+shaOf("enc one")] != 0 {
		t.Errorf("fetches a %v b %v, want the reference once and nothing from b", a.fetches, b.fetches)
	}

	// Again: nothing is left.
	ex.runs = nil
	r2 := newRunner(t, ex, job)
	r2.Log, _ = OpenLog(r.Log.path)
	r2.Cache = r.Cache
	if err := r2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ex.runs) != 0 || len(r2.Log.Rows()) != 3 {
		t.Errorf("a second run ran cvvdp %d times and logged %d scores; want none and 3", len(ex.runs), len(r2.Log.Rows()))
	}
}

func TestRunGivesUp(t *testing.T) {
	a := &queue{
		rows:    []benchweb.ResultRow{row("a.mov", "000000000000000a", "x", 20, "enc", 1080)},
		encodes: map[string]string{shaOf("enc"): "enc"},
		samples: map[string]string{"000000000000000a": "sample"},
		fetches: map[string]int{},
	}
	s := httptest.NewServer(a.handler())
	defer s.Close()
	job := writeJob(t, t.TempDir(), "j.yaml", "hosts: ["+s.URL+"]\nscorers: [{metric: cvvdp, display: standard_fhd}]\n")
	ex := &fakeCVVDP{fail: errors.New("out of memory")}
	r := newRunner(t, ex, job)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ex.runs) != maxFailures || r.Snapshot().Failed != maxFailures || len(r.Log.Rows()) != 0 {
		t.Errorf("%d runs, %d failed, %d scores; want %d tries, then giving up", len(ex.runs), r.Snapshot().Failed, len(r.Log.Rows()), maxFailures)
	}
	ref := ex.runs[0][slices.Index(ex.runs[0], "--ref")+1]
	if data, _ := os.ReadFile(ref); string(data) != "sample" {
		t.Errorf("scored against %q, want the sample itself (it has no reference)", data)
	}
}

func TestRunRetriesWithFFmpegCC(t *testing.T) {
	a := &queue{
		rows:    []benchweb.ResultRow{row("a.mp4", "000000000000000a", "x", 20, "enc", 2160)},
		encodes: map[string]string{shaOf("enc"): "enc"},
		samples: map[string]string{"000000000000000a": "sample"},
		fetches: map[string]int{},
	}
	s := httptest.NewServer(a.handler())
	defer s.Close()
	job := writeJob(t, t.TempDir(), "j.yaml", "hosts: ["+s.URL+"]\nscorers: [{metric: cvvdp, display: standard_fhd}]\n")
	ex := &fakeCVVDP{needCC: true}
	r := newRunner(t, ex, job)
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(r.Log.Rows()); got != 1 || r.Snapshot().Failed != 0 {
		t.Fatalf("%d scores, %d failed; want the encode scored on the retry", got, r.Snapshot().Failed)
	}
	if len(ex.runs) != 2 || !slices.Contains(ex.runs[1], "--ffmpeg-cc") {
		t.Errorf("runs = %q, want one without --ffmpeg-cc, then one with it", ex.runs)
	}
}

func TestCommandSeesLoggedErrors(t *testing.T) {
	_, err := Command{}.Run(context.Background(), []string{"sh", "-c", "echo '[INFO] fine' >&2; echo '[ERROR] cannot handle j42. Run with `--ffmpeg-cc`.' >&2"})
	if err == nil || !strings.Contains(err.Error(), "--ffmpeg-cc") {
		t.Errorf("err = %v, want the [ERROR] line though it exited 0", err)
	}
	if _, err := (Command{}).Run(context.Background(), []string{"sh", "-c", "echo '[INFO] fine' >&2"}); err != nil {
		t.Errorf("err = %v, want none", err)
	}
}

func TestCacheRejectsBadFiles(t *testing.T) {
	a := &queue{encodes: map[string]string{shaOf("enc"): "enc"}, corrupt: true, fetches: map[string]int{}}
	s := httptest.NewServer(a.handler())
	defer s.Close()
	k := &Cache{Dir: t.TempDir()}
	if _, err := k.Encode(context.Background(), &Client{Base: s.URL}, shaOf("enc"), 3); err == nil {
		t.Error("accepted an encode whose SHA-256 doesn't match")
	}
	if m, _ := filepath.Glob(filepath.Join(k.Dir, "encodes", "*", "*")); len(m) != 0 {
		t.Errorf("left %q in the cache", m)
	}
}

func TestCacheEvicts(t *testing.T) {
	content := map[string]string{}
	for i := range 4 {
		c := strings.Repeat(fmt.Sprint(i), 10)
		content[shaOf(c)] = c
	}
	a := &queue{encodes: content, fetches: map[string]int{}}
	s := httptest.NewServer(a.handler())
	defer s.Close()
	k := &Cache{Dir: t.TempDir(), MaxBytes: 25}
	c := &Client{Base: s.URL}
	var held string
	for i := range 4 {
		sha := shaOf(strings.Repeat(fmt.Sprint(i), 10))
		p, err := k.Encode(context.Background(), c, sha, 10)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			held = p // stays in use
		} else {
			k.Release(p)
		}
	}
	m, _ := filepath.Glob(filepath.Join(k.Dir, "encodes", "*", "*.mp4"))
	if len(m) != 2 || !slices.Contains(m, held) {
		t.Errorf("cache holds %q, want 2 files (25 bytes) including the one in use", m)
	}
}

func TestParseCVVDPResult(t *testing.T) {
	tests := []string{"/c/a.mp4", "/c/b.mp4"}
	got, err := parseCVVDPResult([]byte("test, reference, cvvdp\n/c/a.mp4, /r.mov, 9.81\n/c/b.mp4, /r.mov, 9.5\n"), tests)
	if err != nil || !slices.Equal(got, []float64{9.81, 9.5}) {
		t.Errorf("got %v, %v", got, err)
	}
	for _, bad := range []string{
		"test, reference, cvvdp\n/c/a.mp4, /r.mov, 9.81\n",
		"test, reference, cvvdp\n/c/b.mp4, /r.mov, 9.81\n/c/a.mp4, /r.mov, 9.5\n",
		"test, reference, cvvdp\n/c/a.mp4, /r.mov, nan?\n/c/b.mp4, /r.mov, 9.5\n",
	} {
		if _, err := parseCVVDPResult([]byte(bad), tests); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
}

func TestLogReload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "scores.csv")
	l, _ := OpenLog(p)
	s := Score{Key: "k1", Scorer: "cvvdp 0.5.7 standard_fhd", Metric: "jod", Value: 9.7531, SampleID: "s", OutSHA256: "o", Quality: 21.5}
	if err := l.Append(s); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("k2,cvvdp") // partway through a line
	f.Close()
	l2, err := OpenLog(p)
	if err != nil {
		t.Fatal(err)
	}
	if rows := l2.Rows(); len(rows) != 1 || rows[0].Value != 9.7531 || rows[0].Quality != 21.5 || !l2.Has("k1") || l2.Has("k2") {
		t.Errorf("reloaded %+v", rows)
	}
}

func TestJobChecks(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{
		"scorers: [{metric: cvvdp, display: d}]\n",
		"hosts: [render3:8090]\nscorers: [{metric: cvvdp, display: d}]\n",
		"hosts: [http://h]\n",
		"hosts: [http://h]\nscorers: [{metric: vmaf, display: d}]\n",
		"hosts: [http://h]\nscorers: [{metric: cvvdp}]\n",
		"hosts: [http://h]\nscorers: [{metric: cvvdp, display: d, transfer: pq}]\n",
		"hosts: [http://h]\nscorers: [{metric: cvvdp, display: d, display_models: nope.json}]\n",
		"hosts: [http://h]\nscorers: [{metric: cvvdp, display: d}]\nunknown: 1\n",
	} {
		if _, err := LoadJob(writeJob(t, dir, "j.yaml", bad)); err == nil {
			t.Errorf("loaded %q", bad)
		}
	}
	writeJob(t, dir, "display_models.json", `{"m": {}}`)
	j, err := LoadJob(writeJob(t, dir, "j.yaml", "hosts: [http://h/]\nscorers: [{metric: cvvdp, display: m, display_models: display_models.json}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	c := &CVVDP{Version: "0.5.7"}
	name, err := c.ScorerName(&j.Scorers[0])
	if err != nil || !strings.HasPrefix(name, "cvvdp 0.5.7 m models=") || j.Hosts[0] != "http://h" {
		t.Errorf("scorer %q (%v), host %q", name, err, j.Hosts[0])
	}
}

func TestApplies(t *testing.T) {
	sdr := benchweb.ResultRow{Height: ptr(int64(1080)), ColorTransfer: "bt709"}
	hdr := benchweb.ResultRow{Height: ptr(int64(2160)), ColorTransfer: "smpte2084"}
	for _, c := range []struct {
		cfg      ScorerConfig
		sdr, hdr bool
	}{
		{ScorerConfig{}, true, true},
		{ScorerConfig{Transfer: "sdr"}, true, false},
		{ScorerConfig{Transfer: "hdr"}, false, true},
		{ScorerConfig{Heights: []int64{2160}}, false, true},
	} {
		if c.cfg.applies(&sdr) != c.sdr || c.cfg.applies(&hdr) != c.hdr {
			t.Errorf("%+v: applies to SDR %v, HDR %v; want %v, %v", c.cfg, c.cfg.applies(&sdr), c.cfg.applies(&hdr), c.sdr, c.hdr)
		}
	}
}
