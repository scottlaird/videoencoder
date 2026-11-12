package bench

import (
	"context"
	"encoding/csv"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fixedOutput struct{ info OutputInfo }

func (f fixedOutput) ProbeOutput(context.Context, string) (OutputInfo, error) { return f.info, nil }

func TestRowDetails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "samples", "a.mov"), "sample a")
	cfg := benchConfig(t, dir, `
  fast: {args: [-c:v, libx265, -preset, fast], batch: 2}
  broken: {args: [-c:v, libbroken], quality: {flag: -crf, min: 0, max: 50, step: 1}}
`)
	cfg.Job = "10-test.yaml"
	out := OutputInfo{Codec: "hevc", Profile: "Main", Level: "120", PixFmt: "yuv420p", Keyframes: 3, PeakKbps1s: 812.5}
	b := &Bench{Cfg: cfg, Exec: newFake(18.7), Prober: fakeProber{}, OutputProber: fixedOutput{out}, Host: "h"}
	if err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	results, err := LoadResults(filepath.Join(dir, "work", "results.csv"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	batches := map[string]int{}
	timing := 0
	for _, r := range results.Rows {
		if r.RowID == "" || ids[r.RowID] {
			t.Errorf("row %v: RowID %q, want a new one", r.Quality, r.RowID)
		}
		ids[r.RowID] = true
		if r.Source != "a.mov" || r.SampleID == "" || r.Width != 1920 || r.Height != 1080 || r.FPS != 30 ||
			r.DurationS != 30 || r.SourceWidth != 1920 || r.SourceHeight != 1080 {
			t.Errorf("row %v: sample %q %q %dx%d %v fps %v s from %dx%d; want a.mov 1920x1080 30 fps 30 s",
				r.Quality, r.Source, r.SampleID, r.Width, r.Height, r.FPS, r.DurationS, r.SourceWidth, r.SourceHeight)
		}
		if r.VMAFModel != "vmaf_v0.6.1" || r.TargetVMAF != 95 || r.Pass == nil {
			t.Errorf("row %v: model %q target %v pass %v; want vmaf_v0.6.1, 95, set", r.Quality, r.VMAFModel, r.TargetVMAF, r.Pass)
		} else if want := r.Metrics.VMAF >= 95; *r.Pass != want {
			t.Errorf("row %v: pass %v at VMAF %v", r.Quality, *r.Pass, r.Metrics.VMAF)
		}
		if r.Output != out {
			t.Errorf("row %v: output %+v, want %+v", r.Quality, r.Output, out)
		}
		if r.Config != cfg.Path || r.Job != "10-test.yaml" {
			t.Errorf("row %v: config %q job %q", r.Quality, r.Config, r.Job)
		}
		switch {
		case r.BatchSize == 2 && r.BatchID != "" && !r.Timed:
			batches[r.BatchID]++
			if r.ScoreS <= 0 {
				t.Errorf("row %v: score_s %v, want the scoring's time", r.Quality, r.ScoreS)
			}
		case r.BatchSize == 1 && r.BatchID == "" && r.Timed:
			timing++
			if r.ScoreS != 0 {
				t.Errorf("timing row: score_s %v, want 0 (not scored)", r.ScoreS)
			}
		default:
			t.Errorf("row %v: batch %d %q timed %v", r.Quality, r.BatchSize, r.BatchID, r.Timed)
		}
	}
	for id, n := range batches {
		if n != 2 {
			t.Errorf("batch %s has %d rows, want 2", id, n)
		}
	}
	if timing != 1 {
		t.Errorf("%d timing rows, want 1 (the answer, re-timed)", timing)
	}

	// The broken flagset's encode failed: in failures.csv, not results.
	fails := readFailures(t, filepath.Join(dir, "work", "failures.csv"))
	if len(fails) != 2 || fails[0][0] != "time" {
		t.Fatalf("failures.csv = %q, want a header and one failure", fails)
	}
	f := fails[1]
	if f[1] != "h" || f[2] != "10-test.yaml" || f[4] != "a.mov" || f[5] != "broken" || f[6] == "" ||
		f[7] != "encode" || !strings.Contains(f[8], "libbroken") {
		t.Errorf("failure = %q", f)
	}
}

func TestRunFailureLogged(t *testing.T) {
	dir := t.TempDir()
	cfg := benchConfig(t, dir, twoFlagsets) // no samples directory
	b := &Bench{Cfg: cfg, Exec: newFake(18.7), Prober: fakeProber{}, FlagsetFilter: "fast"}
	if err := b.Run(context.Background()); err == nil {
		t.Fatal("run without samples succeeded")
	}
	fails := readFailures(t, filepath.Join(dir, "work", "failures.csv"))
	if len(fails) != 2 || fails[1][5] != "fast" || fails[1][6] != "" || fails[1][7] != "run" {
		t.Errorf("failures.csv = %q, want one run failure for fast, no quality", fails)
	}
}

func readFailures(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestAppendFailureOneLine(t *testing.T) {
	dir := t.TempDir()
	err := AppendFailure(dir, Failure{Time: time.Unix(0, 0), Quality: math.NaN(), Stage: "run", Err: "first\n  second"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "failures.csv"))
	if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != 2 || !strings.Contains(lines[1], ",,run,first second,") {
		t.Errorf("failures.csv =\n%s", data)
	}
}

func TestDetailsRoundTrip(t *testing.T) {
	pass := false
	in := Row{Time: time.Unix(0, 0).UTC(), Scorer: "v6", Timed: true, RowID: "0123456789abcdef",
		Source: "a.mov", SampleID: "s1", Width: 3840, Height: 2160, FPS: 29.97, DurationS: 30.03,
		SourceWidth: 7680, SourceHeight: 4320, ColorTransfer: "bt709", ColorPrimaries: "bt709",
		Output:    OutputInfo{Codec: "av1", Profile: "Main", Level: "13", PixFmt: "yuv420p10le", Keyframes: 4, PeakKbps1s: 40123.5},
		VMAFModel: "vmaf_4k_v0.6.1", TargetVMAF: 95, Pass: &pass, ScoreS: 12.5, BatchSize: 2, BatchID: "b1",
		EncbenchVersion: "v0.0.0-x", Config: "bench.yaml", Job: "30-x.yaml"}
	for _, p := range []*float64{&in.Metrics.VMAFNeg, &in.Metrics.ColorShiftP99, &in.Metrics.ColorShiftP999,
		&in.Metrics.ColorFlickerP99, &in.Metrics.ColorFlickerP999, &in.Metrics.CAMBI, &in.Metrics.CAMBIFR, &in.Metrics.CAMBIFRMax} {
		*p = 1 // not NaN, which never compares equal
	}
	got, err := parseRow(columnIndex(resultColumns), in.record())
	if err != nil {
		t.Fatal(err)
	}
	in.Reference = in.Sample // parseRow's default for an empty reference
	if !reflect.DeepEqual(got, in) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, in)
	}
	// An older row has none of them.
	old, err := parseRow(columnIndex(preRowID), in.record()[:len(preRowID)])
	if err != nil || old.RowID != "" || old.Width != 0 || old.Pass != nil || old.Output != (OutputInfo{}) {
		t.Errorf("older row: %+v, %v", old, err)
	}
}

