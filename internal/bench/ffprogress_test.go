package bench

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestEncodeProgress runs a fake ffmpeg that writes progress to the
// pipe Encode gives it, as fd 3.
func TestEncodeProgress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'out_time_us=5000000\\nprogress=continue\\nout_time_us=9966667\\nprogress=end\\n' >&3\n"
	ff := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(ff, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var got []time.Duration
	_, err := ExecExecutor{}.Encode(context.Background(), []string{ff, "-i", "in.mov", "out.mp4"}, 10*time.Second,
		func(p Progress) {
			if p.Total != 10*time.Second {
				t.Errorf("progress Total = %v, want 10s", p.Total)
			}
			got = append(got, p.Encoded)
		})
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second}
	if !slices.Equal(got, want) {
		t.Errorf("Encode reported %v, want %v", got, want)
	}
}

// TestExecProgressCountsFrames runs a fake ffmpeg whose output times
// are frame numbers in microseconds, as scoring's are, so only its
// frame counts say how far it has got.
func TestExecProgressCountsFrames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'frame=150\\nout_time_us=150\\nprogress=continue\\nframe=299\\nout_time_us=299\\nprogress=end\\n' >&3\n"
	ff := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(ff, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var got []time.Duration
	_, err := ExecExecutor{}.ExecProgress(context.Background(), []string{ff, "-i", "a.mp4", "-i", "b.mp4", "-f", "null", "-"}, 10*time.Second, 30,
		func(p Progress) { got = append(got, p.Encoded) })
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second}
	if !slices.Equal(got, want) {
		t.Errorf("ExecProgress reported %v, want %v", got, want)
	}
}

// TestEncodeErrorUnderTurbostat runs a fake ffmpeg that fails, under a
// fake turbostat that exits with its status as turbostat does: the
// error should name ffmpeg and quote its last words, not turbostat's
// exit status alone.
// TestEncodeErrorWithTurbostat checks that ffmpeg's failures are
// reported as its own, with turbostat sampling beside it.
func TestEncodeErrorWithTurbostat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	dir := t.TempDir()
	write := func(name, script string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ts := write("turbostat", "#!/bin/sh\ntrap 'exit 0' INT\nwhile :; do sleep 0.01; done\n")
	for _, tc := range []struct {
		name, ffmpeg string
		want         []string
	}{
		{"error", "#!/bin/sh\necho 'noise' >&2\necho 'Temporal filtering needs at least 4 B-Frames (-bf 4).' >&2\nexit 234\n",
			[]string{"running ffmpeg", "exit status 234", "Temporal filtering needs at least 4 B-Frames"}},
		{"crash", "#!/bin/sh\nkill -SEGV $$\n",
			[]string{"running ffmpeg", "segmentation fault"}},
	} {
		ff := write("ffmpeg", tc.ffmpeg)
		_, err := ExecExecutor{Turbostat: ts}.Encode(context.Background(), []string{ff, "-i", "in.mov", "out.mp4"}, 10*time.Second, func(Progress) {})
		if err == nil {
			t.Fatalf("%s: Encode succeeded, want an error", tc.name)
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: Encode error %q doesn't say %q", tc.name, err, w)
			}
		}
	}
}

func TestTailWriterLastLines(t *testing.T) {
	tw := &tailWriter{max: 32}
	for _, s := range []string{"first line that scrolls away\n", "a\n\n", "b\nc\n"} {
		tw.Write([]byte(s))
	}
	if got, want := tw.lastLines(2), "b / c"; got != want {
		t.Errorf("lastLines(2) = %q, want %q", got, want)
	}
	if got := len(tw.buf); got > 32 {
		t.Errorf("kept %d bytes, want at most 32", got)
	}
}
