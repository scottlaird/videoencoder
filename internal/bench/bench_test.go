package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scottlaird/videoencoder/internal/probe"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
		check   func(t *testing.T, c *Config)
	}{
		{
			name: "defaults filled in",
			yaml: `
samples: clips
flagsets:
  x265:
    args: [-c:v, libx265, -preset, slow]
  vt:
    args: [-c:v, hevc_videotoolbox]
  custom:
    args: [-c:v, libvvenc]
    quality: {flag: -qp, min: 20, max: 40, step: 1}
`,
			check: func(t *testing.T, c *Config) {
				if c.TargetVMAF != 95 || c.VMAFJobs != 4 || !strings.HasSuffix(c.Workdir, "/bench") {
					t.Errorf("defaults not applied: %+v", c)
				}
				if q := c.Flagsets["x265"].Quality; q.Flag != "-crf" || q.Step != 0.1 || *q.HigherIsBetter {
					t.Errorf("x265 quality = %+v", q)
				}
				if q := c.Flagsets["vt"].Quality; q.Flag != "-q:v" || !*q.HigherIsBetter {
					t.Errorf("videotoolbox quality = %+v", q)
				}
				if q := c.Flagsets["custom"].Quality; q.Flag != "-qp" || *q.HigherIsBetter {
					t.Errorf("custom quality = %+v", q)
				}
				if got := c.FlagsetNames(); !slices.Equal(got, []string{"custom", "vt", "x265"}) {
					t.Errorf("FlagsetNames() = %v", got)
				}
			},
		},
		{name: "unknown key", yaml: "samples: s\ntarget_vmf: 95\nflagsets: {a: {args: [-c:v, libx265]}}", wantErr: "target_vmf"},
		{name: "no samples", yaml: "flagsets: {a: {args: [-c:v, libx265]}}", wantErr: "samples"},
		{name: "unknown encoder", yaml: "samples: s\nflagsets: {a: {args: [-c:v, mystery]}}", wantErr: "quality.flag"},
		{name: "quality flag in args", yaml: "samples: s\nflagsets: {a: {args: [-c:v, libx265, -crf, '20']}}", wantErr: "must not contain"},
		{name: "empty range", yaml: "samples: s\nflagsets: {a: {args: [-c:v, libx265], quality: {min: 30, max: 20}}}", wantErr: "quality.max"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bench.yaml")
			writeFile(t, path, tt.yaml)
			c, err := LoadConfig(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadConfig() error = %v, want one mentioning %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig() = %v", err)
			}
			tt.check(t, c)
		})
	}
}

func TestParseVMAFLog(t *testing.T) {
	log := `{
	  "frames": [
	    {"frameNum": 0, "metrics": {"vmaf": 97.0}},
	    {"frameNum": 1, "metrics": {"vmaf": 90.0}},
	    {"frameNum": 2, "metrics": {"vmaf": 99.0}}
	  ],
	  "pooled_metrics": {
	    "vmaf": {"min": 90, "max": 99, "mean": 95.33, "harmonic_mean": 95.2},
	    "vmaf_neg": {"mean": 94.1},
	    "psnr_y": {"mean": 44.5},
	    "psnr_cb": {"mean": 47.25},
	    "psnr_cr": {"mean": 46.5}
	  }
	}`
	got, err := parseVMAFLog([]byte(log))
	if err != nil {
		t.Fatal(err)
	}
	want := Metrics{VMAF: 95.33, VMAFHMean: 95.2, VMAFP1: 90, VMAFNeg: 94.1, PSNRY: 44.5, PSNRCb: 47.25, PSNRCr: 46.5}
	if got != want {
		t.Errorf("parseVMAFLog() =\n %+v\nwant\n %+v", got, want)
	}

	if _, err := parseVMAFLog([]byte(`{"frames": [], "pooled_metrics": {}}`)); err == nil {
		t.Error("parseVMAFLog(empty) = nil error, want one")
	}
}

