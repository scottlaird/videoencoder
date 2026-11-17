package ffmpeg

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestReadProgress(t *testing.T) {
	in := strings.Join([]string{
		"frame=0", "out_time_us=N/A", "progress=continue",
		"frame=30", "out_time_us=1000000", "speed=2x", "progress=continue",
		"frame=60", "out_time_us=2500000", "progress=end",
	}, "\n")
	var got []time.Duration
	var frames []int64
	var ends []bool
	ReadProgress(strings.NewReader(in), func(d time.Duration, n int64, end bool) {
		got, frames, ends = append(got, d), append(frames, n), append(ends, end)
	})
	want := []time.Duration{0, time.Second, 2500 * time.Millisecond}
	if !slices.Equal(got, want) || !slices.Equal(frames, []int64{0, 30, 60}) || !slices.Equal(ends, []bool{false, false, true}) {
		t.Errorf("ReadProgress reported %v, frames %v, ends %v; want %v, frames [0 30 60], ends [false false true]", got, frames, ends, want)
	}
}

func TestProgressRemaining(t *testing.T) {
	for _, tc := range []struct {
		p      Progress
		want   time.Duration
		wantOK bool
	}{
		{Progress{Encoded: 3 * time.Second, Total: 20 * time.Second, Elapsed: 2 * time.Minute}, 680 * time.Second, true},
		{Progress{Encoded: 20 * time.Second, Total: 20 * time.Second, Elapsed: time.Minute}, 0, true},
		// Too early: nothing encoded, or only a second in.
		{Progress{Encoded: 0, Total: 20 * time.Second, Elapsed: time.Minute}, 0, false},
		{Progress{Encoded: 10 * time.Second, Total: 20 * time.Second, Elapsed: time.Second}, 0, false},
		// Unknown duration.
		{Progress{Encoded: 10 * time.Second, Elapsed: time.Minute}, 0, false},
	} {
		got, ok := tc.p.Remaining()
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("%+v.Remaining() = %v, %v; want %v, %v", tc.p, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestProgressString(t *testing.T) {
	for _, tc := range []struct {
		p    Progress
		want string
	}{
		{Progress{Encoded: 3 * time.Second, Total: 20 * time.Second, Elapsed: 2 * time.Minute}, "15% · 2:00, about 11:20 left"},
		{Progress{Encoded: 0, Total: 20 * time.Second, Elapsed: time.Second}, "0% · 0:01"},
		{Progress{Encoded: time.Second, Total: 30 * time.Second, Elapsed: 5 * time.Minute}, "3% · 5:00, about 2:25:00 left"},
	} {
		if got := tc.p.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// fakeFFmpeg writes an executable named ffmpeg running script into a
// temporary directory and returns its path.
func fakeFFmpeg(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	ff := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(ff, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return ff
}

// TestRunProgress runs a fake ffmpeg that writes progress to the pipe
// RunProgress gives it, as fd 3, and something to stderr, which should
// still reach the runner's Stderr.
func TestRunProgress(t *testing.T) {
	ff := fakeFFmpeg(t, "printf 'out_time_us=5000000\\nprogress=continue\\nout_time_us=9966667\\nprogress=end\\n' >&3\necho warning >&2\n")
	var stderr bytes.Buffer
	var got []time.Duration
	err := ExecRunner{Stderr: &stderr}.RunProgress(context.Background(), []string{ff, "-i", "in.mov", "out.mp4"}, 10*time.Second,
		func(p Progress) {
			if p.Total != 10*time.Second {
				t.Errorf("progress Total = %v, want 10s", p.Total)
			}
			got = append(got, p.Encoded)
		})
	if err != nil {
		t.Fatal(err)
	}
	// The last report is the whole input, though out_time stops a
	// frame short.
	if want := []time.Duration{5 * time.Second, 10 * time.Second}; !slices.Equal(got, want) {
		t.Errorf("RunProgress reported %v, want %v", got, want)
	}
	if !strings.Contains(stderr.String(), "warning") {
		t.Errorf("stderr = %q, want ffmpeg's warning", stderr.String())
	}
}

// TestRunProgressFailure checks that a failing ffmpeg's error comes
// back and RunProgress doesn't hang waiting for its progress.
func TestRunProgressFailure(t *testing.T) {
	ff := fakeFFmpeg(t, "printf 'out_time_us=1000000\\nprogress=continue\\n' >&3\nexit 3\n")
	n := 0
	err := ExecRunner{}.RunProgress(context.Background(), []string{ff, "-i", "in.mov", "out.mp4"}, 10*time.Second,
		func(Progress) { n++ })
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("RunProgress error = %v, want exit status 3", err)
	}
	if n != 1 {
		t.Errorf("RunProgress reported %d times, want 1", n)
	}
}

// TestRunProgressMissing checks that a command that can't start fails
// without calling progress.
func TestRunProgressMissing(t *testing.T) {
	ff := filepath.Join(t.TempDir(), "ffmpeg")
	err := ExecRunner{}.RunProgress(context.Background(), []string{ff, "-i", "in.mov", "out.mp4"}, 10*time.Second,
		func(Progress) { t.Error("progress called for a command that never started") })
	if err == nil {
		t.Error("RunProgress succeeded running a missing ffmpeg")
	}
}

// progressRunner is a fakeRunner that also reports progress, once at
// halfway, and records the duration it was given.
type progressRunner struct {
	fakeRunner
	called bool
	total  time.Duration
}

func (p *progressRunner) RunProgress(ctx context.Context, argv []string, total time.Duration, progress func(Progress)) error {
	p.called, p.total = true, total
	progress(Progress{Encoded: total / 2, Total: total})
	return p.Run(ctx, argv)
}

func TestBatchRunProgress(t *testing.T) {
	dir := t.TempDir()
	var got []Progress
	b := &Batch{Manifest: manifest(t, dir), Duration: 30 * time.Second, Progress: func(p Progress) { got = append(got, p) }}
	b.Add(newJob(dir, "main/av1-1080", "k1", "-c:v", "libsvtav1"))
	r := &progressRunner{}
	if err := b.Run(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if r.total != 30*time.Second {
		t.Errorf("RunProgress got duration %v, want 30s", r.total)
	}
	if want := []Progress{{Encoded: 15 * time.Second, Total: 30 * time.Second}}; !slices.Equal(got, want) {
		t.Errorf("Batch.Progress got %v, want %v", got, want)
	}

	// Without Progress set, the batch just runs.
	b = &Batch{Manifest: manifest(t, dir)}
	b.Add(newJob(dir, "main/av1-720", "k2", "-c:v", "libsvtav1"))
	r = &progressRunner{}
	if err := b.Run(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || r.called {
		t.Errorf("without Progress: %d runs, RunProgress called %v; want 1 run, Run not RunProgress", len(r.calls), r.called)
	}
}
