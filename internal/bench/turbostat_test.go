package bench

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// render2's (Ryzen 9 9955HX) summary: no temperature columns.
const turbostatAMD = "0.512345 sec\n" +
	"Avg_MHz\tBusy%\tBzy_MHz\tTSC_MHz\tIPC\tIRQ\tLLCMRPS\tLLC%hit\tCorWatt\tPkgWatt\n" +
	"2775\t93.94\t2953\t2495\t2.27\t212838\t9027\t95.69\t37.10\t55.96\n"

func TestParseTurbostat(t *testing.T) {
	got, err := parseTurbostat(strings.NewReader(turbostatAMD))
	if err != nil {
		t.Fatal(err)
	}
	want := CPUStats{BusyPct: 93.94, BusyMHz: 2953, IPC: 2.27, PkgWatts: 55.96, CoreWatts: 37.10, PkgTempC: math.NaN(), LLCHitPct: 95.69}
	if !sameStats(*got, want) {
		t.Errorf("parseTurbostat = %+v, want %+v", *got, want)
	}
	if _, err := parseTurbostat(strings.NewReader("0.5 sec\n")); err == nil {
		t.Error("parseTurbostat of no summary: no error")
	}
}

// sameStats is a == b, with NaN equal to NaN.
func sameStats(a, b CPUStats) bool {
	af, bf := a.fields(), b.fields()
	for i := range af {
		if *af[i] != *bf[i] && !(math.IsNaN(*af[i]) && math.IsNaN(*bf[i])) {
			return false
		}
	}
	return true
}

func TestCPUStatsRoundTrip(t *testing.T) {
	stats := CPUStats{BusyPct: 50, BusyMHz: 3790, IPC: 1.88, PkgWatts: 56.1, CoreWatts: 31.87, PkgTempC: math.NaN(), LLCHitPct: 92.64}
	in := Row{Time: time.Unix(0, 0).UTC(), Scorer: "v6", Timed: true, CPUStats: &stats}
	rec := in.record()
	if got := rec[len(rec)-len(turbostatColumns):]; strings.Join(got, ",") != "50,3790,1.88,56.1,31.87,,92.64" {
		t.Errorf("CPU columns = %q", got)
	}
	got, err := parseRow(columnIndex(resultColumns), rec)
	if err != nil {
		t.Fatal(err)
	}
	if got.CPUStats == nil || !sameStats(*got.CPUStats, stats) {
		t.Errorf("round trip: CPUStats = %+v, want %+v", got.CPUStats, stats)
	}
	// Without turbostat the columns are empty, and read back as nil.
	in.CPUStats = nil
	if got, err := parseRow(columnIndex(resultColumns), in.record()); err != nil || got.CPUStats != nil {
		t.Errorf("no stats: CPUStats = %+v, %v; want nil", got.CPUStats, err)
	}
}

// TestEncodeTurbostat runs a fake ffmpeg under a fake turbostat, which
// writes a summary to its --out file and runs the command after "--".
func TestEncodeTurbostat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	dir := t.TempDir()
	turbostat := filepath.Join(dir, "turbostat")
	ts := "#!/bin/sh\n" +
		"while [ \"$1\" != -- ]; do [ \"$1\" = --out ] && out=$2; shift; done; shift\n" +
		"printf '0.5 sec\\nBusy%%\\tBzy_MHz\\tIPC\\tPkgWatt\\n95.5\\t2950\\t2.27\\t56\\n' > \"$out\"\n" +
		"exec \"$@\"\n"
	ff := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\nprintf 'out_time_us=10000000\\nprogress=end\\n' >&3\n"
	for path, body := range map[string]string{turbostat: ts, ff: script} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var progress []time.Duration
	m, err := ExecExecutor{Turbostat: turbostat}.Encode(context.Background(), []string{ff, "-i", "in.mov", "out.mp4"}, 10*time.Second,
		func(p Progress) { progress = append(progress, p.Encoded) })
	if err != nil {
		t.Fatal(err)
	}
	if len(progress) != 1 || progress[0] != 10*time.Second {
		t.Errorf("progress through turbostat = %v, want [10s]", progress)
	}
	want := CPUStats{BusyPct: 95.5, BusyMHz: 2950, IPC: 2.27, PkgWatts: 56, CoreWatts: math.NaN(), PkgTempC: math.NaN(), LLCHitPct: math.NaN()}
	if m.CPU == nil || !sameStats(*m.CPU, want) {
		t.Errorf("Measurement.CPU = %+v, want %+v", m.CPU, want)
	}
}
