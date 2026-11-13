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

func TestReadFFmpegProgress(t *testing.T) {
	in := strings.Join([]string{
		"frame=0", "out_time_us=N/A", "progress=continue",
		"frame=30", "out_time_us=1000000", "speed=2x", "progress=continue",
		"frame=60", "out_time_us=2500000", "progress=end",
	}, "\n")
	var got []time.Duration
	readFFmpegProgress(strings.NewReader(in), func(d time.Duration) { got = append(got, d) })
	want := []time.Duration{0, time.Second, 2500 * time.Millisecond}
	if !slices.Equal(got, want) {
		t.Errorf("readFFmpegProgress reported %v, want %v", got, want)
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

// TestEncodeProgress runs a fake ffmpeg that writes progress to the
// pipe Encode gives it, as fd 3.
func TestEncodeProgress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'out_time_us=5000000\\nprogress=continue\\nout_time_us=10000000\\nprogress=end\\n' >&3\n"
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