func TestParseColourLog(t *testing.T) {
	tests := []struct {
		log     string
		want    float64
		wantErr bool
	}{
		{log: `{"pooled_metrics": {"ciede2000": {"mean": 43.25}}}`, want: 43.25},
		{log: `{"pooled_metrics": {"ciede2000": {"mean": null}}}`, want: math.Inf(1)},
		{log: `{"pooled_metrics": {"vmaf": {"mean": 99}}}`, wantErr: true},
		{log: `not json`, wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseColourLog([]byte(tt.log))
		if (err != nil) != tt.wantErr || (!tt.wantErr && got != tt.want) {
			t.Errorf("parseColourLog(%s) = %v, %v; want %v, err %v", tt.log, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestScoreArgvEscaping(t *testing.T) {
	argv := scoreArgv("d.mp4", "r.mov", 3840, 2160, 8, "/tmp/a:b/log.json", "/tmp/c.json")
	graph := argv[slices.Index(argv, "-filter_complex")+1]
	for _, want := range []string{
		`log_path=/tmp/a\:b/log.json`, "log_path=/tmp/c.json", "version=vmaf_4k_v0.6.1\\:name=vmaf|",
		"scale=3840:2160", "scale=1920:1080:flags=area", "feature='name=ciede'",
	} {
		if !strings.Contains(graph, want) {
			t.Errorf("filtergraph %q lacks %q", graph, want)
		}
	}
}

func TestResultsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.csv")
	r, err := LoadResults(path)
	if err != nil || len(r.Rows) != 0 {
		t.Fatalf("LoadResults(missing) = %v, %v", r, err)
	}
	row := Row{
		Sample: "a.mov", Encoder: "libx265", Flagset: "slow", Args: "-c:v libx265 -preset slow",
		QualityFlag: "-crf", Quality: 18.7, Key: "k1", FlagsetKey: "f1", Bytes: 1234, Kbps: 329.1,
		WallS: 1.5, UserS: 9.25, SysS: 0.5, MaxRSSKB: 2048,
		Metrics:       Metrics{VMAF: 95.01, VMAFHMean: 94.9, VMAFP1: 91, VMAFNeg: 93.6, PSNRY: 44, PSNRCb: 48, PSNRCr: 47, CIEDE2000: math.Inf(1)},
		FFmpegVersion: "8.1", Host: "h", Time: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
	}
	for _, k := range []string{"k1", "k2"} {
		row.Key = k
		if err := r.Append(row); err != nil {
			t.Fatal(err)
		}
	}

	again, err := LoadResults(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Rows) != 2 {
		t.Fatalf("reloaded %d rows, want 2", len(again.Rows))
	}
	got, ok := again.Lookup("k2")
	if !ok || got != row {
		t.Errorf("Lookup(k2) = %+v, %v\nwant %+v", got, ok, row)
	}
}

// fakeFFmpeg pretends to encode and score. An encode at quality q
// produces q-dependent file sizes; scoring reports a VMAF that falls
// linearly with q, so with a 95 target the answer is q = cutoff.
type fakeFFmpeg struct {
	mu      sync.Mutex // scoring runs in parallel
	cutoff  float64
	ciede   float64
	quality map[string]float64 // encoded file -> quality
	encodes []string           // "sample q" per encode
	scores  int
}

func newFake(cutoff float64) *fakeFFmpeg {
	return &fakeFFmpeg{cutoff: cutoff, ciede: 60, quality: map[string]float64{}}
}

func (f *fakeFFmpeg) Exec(_ context.Context, argv []string) (Measurement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := slices.Index(argv, "-filter_complex"); i >= 0 {
		f.scores++
		var logs []string
		for _, part := range strings.Split(argv[i+1], "log_path=")[1:] {
			logs = append(logs, strings.SplitN(part, ":n_threads", 2)[0])
		}
		q := f.quality[argv[slices.Index(argv, "-i")+1]]
		vmaf := 95 + (f.cutoff-q)*0.5
		log := fmt.Sprintf(`{"frames":[{"metrics":{"vmaf":%v}}],"pooled_metrics":{"vmaf":{"mean":%v,"harmonic_mean":%v},"vmaf_neg":{"mean":%v},"psnr_y":{"mean":45},"psnr_cb":{"mean":48},"psnr_cr":{"mean":48}}}`,
			vmaf, vmaf, vmaf, vmaf-1)
		colour := fmt.Sprintf(`{"pooled_metrics":{"ciede2000":{"mean":%v}}}`, f.ciede)
		return Measurement{Wall: time.Second}, errors.Join(
			os.WriteFile(logs[0], []byte(log), 0o644),
			os.WriteFile(logs[1], []byte(colour), 0o644))
	}
	if slices.Contains(argv, "libbroken") {
		return Measurement{}, errors.New("Unknown encoder 'libbroken'")
	}
	out := argv[len(argv)-1]
	q, err := strconv.ParseFloat(argv[len(argv)-3], 64)
	if err != nil {
		return Measurement{}, err
	}
	f.quality[out] = q
	f.encodes = append(f.encodes, fmt.Sprintf("%s %v", filepath.Base(argv[slices.Index(argv, "-i")+1]), q))
	return Measurement{Wall: 2 * time.Second, User: 10 * time.Second}, os.WriteFile(out, bytes.Repeat([]byte("x"), int(10000/q)), 0o644)
}

type fakeProber struct{}

func (fakeProber) Probe(context.Context, string) (probe.VideoData, error) {
	return probe.VideoData{Width: 1920, Height: 1080, FPS: 30, Duration: 30 * time.Second}, nil
}

func benchConfig(t *testing.T, dir, flagsets string) *Config {
	t.Helper()
	path := filepath.Join(dir, "bench.yaml")
	writeFile(t, path, "samples: samples\nworkdir: work\nflagsets:\n"+flagsets)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const twoFlagsets = `
  fast: {args: [-c:v, libx265, -preset, fast]}
  slow: {args: [-c:v, libx265, -preset, slow]}
`

func TestBenchRun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "samples", "a.mov"), "sample a")
	writeFile(t, filepath.Join(dir, "samples", "notes.txt"), "not a video")
	ctx := context.Background()

	fake := newFake(18.7)
	b := &Bench{Cfg: benchConfig(t, dir, twoFlagsets), Exec: fake, Prober: fakeProber{}}
	if err := b.Run(ctx); err != nil {
		t.Fatalf("first run: %v", err)
	}
	firstEncodes := len(fake.encodes)
	if firstEncodes == 0 || firstEncodes > 20 {
		t.Errorf("first run: %d encodes, want 1..20", firstEncodes)
	}
	if fake.scores != firstEncodes {
		t.Errorf("first run: %d scores for %d encodes", fake.scores, firstEncodes)
	}

	results, err := LoadResults(filepath.Join(dir, "work", "results.csv"))
	if err != nil {
		t.Fatal(err)
	}
	bests := Bests(b.Cfg, results)
	if len(bests) != 2 {
		t.Fatalf("got %d answers, want 2", len(bests))
	}
	for _, best := range bests {
		if best.Quality != 18.7 || best.Outcome != Found {
			t.Errorf("%s: answer %v (%v), want 18.7 found", best.Name, best.Quality, best.Outcome)
		}
		keeper := filepath.Join(dir, "work", "keep", "a", best.Name+".q18.7.mp4")
		if _, err := os.Stat(keeper); err != nil {
			t.Errorf("keeper missing: %v", err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "work", "tmp", "*")); len(left) != 0 {
		t.Errorf("scratch files left behind: %q", left)
	}
	best, err := os.ReadFile(filepath.Join(dir, "work", "best.csv"))
	if err != nil || !strings.Contains(string(best), "a.mov,fast,libx265") {
		t.Errorf("best.csv = %q, %v", best, err)
	}

	// Nothing new: nothing is encoded.
	fake.encodes = nil
	if err := b.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.encodes) != 0 {
		t.Errorf("rerun encoded %q, want nothing", fake.encodes)
	}

	// Renaming a flagset reuses its results.
	b.Cfg = benchConfig(t, dir, `
  quick: {args: [-c:v, libx265, -preset, fast]}
  slow: {args: [-c:v, libx265, -preset, slow]}
`)
	if err := b.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.encodes) != 0 {
		t.Errorf("rename encoded %q, want nothing", fake.encodes)
	}

	// A new sample runs every flagset on that sample only, seeded from
	// the first sample's answers, so it needs fewer encodes.
	writeFile(t, filepath.Join(dir, "samples", "b.mov"), "sample b")
	if err := b.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, e := range fake.encodes {
		if !strings.HasPrefix(e, "b.mov ") {
			t.Errorf("new sample run encoded %q", e)
		}
	}
	if got := len(fake.encodes); got == 0 || got >= firstEncodes {
		t.Errorf("new sample took %d encodes; want fewer than the unseeded %d", got, firstEncodes)
	}

	// A new flagset runs on every sample, and nothing else does.
	fake.encodes = nil
	b.Cfg = benchConfig(t, dir, twoFlagsets+"  medium: {args: [-c:v, libx265, -preset, medium]}\n")
	if err := b.Run(ctx); err != nil {
		t.Fatal(err)
	}
	samples := map[string]bool{}
	for _, e := range fake.encodes {
		samples[strings.Fields(e)[0]] = true
	}
	if !samples["a.mov"] || !samples["b.mov"] {
		t.Errorf("new flagset encoded on %v, want both samples", samples)
	}
}

func TestBenchColourLimit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "samples", "a.mov"), "sample a")
	c := benchConfig(t, dir, "  x: {args: [-c:v, libx265]}\n")
	c.MinCIEDE2000 = 50

	fake := newFake(18.7)
	fake.ciede = 45 // a colour cast at every quality: nothing passes
	b := &Bench{Cfg: c, Exec: fake, Prober: fakeProber{}}
	if err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	results, _ := LoadResults(filepath.Join(dir, "work", "results.csv"))
	if bests := Bests(c, results); len(bests) != 0 {
		t.Errorf("got answers %+v despite the colour limit", bests)
	}
}

func TestBenchBrokenFlagset(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "samples", "a.mov"), "sample a")
	c := benchConfig(t, dir, `
  broken: {args: [-c:v, libbroken], quality: {flag: -crf, min: 0, max: 50, step: 1}}
  fine: {args: [-c:v, libx265]}
`)
	fake := newFake(18.7)
	b := &Bench{Cfg: c, Exec: fake, Prober: fakeProber{}}
	if err := b.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v; a broken flagset should not stop the others", err)
	}
	results, _ := LoadResults(filepath.Join(dir, "work", "results.csv"))
	if bests := Bests(c, results); len(bests) != 1 || bests[0].Name != "fine" {
		t.Errorf("answers = %+v, want just fine", bests)
	}
}

func TestBenchDryRun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "samples", "a.mov"), "sample a")
	fake := newFake(18.7)
	b := &Bench{Cfg: benchConfig(t, dir, twoFlagsets), Exec: fake, Prober: fakeProber{}, DryRun: true}
	if err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.encodes) != 0 || fake.scores != 0 {
		t.Errorf("dry run ran %d encodes and %d scores", len(fake.encodes), fake.scores)
	}
}
