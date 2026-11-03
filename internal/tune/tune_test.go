package tune

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scottlaird/videoencoder/internal/bench"
	"github.com/scottlaird/videoencoder/internal/config"
	"github.com/scottlaird/videoencoder/internal/plan"
	"github.com/scottlaird/videoencoder/internal/probe"
)

// fakeFFmpeg cuts, scales and encodes by writing files, and scores by
// writing libvmaf logs. A rendition h pixels tall reaches VMAF 95 at
// quality cutoff(h) and loses 0.5 per step above it; files get smaller
// as quality numbers rise.
type fakeFFmpeg struct {
	mu        sync.Mutex
	encodes   map[string]int     // rendition height -> encodes
	scoredAt  []string           // reference names used
	prefilter []string           // filtergraphs used for scoring
	quality   map[string]float64 // encoded file -> quality
	height    map[string]uint64  // encoded file -> rendition height
	// toneMapped is set if any trial encode was tone-mapped.
	toneMapped bool
	// harder lowers the cutoff of samples cut into the named range
	// directories ("range1") by that many steps, and doubles their size.
	harder map[string]float64
	rng    map[string]string // encoded file -> range directory
}

func cutoff(h uint64) float64 { return 20 + float64(h)/240 } // 480p: 22, 2160p: 29

func (f *fakeFFmpeg) Exec(_ context.Context, argv []string) (bench.Measurement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := slices.Index(argv, "-filter_complex"); i >= 0 {
		graph := argv[i+1]
		var logs []string
		for _, part := range strings.Split(graph, "log_path=")[1:] {
			logs = append(logs, strings.SplitN(part, ":n_threads", 2)[0])
		}
		in := argv[slices.Index(argv, "-i")+1]
		ref := argv[slices.Index(argv, "-i")+3]
		f.scoredAt = append(f.scoredAt, ref[strings.LastIndex(ref, "/")+1:])
		f.prefilter = append(f.prefilter, graph)
		vmaf := 95 - 0.5*(f.quality[in]-cutoff(f.height[in])+f.harder[f.rng[in]])
		log := fmt.Sprintf(`{"frames":[{"metrics":{"vmaf":%v}}],"pooled_metrics":{"vmaf":{"mean":%v,"harmonic_mean":%v},"vmaf_neg":{"mean":%v},"psnr_y":{"mean":45},"psnr_cb":{"mean":48},"psnr_cr":{"mean":48}}}`, vmaf, vmaf, vmaf, vmaf)
		os.WriteFile(logs[0], []byte(log), 0o644)
		os.WriteFile(logs[1], []byte(`{"pooled_metrics":{"ciede2000":{"mean":50}}}`), 0o644)
		// Identical gray block images: no color shift.
		var bw, bh int
		fmt.Sscanf(graph[strings.LastIndex(graph, "rgb48le,scale=")+len("rgb48le,scale="):], "%d:%d", &bw, &bh)
		blocks := bytes.Repeat([]byte{0x30, 0x75}, 3*bw*bh)
		os.WriteFile(argv[len(argv)-8], blocks, 0o644)
		os.WriteFile(argv[len(argv)-1], blocks, 0o644)
		return bench.Measurement{}, nil
	}
	out := argv[len(argv)-1]
	if i := slices.Index(argv, "-crf"); i >= 0 {
		q, _ := strconv.ParseFloat(argv[i+1], 64)
		vf := argv[slices.Index(argv, "-vf")+1]
		var w, h uint64
		fmt.Sscanf(vf[strings.Index(vf, "scale="):], "scale=%d:%d", &w, &h)
		f.quality[out], f.height[out] = q, h
		if i := slices.Index(argv, "-vf"); i >= 0 && strings.Contains(argv[i+1], "tonemap=") {
			f.toneMapped = true
		}
		f.encodes[fmt.Sprint(h)]++
		if f.rng == nil {
			f.rng = map[string]string{}
		}
		f.rng[out] = filepath.Base(filepath.Dir(argv[slices.Index(argv, "-i")+1]))
		size := int(h * 1000 / uint64(q))
		if f.harder[f.rng[out]] > 0 {
			size *= 2
		}
		return bench.Measurement{}, os.WriteFile(out, bytes.Repeat([]byte("x"), size), 0o644)
	}
	return bench.Measurement{}, os.WriteFile(out, []byte("media"), 0o644)
}

