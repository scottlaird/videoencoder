package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/scottlaird/videoencoder/internal/ffmpeg"
	"github.com/scottlaird/videoencoder/internal/probe"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		args    []string
		want    options
		wantErr bool
	}{
		{
			args: []string{"a.mov"},
			want: options{outdir: "transcodes", streamdir: "stream", sources: []string{"a.mov"}},
		},
		{
			args: []string{"--outdir", "t", "--streamdir", "s", "--thumbnails", "hdr.mov", "sdr.mov"},
			want: options{outdir: "t", streamdir: "s", thumbnails: true, sources: []string{"hdr.mov", "sdr.mov"}},
		},
		{args: nil, wantErr: true},
		{args: []string{"--nope", "a.mov"}, wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseArgs(tt.args, io.Discard)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseArgs(%q) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			continue
		}
		if tt.wantErr {
			continue
		}
		if got.outdir != tt.want.outdir || got.streamdir != tt.want.streamdir ||
			got.thumbnails != tt.want.thumbnails || !slices.Equal(got.sources, tt.want.sources) {
			t.Errorf("parseArgs(%q) = %+v, want %+v", tt.args, got, tt.want)
		}
	}
}

// countingRunner passes commands through to a real runner and counts
// them by program name.
type countingRunner struct {
	next  ffmpeg.Runner
	calls map[string]int
}

func (c *countingRunner) Run(ctx context.Context, argv []string) error {
	c.calls[argv[0]]++
	return c.next.Run(ctx, argv)
}

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
}

// TestRunEndToEnd encodes a two-second synthetic clip, then runs again
// to check that nothing is re-encoded, then changes one thing.
func TestRunEndToEnd(t *testing.T) {
	requireTools(t, "ffmpeg", "ffprobe", "packager")

	dir := t.TempDir()
	src := filepath.Join(dir, "src.mov")
	gen := exec.Command("ffmpeg", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generating clip: %v\n%s", err, out)
	}

	// Output directories don't exist yet; run must create them.
	o := options{
		outdir:    filepath.Join(dir, "work", "transcodes"),
		streamdir: filepath.Join(dir, "work", "stream"),
		sources:   []string{src},
	}
	runner := &countingRunner{next: ffmpeg.ExecRunner{Stderr: io.Discard}, calls: map[string]int{}}
	d := deps{runner: runner, prober: probe.FFProbe{}}

	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// One batched video encode, audio, poster.
	if got := runner.calls["ffmpeg"]; got != 3 {
		t.Errorf("first run: %d ffmpeg calls, want 3", got)
	}
	for _, name := range []string{"dash.mpd", "hls.m3u8", "thumbnail.jpg", "audio.mp4"} {
		if _, err := os.Stat(filepath.Join(o.streamdir, name)); err != nil {
			t.Errorf("stream output %s: %v", name, err)
		}
	}
	mpd, err := os.ReadFile(filepath.Join(o.streamdir, "dash.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mpd), `label="SDR"`) && !strings.Contains(string(mpd), "<Label>SDR</Label>") {
		t.Errorf("dash.mpd has no SDR label:\n%s", mpd)
	}

	runner.calls = map[string]int{}
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := runner.calls["ffmpeg"]; got != 0 {
		t.Errorf("second run: %d ffmpeg calls, want 0", got)
	}

	// Deleting one output (leaving its status file) rebuilds just that one.
	matches, _ := filepath.Glob(filepath.Join(o.outdir, "video_0_av1_*.mp4"))
	if len(matches) != 1 {
		t.Fatalf("found av1 outputs %q, want exactly one", matches)
	}
	if err := os.Remove(matches[0]); err != nil {
		t.Fatal(err)
	}
	runner.calls = map[string]int{}
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if got := runner.calls["ffmpeg"]; got != 1 {
		t.Errorf("third run: %d ffmpeg calls, want 1", got)
	}
	if _, err := os.Stat(matches[0]); err != nil {
		t.Errorf("deleted output was not rebuilt: %v", err)
	}
}

func TestRunMissingSource(t *testing.T) {
	requireTools(t, "ffprobe")
	o := options{outdir: t.TempDir(), streamdir: t.TempDir(), sources: []string{"/nonexistent.mov"}}
	err := run(context.Background(), o, deps{runner: ffmpeg.ExecRunner{}, prober: probe.FFProbe{}})
	if err == nil {
		t.Fatal("run() with a missing source: got nil error, want one")
	}
	if !strings.Contains(err.Error(), "/nonexistent.mov") {
		t.Errorf("error %q doesn't name the source", err)
	}
}
