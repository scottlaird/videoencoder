package tune

import (
	"bytes"
	"context"
	"fmt"
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
		vmaf := 95 - 0.5*(f.quality[in]-cutoff(f.height[in]))
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
		size := argv[slices.Index(argv, "-s")+1]
		h, _ := strconv.ParseUint(strings.Split(size, "x")[1], 10, 64)
		f.quality[out], f.height[out] = q, h
		if i := slices.Index(argv, "-vf"); i >= 0 && strings.Contains(argv[i+1], "tonemap=") {
			f.toneMapped = true
		}
		f.encodes[fmt.Sprint(h)]++
		return bench.Measurement{}, os.WriteFile(out, bytes.Repeat([]byte("x"), int(h*1000/uint64(q))), 0o644)
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
	r.Tune = config.Tune{TargetVMAF: 95, Start: "0s", Duration: "8s", Anchors: 2}
	r.Renditions = map[string]*config.Rendition{"av1-720": {TargetVMAF: 94}}
	return r
}

func TestTune(t *testing.T) {
	av1 := config.Encoder{Codec: "libsvtav1", QualityFlag: "-crf", Args: []string{"-preset", "5"}}
	targets := plan.Targets{
		{Name: "av1-480", EncoderName: "av1", Encoder: av1, Width: 852, Height: 480, Quality: 28},
		{Name: "av1-720", EncoderName: "av1", Encoder: av1, Width: 1280, Height: 720, Quality: 30},
		{Name: "av1-1080", EncoderName: "av1", Encoder: av1, Width: 1920, Height: 1080, Quality: 31},
		{Name: "av1-2160", EncoderName: "av1", Encoder: av1, Width: 3840, Height: 2160, Quality: 32},
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
	// The rest are interpolated in log height and rounded to the step.
	if r := got["av1-1080"]; !r.Interpolated || r.Quality != 26 {
		t.Errorf("av1-1080 = %+v, want interpolated crf 26", r)
	}
	if r := got["av1-720"]; r.Target != 94 {
		t.Errorf("av1-720 target = %v, want its own 94", r.Target)
	}
	// Only the anchors were encoded, each in a handful of tries.
	if f.encodes["720"] != 0 || f.encodes["1080"] != 0 || f.encodes["480"] == 0 || f.encodes["480"] > 6 {
		t.Errorf("encodes by height = %v", f.encodes)
	}
	// 480p is judged at 1080p, 2160p at 2160p; HDR is tone-mapped first.
	if !slices.Contains(f.scoredAt, "ref-1080.mkv") || !slices.Contains(f.scoredAt, "ref-2160.mkv") {
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
	if !slices.Equal(slices.Compact(f.scoredAt), []string{"ref-1080-sdr.mkv"}) {
		t.Errorf("references used: %v, want only ref-1080-sdr.mkv", f.scoredAt)
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

func TestViewingHeight(t *testing.T) {
	tests := []struct{ h, source, want uint64 }{
		{480, 2160, 1080}, {1080, 2160, 1080}, {1440, 3024, 2160}, {3024, 3024, 2160}, {480, 720, 720},
	}
	for _, tt := range tests {
		if got := viewingHeight(tt.h, tt.source); got != tt.want {
			t.Errorf("viewingHeight(%d, %d) = %d, want %d", tt.h, tt.source, got, tt.want)
		}
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
