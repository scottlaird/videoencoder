package tune

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
		os.WriteFile(argv[len(argv)-6], blocks, 0o644)
		os.WriteFile(argv[len(argv)-1], blocks, 0o644)
		return bench.Measurement{}, nil
	}
	out := argv[len(argv)-1]
	if i := slices.Index(argv, "-crf"); i >= 0 {
		q, _ := strconv.ParseFloat(argv[i+1], 64)
		size := argv[slices.Index(argv, "-s")+1]
		h, _ := strconv.ParseUint(strings.Split(size, "x")[1], 10, 64)
		f.quality[out], f.height[out] = q, h
		f.encodes[fmt.Sprint(h)]++
		return bench.Measurement{}, os.WriteFile(out, bytes.Repeat([]byte("x"), int(h*1000/uint64(q))), 0o644)
	}
	return bench.Measurement{}, os.WriteFile(out, []byte("media"), 0o644)
}

type fakeProber struct{ vd probe.VideoData }

func (p fakeProber) Probe(context.Context, string) (probe.VideoData, error) {
	return probe.VideoData{Width: 3840, Height: 2160, Duration: 8 * time.Second}, nil
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

	results, err := tu.Tune(context.Background(), tuneConfig(), "src.mov", vd, targets, t.TempDir())
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
