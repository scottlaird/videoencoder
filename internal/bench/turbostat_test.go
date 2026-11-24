package bench

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// render2's (Ryzen 9 9955HX) summary: no temperature columns.
const turbostatAMD = "0.512345 sec\n" +
	"Avg_MHz\tBusy%\tBzy_MHz\tTSC_MHz\tIPC\tIRQ\tLLCMRPS\tLLC%hit\tCorWatt\tPkgWatt\n" +
	"2775\t93.94\t2953\t2495\t2.27\t212838\t9027\t95.69\t37.10\t55.96\n"

func TestParseTurbostat(t *testing.T) {
	got, err := parseTurbostat(strings.NewReader(turbostatAMD), 2)
	if err != nil {
		t.Fatal(err)
	}
	want := CPUStats{BusyPct: 93.94, BusyMHz: 2953, IPC: 2.27, PkgWatts: 55.96, CoreWatts: 37.10, PkgTempC: math.NaN(), LLCHitPct: 95.69}
	if !sameStats(*got, want) {
		t.Errorf("parseTurbostat = %+v, want %+v", *got, want)
	}
	if _, err := parseTurbostat(strings.NewReader("0.5 sec\n"), 2); err == nil {
		t.Error("parseTurbostat of no summary: no error")
	}
}

// TestParseTurbostatSamples averages interval samples: two full
// 2-second intervals and the half second before turbostat was stopped.
func TestParseTurbostatSamples(t *testing.T) {
	in := "Busy%\tBzy_MHz\tIPC\tLLC%hit\tCorWatt\tPkgWatt\tPkgTmp\tTime_Of_Day_Seconds\n" +
		"50\t4000\t2\t90\t100\t200\t60\t100.0\n" +
		"100\t3000\t1\t80\t150\t300\t70\t102.0\n" +
		"100\t3000\t1\t80\t200\t400\t75\t102.5\n"
	got, err := parseTurbostat(strings.NewReader(in), 2)
	if err != nil {
		t.Fatal(err)
	}
	// Busy% and power over 4.5 s; the clock over busy time (100, 200
	// and 50 busy-seconds), IPC over busy cycles, LLC over busy time.
	want := CPUStats{
		BusyPct:   350 / 4.5,
		BusyMHz:   (4000*100 + 3000*250) / 350.0,
		IPC:       (2*400000 + 1*750000) / 1150000.0,
		PkgWatts:  1200 / 4.5,
		CoreWatts: 600 / 4.5,
		PkgTempC:  75,
		LLCHitPct: (90*100 + 80*250) / 350.0,
	}
	gf, wf := got.fields(), want.fields()
	for i, name := range turbostatColumns {
		if math.Abs(*gf[i]-*wf[i]) > 1e-9 {
			t.Errorf("%s = %v, want %v", name, *gf[i], *wf[i])
		}
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
	if got := rec[cpuColumn : cpuColumn+len(turbostatColumns)]; strings.Join(got, ",") != "50,3790,1.88,56.1,31.87,,92.64" {
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

// TestEncodeTurbostat runs a fake ffmpeg with a fake turbostat beside
// it, which writes a sample to its --out file, and another when it's
// interrupted.
func TestEncodeTurbostat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	dir := t.TempDir()
	turbostat := filepath.Join(dir, "turbostat")
	ts := "#!/bin/sh\n" +
		"while [ $# -gt 0 ]; do [ \"$1\" = --out ] && out=$2; shift; done\n" +
		"trap 'printf \"95.5\\t2950\\t2.27\\t56\\t1.5\\n\" >> \"$out\"; exit 0' INT\n" +
		"printf 'Busy%%\\tBzy_MHz\\tIPC\\tPkgWatt\\tTime_Of_Day_Seconds\\n95.5\\t2950\\t2.27\\t56\\t1\\n' > \"$out\"\n" +
		"while :; do sleep 0.01; done\n"
	ff := filepath.Join(dir, "ffmpeg")
	// It takes a moment, so turbostat is sampling before it ends.
	script := "#!/bin/sh\nsleep 0.3\nprintf 'out_time_us=10000000\\nprogress=end\\n' >&3\n"
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
		t.Errorf("progress = %v, want [10s]", progress)
	}
	want := CPUStats{BusyPct: 95.5, BusyMHz: 2950, IPC: 2.27, PkgWatts: 56, CoreWatts: math.NaN(), PkgTempC: math.NaN(), LLCHitPct: math.NaN()}
	if m.CPU == nil || !sameStats(*m.CPU, want) {
		t.Errorf("Measurement.CPU = %+v, want %+v", m.CPU, want)
	}
}

// TestRealTurbostat samples the machine with the real turbostat beside
// a fake ffmpeg that only sleeps, when ENCBENCH_REAL_TURBOSTAT is set
// (and turbostat runs without sudo): for that many seconds, if it's a
// number, else 5. Set it to 150 on a busy Threadripper to check its
// package power past where the energy count used to wrap.
func TestRealTurbostat(t *testing.T) {
	if os.Getenv("ENCBENCH_REAL_TURBOSTAT") == "" {
		t.Skip("ENCBENCH_REAL_TURBOSTAT isn't set")
	}
	turbostat := FindTurbostat(context.Background())
	if turbostat == "" {
		t.Fatal("no usable turbostat")
	}
	ff := filepath.Join(t.TempDir(), "ffmpeg")
	secs := os.Getenv("ENCBENCH_REAL_TURBOSTAT")
	if _, err := strconv.Atoi(secs); err != nil {
		secs = "5"
	}
	if err := os.WriteFile(ff, []byte("#!/bin/sh\nsleep "+secs+"\nprintf 'progress=end\\n' >&3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m, err := ExecExecutor{Turbostat: turbostat}.Encode(context.Background(), []string{ff, "-i", "in.mov", "out.mp4"}, time.Second, func(Progress) {})
	if err != nil {
		t.Fatal(err)
	}
	if m.CPU == nil || math.IsNaN(m.CPU.BusyPct) || math.IsNaN(m.CPU.BusyMHz) {
		t.Fatalf("Measurement.CPU = %+v", m.CPU)
	}
	t.Logf("wall %v: %+v", m.Wall, *m.CPU)
}
