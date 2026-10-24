package bench

import (
	"bytes"
	"context"
	"encoding/binary"
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

func TestParseColorLog(t *testing.T) {
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
		got, err := parseColorLog([]byte(tt.log))
		if (err != nil) != tt.wantErr || (!tt.wantErr && got != tt.want) {
			t.Errorf("parseColorLog(%s) = %v, %v; want %v, err %v", tt.log, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestScoreArgvEscaping(t *testing.T) {
	argv := scoreArgv("d.mp4", "r.mov", 3840, 2160, 8, "/tmp/a:b/log.json", "/tmp/c.json", "/tmp/d.blocks", "/tmp/r.blocks", "")
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

func TestScoreArgvPrefilter(t *testing.T) {
	argv := scoreArgv("d.mp4", "r.mov", 1920, 1080, 4, "/l.json", "/c.json", "/d.blocks", "/r.blocks", "tonemap=hable")
	graph := argv[slices.Index(argv, "-filter_complex")+1]
	if strings.Count(graph, "tonemap=hable,") != 2 {
		t.Errorf("prefilter isn't applied to both inputs: %q", graph)
	}
	if !strings.Contains(graph, "format=rgb48le,scale=120:67:flags=area") || !slices.Contains(argv, "/d.blocks") {
		t.Errorf("no block-average outputs: %q", argv)
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
	mu         sync.Mutex // scoring runs in parallel
	tint       uint16     // added to the red of every distorted block
	cutoff     float64
	ciede      float64
	quality    map[string]float64 // encoded file -> quality
	encodes    []string           // "sample q" per encode
	scores     int
	graphs     []string // scoring filtergraphs
	downscales int
}

func newFake(cutoff float64) *fakeFFmpeg {
	return &fakeFFmpeg{cutoff: cutoff, ciede: 60, quality: map[string]float64{}}
}

func (f *fakeFFmpeg) Exec(_ context.Context, argv []string) (Measurement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := slices.Index(argv, "-filter_complex"); i >= 0 {
		f.scores++
		f.graphs = append(f.graphs, argv[i+1])
		var logs []string
		for _, part := range strings.Split(argv[i+1], "log_path=")[1:] {
			logs = append(logs, strings.SplitN(part, ":n_threads", 2)[0])
		}
		q := f.quality[argv[slices.Index(argv, "-i")+1]]
		vmaf := 95 + (f.cutoff-q)*0.5
		log := fmt.Sprintf(`{"frames":[{"metrics":{"vmaf":%v}}],"pooled_metrics":{"vmaf":{"mean":%v,"harmonic_mean":%v},"vmaf_neg":{"mean":%v},"psnr_y":{"mean":45},"psnr_cb":{"mean":48},"psnr_cr":{"mean":48}}}`,
			vmaf, vmaf, vmaf, vmaf-1)
		color := fmt.Sprintf(`{"pooled_metrics":{"ciede2000":{"mean":%v}}}`, f.ciede)
		dist, ref := blockImages(argv, f.tint)
		return Measurement{Wall: time.Second}, errors.Join(
			os.WriteFile(logs[0], []byte(log), 0o644),
			os.WriteFile(logs[1], []byte(color), 0o644),
			os.WriteFile(argv[len(argv)-6], dist, 0o644),
			os.WriteFile(argv[len(argv)-1], ref, 0o644))
	}
	if slices.Contains(argv, "prores_ks") {
		f.downscales++
		return Measurement{}, os.WriteFile(argv[len(argv)-1], []byte("scaled"), 0o644)
	}
	if slices.Contains(argv, "libbroken") {
		return Measurement{}, errors.New("Unknown encoder 'libbroken'")
	}
	out := argv[len(argv)-1]
	qi := slices.IndexFunc(argv, func(a string) bool { return a == "-crf" || a == "-qp" })
	if qi < 0 {
		return Measurement{}, errors.New("no quality flag")
	}
	q, err := strconv.ParseFloat(argv[qi+1], 64)
	if err != nil {
		return Measurement{}, err
	}
	f.quality[out] = q
	if slices.Contains(argv, "libx265") && !slices.Contains(argv, "hvc1") {
		return Measurement{}, errors.New("HEVC encode not tagged hvc1")
	}
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

func TestBenchColorLimit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "samples", "a.mov"), "sample a")
	c := benchConfig(t, dir, "  x: {args: [-c:v, libx265]}\n")
	c.MinCIEDE2000 = 50

	fake := newFake(18.7)
	fake.ciede = 45 // a color cast at every quality: nothing passes
	b := &Bench{Cfg: c, Exec: fake, Prober: fakeProber{}}
	if err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	results, _ := LoadResults(filepath.Join(dir, "work", "results.csv"))
	if bests := Bests(c, results); len(bests) != 0 {
		t.Errorf("got answers %+v despite the color limit", bests)
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

// sizedProber reports 8K PQ for "big" files, their scaled heights for
// "@<h>p" copies, and 1080p SDR otherwise.
type sizedProber struct{}

func (sizedProber) Probe(_ context.Context, path string) (probe.VideoData, error) {
	vd := probe.VideoData{Width: 1920, Height: 1080, Duration: 30 * time.Second, ColorTransfer: "bt709"}
	base := filepath.Base(path)
	if strings.HasPrefix(base, "big") {
		vd.ColorTransfer = "smpte2084"
		vd.Width, vd.Height = 7680, 4320
		if strings.Contains(base, "@2160p") {
			vd.Width, vd.Height = 3840, 2160
		} else if strings.Contains(base, "@1080p") {
			vd.Width, vd.Height = 1920, 1080
		}
	}
	return vd, nil
}

func TestBenchScales(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "samples", "big.mov"), "8k hdr")
	writeFile(t, filepath.Join(dir, "samples", "hd.mov"), "1080p sdr")
	c := benchConfig(t, dir, "  x: {args: [-c:v, libx265]}\n")
	c.Scales = []uint64{1080, 2160}

	fake := newFake(18.7)
	b := &Bench{Cfg: c, Exec: fake, Prober: sizedProber{}}
	if err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	results, _ := LoadResults(filepath.Join(dir, "work", "results.csv"))
	var names []string
	for _, best := range Bests(c, results) {
		names = append(names, best.Sample)
	}
	slices.Sort(names)
	if want := []string{"big@1080p.mov", "big@2160p.mov", "hd.mov"}; !slices.Equal(names, want) {
		t.Errorf("samples tested = %q, want %q", names, want)
	}
	if fake.downscales != 2 {
		t.Errorf("made %d downscaled copies, want 2", fake.downscales)
	}
	toneMapped := 0
	for _, g := range fake.graphs {
		if strings.Contains(g, "tonemap=") {
			toneMapped++
		}
	}
	if toneMapped == 0 || toneMapped == len(fake.graphs) {
		t.Errorf("%d of %d scorings tone-mapped; want the HDR sample's only", toneMapped, len(fake.graphs))
	}

	// Copies are made once and reused, and keep their results.
	fake.encodes, fake.downscales = nil, 0
	if err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.downscales != 0 || len(fake.encodes) != 0 {
		t.Errorf("rerun made %d copies and %d encodes, want none", fake.downscales, len(fake.encodes))
	}
}

func TestContainerArgs(t *testing.T) {
	tests := []struct {
		args []string
		want []string
	}{
		{[]string{"-c:v", "libx265"}, []string{"-tag:v", "hvc1"}},
		{[]string{"-c:v", "hevc_videotoolbox"}, []string{"-tag:v", "hvc1"}},
		{[]string{"-c:v", "libx265", "-tag:v", "hev1"}, nil},
		{[]string{"-c:v", "libsvtav1"}, nil},
	}
	for _, tt := range tests {
		if got := containerArgs(&Flagset{Args: tt.args}); !slices.Equal(got, tt.want) {
			t.Errorf("containerArgs(%q) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

// blockImages returns two frames of gray block averages sized as the
// scoring command asks, the distorted one with tint added to red.
func blockImages(argv []string, tint uint16) (dist, ref []byte) {
	graph := argv[slices.Index(argv, "-filter_complex")+1]
	var bw, bh int
	fmt.Sscanf(graph[strings.LastIndex(graph, "rgb48le,scale=")+len("rgb48le,scale="):], "%d:%d", &bw, &bh)
	for range 2 * bw * bh {
		ref = binary.LittleEndian.AppendUint16(ref, 30000)
		ref = binary.LittleEndian.AppendUint16(ref, 30000)
		ref = binary.LittleEndian.AppendUint16(ref, 30000)
		dist = binary.LittleEndian.AppendUint16(dist, 30000+tint)
		dist = binary.LittleEndian.AppendUint16(dist, 30000)
		dist = binary.LittleEndian.AppendUint16(dist, 30000)
	}
	return dist, ref
}

func TestColorShift(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, px [][3]uint16) string {
		var b []byte
		for _, p := range px {
			for _, c := range p {
				b = binary.LittleEndian.AppendUint16(b, c)
			}
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	gray := [3]uint16{30000, 30000, 30000}
	// 2x2 blocks, one frame: one block gets a strong magenta cast.
	ref := write("ref", [][3]uint16{gray, gray, gray, gray})
	dist := write("dist", [][3]uint16{gray, gray, gray, {33000, 29000, 33000}})
	got, _, err := ColorShift(dist, ref, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got < 5 {
		t.Errorf("ColorShift = %.2f; want the cast block's shift (several a*b* units) at p99", got)
	}
	if same, _, _ := ColorShift(ref, ref, 2, 2); same != 0 {
		t.Errorf("ColorShift(ref, ref) = %v, want 0", same)
	}
	// An extra frame on one side is ignored.
	long := write("long", [][3]uint16{gray, gray, gray, {33000, 29000, 33000}, gray, gray, gray, gray})
	if a, _, err := ColorShift(long, ref, 2, 2); err != nil || a != got {
		t.Errorf("ColorShift with an extra frame = %v, %v; want %v", a, err, got)
	}
	ragged := write("ragged", [][3]uint16{gray})
	if _, _, err := ColorShift(ragged, ref, 2, 2); err == nil {
		t.Error("ColorShift with a partial frame: nil error")
	}
}

func TestBenchColorShiftLimit(t *testing.T) {
	for _, tc := range []struct {
		tint    uint16
		answers int
	}{{0, 1}, {4000, 0}} {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "samples", "a.mov"), "sample a")
		c := benchConfig(t, dir, "  x: {args: [-c:v, libx265]}\n")
		c.MaxColorShift = 3.9
		fake := newFake(18.7)
		fake.tint = tc.tint
		b := &Bench{Cfg: c, Exec: fake, Prober: fakeProber{}}
		if err := b.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		results, _ := LoadResults(filepath.Join(dir, "work", "results.csv"))
		if got := len(Bests(c, results)); got != tc.answers {
			t.Errorf("tint %d: %d answers, want %d", tc.tint, got, tc.answers)
		}
		if len(results.Rows) > 0 && results.Rows[0].Scorer != scorer {
			t.Errorf("rows logged with scorer %q, want %q", results.Rows[0].Scorer, scorer)
		}
	}
}

func TestResultsUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.csv")
	oldCols := resultColumns[:len(resultColumns)-3]
	oldRow := "a.mov,libx265,x,-c:v libx265,-crf,20,k1,f1,100,26.6,1,1,0,0,96,96,95,95,45,48,48,50,8.1,h,2026-10-24T00:00:00Z"
	// An old header, an old row, then a row appended by the new code.
	newRow := oldRow[:strings.Index(oldRow, ",k1,")] + ",k2," + oldRow[strings.Index(oldRow, ",k1,")+4:] + ",1.5,3.2,v3"
	if err := os.WriteFile(path, []byte(strings.Join(oldCols, ",")+"\n"+oldRow+"\n"+newRow+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := LoadResults(path)
	if err != nil {
		t.Fatal(err)
	}
	if old, _ := r.Lookup("k1"); old.Scorer != "v2" || !math.IsNaN(old.Metrics.ColorShiftP999) {
		t.Errorf("old row = %+v", old)
	}
	if nw, _ := r.Lookup("k2"); nw.Scorer != "v3" || nw.Metrics.ColorShiftP999 != 3.2 {
		t.Errorf("appended row = scorer %q shift %v", nw.Scorer, nw.Metrics.ColorShiftP999)
	}
	data, _ := os.ReadFile(path)
	if first := strings.SplitN(string(data), "\n", 2)[0]; first != strings.Join(resultColumns, ",") {
		t.Errorf("header not upgraded: %q", first)
	}
	// And it reads back the same.
	again, err := LoadResults(path)
	if err != nil || len(again.Rows) != 2 {
		t.Fatalf("reload: %v, %d rows", err, len(again.Rows))
	}
	if nw, _ := again.Lookup("k2"); nw.Scorer != "v3" {
		t.Errorf("after upgrade, appended row scorer %q", nw.Scorer)
	}
}

func TestOldSpellings(t *testing.T) {
	dir := t.TempDir()
	// A log whose header has the first spelling of the color columns.
	header := strings.Join(resultColumns, ",")
	header = strings.ReplaceAll(header, "color_shift", "colour_shift")
	row := "a.mov,libx265,x,-c:v libx265,-crf,20,k1,f1,100,26.6,1,1,0,0,96,96,95,95,45,48,48,50,8.1,h,2026-10-24T00:00:00Z,1.5,3.2,v3"
	path := filepath.Join(dir, "results.csv")
	if err := os.WriteFile(path, []byte(header+"\n"+row+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := LoadResults(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Lookup("k1"); got.Metrics.ColorShiftP999 != 3.2 || got.Scorer != "v3" {
		t.Errorf("row read as %+v", got)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "colour") {
		t.Errorf("header not rewritten with the current spelling:\n%s", data)
	}

	// And the config key.
	cfg := filepath.Join(dir, "bench.yaml")
	writeFile(t, cfg, "samples: s\nmax_colour_shift: 3.9\nflagsets: {a: {args: [-c:v, libx265]}}")
	c, err := LoadConfig(cfg)
	if err != nil || c.MaxColorShift != 3.9 {
		t.Errorf("max_colour_shift: %v, %v", c, err)
	}
}

func TestOldScorerRowsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.csv")
	// A row as written before color_shift_p99 and scorer existed.
	old := strings.Join(resultColumns[:len(resultColumns)-3], ",") + "\n" +
		"a.mov,libx265,x,-c:v libx265,-crf,20,k1,f1,100,26.6,1,1,0,0,96,96,95,95,45,48,48,50,8.1,h,2026-10-24T00:00:00Z\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := LoadResults(path)
	if err != nil {
		t.Fatal(err)
	}
	if row := r.Rows[0]; row.Scorer != "v2" || !math.IsNaN(row.Metrics.ColorShiftP99) || !math.IsNaN(row.Metrics.ColorShiftP999) {
		t.Errorf("old row: scorer %q, color shift %v", row.Scorer, row.Metrics.ColorShiftP99)
	}
	c := &Config{TargetVMAF: 95, Flagsets: map[string]*Flagset{"x": {Args: []string{"-c:v", "libx265"}, Quality: Quality{Flag: "-crf"}}}}
	if len(bestByPair(c, r)) != 0 {
		t.Error("an old-scorer row counted as an answer")
	}
}