type fakeProber struct{ vd probe.VideoData }

func (p fakeProber) Probe(context.Context, string) (probe.VideoData, error) {
	return probe.VideoData{Width: 3840, Height: 2160, Duration: 8 * time.Second}, nil
}

// source writes a stand-in source file, which tune identifies by
// content.
func source(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src.mov")
	if err := os.WriteFile(p, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func tuneConfig() *config.Resolved {
	r := &config.Resolved{}
	r.Name = "v"
	r.Tune = config.Tune{TargetVMAF: 95, Ranges: []config.TuneRange{{Start: "0s", Duration: "8s"}}, Anchors: 2}
	r.Renditions = map[string]*config.Rendition{"av1-720": {TargetVMAF: 94}}
	return r
}

func TestTune(t *testing.T) {
	av1 := config.Encoder{Codec: "libsvtav1", QualityFlag: "-crf", Args: []string{"-preset", "5"}}
	targets := plan.Targets{
		{Name: "av1-480", EncoderName: "av1", Encoder: av1, Width: 852, Height: 480, Quality: 28, HDRQuality: 28},
		{Name: "av1-720", EncoderName: "av1", Encoder: av1, Width: 1280, Height: 720, Quality: 30, HDRQuality: 30},
		{Name: "av1-1080", EncoderName: "av1", Encoder: av1, Width: 1920, Height: 1080, Quality: 31, HDRQuality: 31},
		{Name: "av1-2160", EncoderName: "av1", Encoder: av1, Width: 3840, Height: 2160, Quality: 32, HDRQuality: 32},
	}
	f := &fakeFFmpeg{encodes: map[string]int{}, quality: map[string]float64{}, height: map[string]uint64{}}
	tu := &Tuner{Exec: f, Prober: fakeProber{}, Threads: 1}
	vd := probe.VideoData{Width: 3840, Height: 2160, ColorTransfer: "smpte2084"}

	results, err := tu.Tune(context.Background(), tuneConfig(), source(t), vd, targets, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Result{}
	for _, r := range results {
		got[r.Rendition] = r
	}
	// Anchors are searched: the answer is the cutoff.
	if r := got["av1-480"]; r.Quality != 22 || r.Interpolated || r.Outcome != bench.Found {
		t.Errorf("av1-480 = %+v, want crf 22 found", r)
	}
	if r := got["av1-2160"]; r.Quality != 29 || r.Interpolated {
		t.Errorf("av1-2160 = %+v, want crf 29", r)
	}
	// 1080p is the tallest rendition the 1080 model scores, so it's an
	// anchor too, and the 4K model's 2160p isn't interpolated across.
	if r := got["av1-1080"]; r.Interpolated || r.Quality != 24 {
		t.Errorf("av1-1080 = %+v, want searched crf 24", r)
	}
	// The rest are interpolated in log height and rounded to the step.
	if r := got["av1-720"]; !r.Interpolated || r.Quality != 23 || r.Target != 94 {
		t.Errorf("av1-720 = %+v, want interpolated crf 23 for its own target 94", r)
	}
	// Only the anchors were encoded, each in a handful of tries.
	if f.encodes["720"] != 0 || f.encodes["1080"] == 0 || f.encodes["480"] == 0 || f.encodes["480"] > 6 {
		t.Errorf("encodes by height = %v", f.encodes)
	}
	// Each anchor is judged at its own size; HDR is tone-mapped first.
	if !slices.Contains(f.scoredAt, "ref-480.mkv") || !slices.Contains(f.scoredAt, "ref-2160.mkv") || slices.Contains(f.scoredAt, "ref-720.mkv") {
		t.Errorf("references used: %v", f.scoredAt)
	}
	if !strings.Contains(f.prefilter[0], "tonemap=") {
		t.Errorf("HDR sample scored without tone mapping: %q", f.prefilter[0])
	}
	if b := got["av1-2160"].Bitrate; b == 0 || b < got["av1-480"].Bitrate {
		t.Errorf("bitrates 480p %d, 2160p %d", got["av1-480"].Bitrate, b)
	}
}

// An SDR-only encoder tunes HDR as a render would make it: tone-mapped,
// and judged against a tone-mapped reference with no further filtering.
func TestTuneSDROnly(t *testing.T) {
	h264 := config.Encoder{Codec: "libx264", QualityFlag: "-crf", SDROnly: true}
	targets := plan.Targets{
		{Name: "h264-480", EncoderName: "h264", Encoder: h264, Width: 852, Height: 480, Quality: 23},
		{Name: "h264-1080", EncoderName: "h264", Encoder: h264, Width: 1920, Height: 1080, Quality: 23},
	}
	f := &fakeFFmpeg{encodes: map[string]int{}, quality: map[string]float64{}, height: map[string]uint64{}}
	tu := &Tuner{Exec: f, Prober: fakeProber{}, Threads: 1}
	vd := probe.VideoData{Width: 3840, Height: 2160, ColorTransfer: "smpte2084"}
	if _, err := tu.Tune(context.Background(), tuneConfig(), source(t), vd, targets, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(slices.Compact(f.scoredAt), []string{"ref-480-sdr.mkv", "ref-1080-sdr.mkv"}) {
		t.Errorf("references used: %v, want ref-480-sdr.mkv and ref-1080-sdr.mkv", f.scoredAt)
	}
	for _, g := range f.prefilter {
		if strings.Contains(g, "tonemap=") {
			t.Errorf("tone-mapped encode scored with a tone map: %q", g)
		}
	}
	if !f.toneMapped {
		t.Error("trial encodes weren't tone-mapped")
	}
}

func TestReferenceSize(t *testing.T) {
	vd := probe.VideoData{Width: 7680, Height: 4320}
	tests := []struct{ w, h uint64 }{{852, 480}, {1920, 1080}, {3840, 2160}}
	tu := &Tuner{}
	for _, tt := range tests {
		ref := tu.reference(map[refKey]reference{}, vd, &plan.Target{Width: tt.w, Height: tt.h}, false, "w")
		if ref.width != tt.w || ref.height != tt.h {
			t.Errorf("reference for %dx%d is %dx%d, want its own size", tt.w, tt.h, ref.width, ref.height)
		}
	}
}

// VMAF can't judge renditions above 2160p, so Tune refuses them
// rather than scoring something meaningless.
func TestTuneTooTall(t *testing.T) {
	av1 := config.Encoder{Codec: "libsvtav1", QualityFlag: "-crf"}
	targets := plan.Targets{
		{Name: "av1-2160", EncoderName: "av1", Encoder: av1, Width: 3840, Height: 2160, Quality: 32},
		{Name: "av1-4320", EncoderName: "av1", Encoder: av1, Width: 7680, Height: 4320, Quality: 32},
	}
	f := &fakeFFmpeg{encodes: map[string]int{}, quality: map[string]float64{}, height: map[string]uint64{}}
	tu := &Tuner{Exec: f, Prober: fakeProber{}, Threads: 1}
	vd := probe.VideoData{Width: 7680, Height: 4320}
	_, err := tu.Tune(context.Background(), tuneConfig(), source(t), vd, targets, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "av1-4320") {
		t.Errorf("Tune with an 8K rendition: err = %v, want one naming av1-4320", err)
	}
	if len(f.encodes) != 0 {
		t.Errorf("encoded %v before refusing", f.encodes)
	}
}

// A results file lets a later run reuse earlier trials: the same
// target needs no encodes at all, and a new one starts from the
// bracket the old trials give. Trials with other flags don't count.
func TestTuneResults(t *testing.T) {
	av1 := config.Encoder{Codec: "libsvtav1", QualityFlag: "-crf", Args: []string{"-preset", "5"}}
	targets := func(enc config.Encoder) plan.Targets {
		return plan.Targets{
			{Name: "av1-480", EncoderName: "av1", Encoder: enc, Width: 852, Height: 480, Quality: 28},
			{Name: "av1-2160", EncoderName: "av1", Encoder: enc, Width: 3840, Height: 2160, Quality: 32},
		}
	}
	src := source(t)
	file := filepath.Join(t.TempDir(), ResultsFile)
	vd := probe.VideoData{Width: 3840, Height: 2160, ColorTransfer: "bt709"}
	run := func(goal float64, enc config.Encoder) (map[string]Result, int) {
		t.Helper()
		f := &fakeFFmpeg{encodes: map[string]int{}, quality: map[string]float64{}, height: map[string]uint64{}}
		tu := &Tuner{Exec: f, Prober: fakeProber{}, Threads: 1, Results: file, FFmpegVersion: "9.0"}
		r := tuneConfig()
		r.Tune.TargetVMAF = goal
		r.Renditions = nil
		results, err := tu.Tune(context.Background(), r, src, vd, targets(enc), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]Result{}
		for _, res := range results {
			got[res.Rendition] = res
		}
		return got, f.encodes["480"] + f.encodes["2160"]
	}

	first, n1 := run(95, av1)
	if first["av1-480"].Quality != 22 || first["av1-2160"].Quality != 29 || n1 == 0 {
		t.Fatalf("first run = %+v after %d encodes", first, n1)
	}

	again, n2 := run(95, av1)
	if n2 != 0 {
		t.Errorf("same target again: %d encodes, want 0", n2)
	}
	if again["av1-480"].Quality != 22 || again["av1-2160"].Quality != 29 || again["av1-480"].Cached == 0 {
		t.Errorf("same target again = %+v", again)
	}
	if b1, b2 := first["av1-2160"].Bitrate, again["av1-2160"].Bitrate; b1 != b2 {
		t.Errorf("cached bitrate %d, want %d", b2, b1)
	}

	// VMAF 93 is 4 steps past the 95 cutoff.
	lower, n3 := run(93, av1)
	t.Logf("encodes: first %d, same target %d, target 93 %d", n1, n2, n3)
	if lower["av1-480"].Quality != 26 || lower["av1-2160"].Quality != 33 {
		t.Errorf("target 93 = %+v, want crf 26 and 33", lower)
	}
	if n3 >= n1 {
		t.Errorf("target 93 took %d encodes, no fewer than the first run's %d", n3, n1)
	}

	other := av1
	other.Args = []string{"-preset", "6"}
	if res, n := run(95, other); n == 0 || res["av1-480"].Cached != 0 {
		t.Errorf("other flags: %d encodes, %d cached; want fresh trials", n, res["av1-480"].Cached)
	}

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if !strings.HasPrefix(lines[0], "time,key,rendition,quality,vmaf,bitrate") || len(lines) < 2 {
		t.Errorf("results file starts %q, has %d lines", lines[0], len(lines))
	}
}

func TestWithoutQuality(t *testing.T) {
	got := withoutQuality([]string{"-c:v", "libx265", "-crf", "20", "-crf-max", "x"}, "-crf")
	want := []string{"-c:v", "libx265", "-crf", "{quality}", "-crf-max", "x"}
	if !slices.Equal(got, want) {
		t.Errorf("withoutQuality = %v, want %v", got, want)
	}
}

// An HDR VMAF model scores HDR renditions as HDR, converted for the
// model rather than tone-mapped, and SDR-only ones with the built-in
// model. Its trials don't share a cache with the built-in model's.
func TestTuneHDRModel(t *testing.T) {
	model := filepath.Join(t.TempDir(), "hdr_vmaf_v1.1.8_3d0h.json")
	if err := os.WriteFile(model, []byte(`{"model_dict": {"conversion_target": {"colorspace": {"trc": "smpte2084"}, "resolution": {"display": 1080}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	av1 := config.Encoder{Codec: "libsvtav1", QualityFlag: "-crf"}
	h264 := config.Encoder{Codec: "libx264", QualityFlag: "-crf", SDROnly: true}
	targets := plan.Targets{
		{Name: "av1-480", EncoderName: "av1", Encoder: av1, Width: 852, Height: 480, Quality: 28, HDRQuality: 28},
		{Name: "av1-1080", EncoderName: "av1", Encoder: av1, Width: 1920, Height: 1080, Quality: 30, HDRQuality: 30},
		{Name: "h264-480", EncoderName: "h264", Encoder: h264, Width: 852, Height: 480, Quality: 23, HDRQuality: 23},
	}
	src := source(t)
	file := filepath.Join(t.TempDir(), ResultsFile)
	vd := probe.VideoData{Width: 3840, Height: 2160, ColorTransfer: "smpte2084"}
	run := func(vmafModel string) (*fakeFFmpeg, map[string]Result) {
		t.Helper()
		f := &fakeFFmpeg{encodes: map[string]int{}, quality: map[string]float64{}, height: map[string]uint64{}}
		tu := &Tuner{Exec: f, Prober: fakeProber{}, Threads: 1, Results: file}
		r := tuneConfig()
		r.Tune.VMAFModel = vmafModel
		results, err := tu.Tune(context.Background(), r, src, vd, targets, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]Result{}
		for _, res := range results {
			got[res.Rendition] = res
		}
		return f, got
	}

	if f, _ := run(config.BuiltinVMAFModel); len(f.prefilter) == 0 {
		t.Fatal("no trials with the built-in model")
	}
	f, got := run(model)
	if len(f.prefilter) == 0 {
		t.Fatal("HDR model reused the built-in model's trials")
	}
	for i, g := range f.prefilter {
		sdr := strings.HasSuffix(f.scoredAt[i], "-sdr.mkv")
		// libvmaf gets a copy of the model without its
		// conversion_target, which the filtergraph carries out.
		hdrScored := strings.Contains(g, ".model.json") && strings.Contains(g, "m=ictcp") && !strings.Contains(g, "tonemap=")
		if sdr == hdrScored || sdr != strings.Contains(g, "version=vmaf_v0.6.1") {
			t.Errorf("against %s: scored with %q", f.scoredAt[i], g)
		}
		// Scaled to the model's 1080-line display.
		if want := strings.HasSuffix(f.scoredAt[i], "ref-480.mkv"); want != strings.Contains(g, "zscale=w=1918:h=1080:f=bicubic:") {
			t.Errorf("against %s: display scaling %v, want %v, in %q", f.scoredAt[i], !want, want, g)
		}
	}
	for name, want := range map[string]string{"av1-480": "hdr_vmaf_v1.1.8_3d0h", "av1-1080": "hdr_vmaf_v1.1.8_3d0h", "h264-480": ""} {
		if got[name].Model != want {
			t.Errorf("%s model = %q, want %q", name, got[name].Model, want)
		}
	}

	if f, _ := run(model); len(f.prefilter) != 0 {
		t.Errorf("HDR model again: %d scorings, want all cached", len(f.prefilter))
	}
	// An SDR model scores just as the built-in one does, but its scores
	// are its own.
	sdrModel := filepath.Join(t.TempDir(), "vmaf_v1.0.16_3d0h.json")
	if err := os.WriteFile(sdrModel, []byte(`{"model_dict": {}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if f, got := run(sdrModel); len(f.prefilter) == 0 || got["av1-480"].Model != "vmaf_v1.0.16_3d0h" {
		t.Errorf("SDR model: %d scorings, model %q; want fresh trials with vmaf_v1.0.16_3d0h", len(f.prefilter), got["av1-480"].Model)
	}
	for name, want := range map[string]bool{"av1-480": true, "av1-1080": true, "h264-480": false} {
		if got[name].HDR != want {
			t.Errorf("%s HDR = %v, want %v", name, got[name].HDR, want)
		}
	}
}

// TestTuneHDRSettings checks that HDR renditions are tuned with tune's
// HDR model and target, from their HDR quality, while SDR-only ones keep
// the SDR settings.
func TestTuneHDRSettings(t *testing.T) {
	model := filepath.Join(t.TempDir(), "hdr_vmaf_v1.1.8_3d0h.json")
	if err := os.WriteFile(model, []byte(`{"model_dict": {"conversion_target": {"colorspace": {"trc": "smpte2084"}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	av1 := config.Encoder{Codec: "libsvtav1", QualityFlag: "-crf"}
	h264 := config.Encoder{Codec: "libx264", QualityFlag: "-crf", SDROnly: true}
	targets := plan.Targets{
		{Name: "av1-480", EncoderName: "av1", Encoder: av1, Width: 852, Height: 480, Quality: 28, HDRQuality: 20},
		{Name: "av1-1080", EncoderName: "av1", Encoder: av1, Width: 1920, Height: 1080, Quality: 30, HDRQuality: 22},
		{Name: "h264-480", EncoderName: "h264", Encoder: h264, Width: 852, Height: 480, Quality: 23, HDRQuality: 10},
	}
	f := &fakeFFmpeg{encodes: map[string]int{}, quality: map[string]float64{}, height: map[string]uint64{}}
	tu := &Tuner{Exec: f, Prober: fakeProber{}, Threads: 1}
	r := tuneConfig()
	r.Tune.VMAFModel = config.BuiltinVMAFModel
	r.Tune.HDR = config.TuneHDR{TargetVMAF: 93, VMAFModel: model}
	r.Renditions = map[string]*config.Rendition{"av1-1080": {TargetVMAF: 97, HDR: &config.RenditionHDR{TargetVMAF: 92}}}
	vd := probe.VideoData{Width: 3840, Height: 2160, ColorTransfer: "smpte2084"}
	var logs bytes.Buffer
	defer slog.SetDefault(slog.Default())
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	results, err := tu.Tune(context.Background(), r, source(t), vd, targets, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Each trial says which tone it tuned.
	seen := 0
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if !strings.Contains(line, "msg=Trial") && !strings.Contains(line, "msg=Tuned") {
			continue
		}
		seen++
		hdrOut := !strings.Contains(line, "rendition=h264-480")
		if want := "tone=" + toneName(hdrOut); !strings.Contains(line, want) {
			t.Errorf("log line lacks %s: %s", want, line)
		}
	}
	if seen == 0 {
		t.Errorf("no Trial or Tuned log lines:\n%s", logs.String())
	}
	want := map[string]struct {
		hdr    bool
		target float64
		model  string
	}{
		"av1-480":  {true, 93, "hdr_vmaf_v1.1.8_3d0h"},
		"av1-1080": {true, 92, "hdr_vmaf_v1.1.8_3d0h"},
		"h264-480": {false, 95, ""},
	}
	for _, res := range results {
		w := want[res.Rendition]
		if res.HDR != w.hdr || res.Target != w.target || res.Model != w.model {
			t.Errorf("%s: HDR %v, target %g, model %q; want %v, %g, %q", res.Rendition, res.HDR, res.Target, res.Model, w.hdr, w.target, w.model)
		}
	}
}

// argvRecorder records each command it's asked to run and writes its
// output file.
type argvRecorder struct{ argvs [][]string }

func (r *argvRecorder) Exec(_ context.Context, argv []string) (bench.Measurement, error) {
	r.argvs = append(r.argvs, argv)
	return bench.Measurement{}, os.WriteFile(argv[len(argv)-1], []byte("media"), 0o644)
}

func TestMakeReferenceFormat(t *testing.T) {
	tests := []struct {
		toneMap bool
		want    string // the -vf's last filter
	}{
		{false, plan.Scale(852, 480)},
		{true, "format=gbrp16le"},
	}
	for _, tt := range tests {
		r := &argvRecorder{}
		tu := &Tuner{Exec: r, Threads: 1}
		ref := reference{path: filepath.Join(t.TempDir(), "ref.mkv"), width: 852, height: 480, toneMap: tt.toneMap}
		if err := tu.makeReference(context.Background(), "sample.mov", ref); err != nil {
			t.Fatal(err)
		}
		argv := r.argvs[0]
		vf := argv[slices.Index(argv, "-vf")+1]
		filters := strings.Split(vf, ",")
		if got := filters[len(filters)-1]; got != tt.want {
			t.Errorf("toneMap %v: -vf ends with %q, want %q (-vf %s)", tt.toneMap, got, tt.want, vf)
		}
	}
}

func TestTuneRanges(t *testing.T) {
	av1 := config.Encoder{Codec: "libsvtav1", QualityFlag: "-crf", Args: []string{"-preset", "5"}}
	targets := func() plan.Targets {
		return plan.Targets{
			{Name: "av1-480", EncoderName: "av1", Encoder: av1, Width: 852, Height: 480, Quality: 28},
			{Name: "av1-2160", EncoderName: "av1", Encoder: av1, Width: 3840, Height: 2160, Quality: 32},
		}
	}
	vd := probe.VideoData{Width: 3840, Height: 2160, Duration: 40 * time.Second}
	src := source(t)
	tuneOn := func(tu *Tuner, ranges ...config.TuneRange) map[string]Result {
		t.Helper()
		r := tuneConfig()
		r.Tune.Ranges = ranges
		results, err := tu.Tune(context.Background(), r, src, vd, targets(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]Result{}
		for _, res := range results {
			got[res.Rendition] = res
		}
		return got
	}
	titles, scenery := config.TuneRange{Start: "0s", Duration: "4s"}, config.TuneRange{Start: "30s", Duration: "8s"}

	// The second range is 4 steps harder, so it decides both the
	// quality and the bitrate: the same as tuning on it alone.
	newFake := func() *fakeFFmpeg {
		return &fakeFFmpeg{encodes: map[string]int{}, quality: map[string]float64{}, height: map[string]uint64{},
			harder: map[string]float64{"range1": 4}}
	}
	results := filepath.Join(t.TempDir(), ResultsFile)
	f := newFake()
	tu := &Tuner{Exec: f, Prober: fakeProber{}, Threads: 1, Results: results}
	both := tuneOn(tu, titles, scenery)
	if r := both["av1-480"]; r.Quality != 18 || r.Outcome != bench.Found {
		t.Errorf("av1-480 over both ranges = %+v, want crf 18 found", r)
	}
	// Tuned alone, the harder range sits in range0's directory.
	f2 := newFake()
	f2.harder = map[string]float64{"range0": 4}
	alone := tuneOn(&Tuner{Exec: f2, Prober: fakeProber{}, Threads: 1}, scenery)
	for name, r := range both {
		if a := alone[name]; r.Quality != a.Quality || r.Bitrate != a.Bitrate {
			t.Errorf("%s over both ranges = crf %v at %v, want the harder range's crf %v at %v", name, r.Quality, r.Bitrate, a.Quality, a.Bitrate)
		}
	}

	// Each range's trials are recorded under its own key, so a rerun
	// encodes nothing.
	f.encodes = map[string]int{}
	tuneOn(tu, titles, scenery)
	if n := f.encodes["480"] + f.encodes["2160"]; n != 0 {
		t.Errorf("rerun made %d encodes, want 0", n)
	}
	log, err := os.ReadFile(results)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{",0s,4s,", ",30s,8s,"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("results lack trials for range %q:\n%s", want, log)
		}
	}

	// A range past the end of the source is an error.
	r := tuneConfig()
	r.Tune.Ranges = []config.TuneRange{{Start: "35s", Duration: "8s"}}
	_, err = tu.Tune(context.Background(), r, src, vd, targets(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "runs past the end") {
		t.Errorf("Tune with a range past the end = %v, want an error", err)
	}
}