func TestParseOutputProbe(t *testing.T) {
	// Four packets: 1000 and 3000 bytes in the first second, 2000 and
	// 500 after; the busiest second is packets 2 and 3 (0.6 s to 1.6 s).
	data := `{"packets": [
		{"dts_time": "0.000000", "size": "1000", "flags": "K__"},
		{"dts_time": "0.600000", "size": "3000", "flags": "___"},
		{"dts_time": "1.500000", "size": "2000", "flags": "___"},
		{"dts_time": "10.000000", "size": "500", "flags": "K__"}],
	  "streams": [{"codec_name": "hevc", "profile": "Main 10", "level": 153, "pix_fmt": "yuv420p10le"}]}`
	got, err := parseOutputProbe([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	want := OutputInfo{Codec: "hevc", Profile: "Main 10", Level: "153", PixFmt: "yuv420p10le", Keyframes: 2, PeakKbps1s: 40}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	got, _ = parseOutputProbe([]byte(`{"streams": [{"codec_name": "av1", "level": -99}]}`))
	if got.Level != "" {
		t.Errorf("unknown level read as %q, want empty", got.Level)
	}
}

func TestDriverVersions(t *testing.T) {
	if got, want := nvidiaVersion("NVIDIA GeForce RTX 4090, 615.71.09\n"), "NVIDIA GeForce RTX 4090, driver 615.71.09"; got != want {
		t.Errorf("nvidia: %q, want %q", got, want)
	}
	vainfo := "libva info: VA-API version 1.23.0\nvainfo: Driver version: Intel iHD driver for Intel(R) Gen Graphics - 26.1.2 ()\nvainfo: Supported profile\n"
	if got, want := vaDriverVersion(vainfo), "Intel iHD driver for Intel(R) Gen Graphics - 26.1.2"; got != want {
		t.Errorf("vainfo: %q, want %q", got, want)
	}
}
