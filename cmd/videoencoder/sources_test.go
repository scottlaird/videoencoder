package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scottlaird/videoencoder/internal/plan"
)

func TestCopySources(t *testing.T) {
	dir := t.TempDir()
	stream := filepath.Join(dir, "stream")
	if err := os.Mkdir(stream, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "clip.mov")
	if err := os.WriteFile(src, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	sources := []plan.Source{{Label: "sdr", Path: src}}

	got, err := copySources(stream, sources)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"clip.mov"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("copySources = %q, want %q", got, want)
	}
	dst := filepath.Join(stream, "clip.mov")
	if b, _ := os.ReadFile(dst); string(b) != "original" {
		t.Errorf("copy holds %q, want %q", b, "original")
	}

	// A copy matching the source's size and time is left alone; mark it
	// to tell.
	st, _ := os.Stat(src)
	if err := os.WriteFile(dst, []byte("marked!!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dst, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := copySources(stream, sources); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "marked!!" {
		t.Errorf("an up-to-date copy was rewritten: %q", b)
	}
	if b, _ := os.ReadFile(src); string(b) != "original" {
		t.Errorf("changing the copy changed the source: %q", b)
	}

	// A changed source is copied again.
	if err := os.WriteFile(src, []byte("re-exported"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := st.ModTime().Add(time.Minute)
	if err := os.Chtimes(src, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := copySources(stream, sources); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "re-exported" {
		t.Errorf("after the source changed, the copy holds %q", b)
	}

	// Two sources with one name would overwrite each other.
	other := filepath.Join(dir, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "clip.mov"), []byte("hdr"), 0o644); err != nil {
		t.Fatal(err)
	}
	both := append(sources, plan.Source{Label: "hdr", Path: filepath.Join(other, "clip.mov")})
	if _, err := copySources(stream, both); err == nil || !strings.Contains(err.Error(), "both named") {
		t.Errorf("same-named sources: %v, want an error", err)
	}
}

// TestCopySourcesEndToEnd renders with copy_sources, checks that the
// source is in the stream and published, and that turning it off drops
// it from the stream.
func TestCopySourcesEndToEnd(t *testing.T) {
	requireTools(t, "ffmpeg", "ffprobe", "packager")
	dir := t.TempDir()
	makeClip(t, filepath.Join(dir, "clip.mov"))
	writeSidecar := func(copySources bool) {
		t.Helper()
		content := `
sources: {main: clip.mov}
renditions: {h264-480: null, h264-720: null, h265-480: null, h265-720: null, h265-1080: null,
  h265-1440: null, h265-2160: null, av1-480: null, av1-720: null, av1-1440: null,
  av1-2160: null, av1-3240: null, av1-4320: null, av1-1080: {quality: 35}}
publish:
  to: [www]
  targets: {www: {dir: "www/{name}"}}
copy_sources: ` + map[bool]string{true: "true", false: "false"}[copySources] + "\n"
		if err := os.WriteFile(filepath.Join(dir, "clip.video.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSidecar(true)
	o, err := parseArgs([]string{"--config", "", dir}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := testDeps(io.Discard)
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("render: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(dir, "clip.mov"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "stream", "clip", "clip.mov"), filepath.Join(dir, "www", "clip", "clip.mov")} {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("source not copied: %v", err)
		} else if string(got) != string(want) {
			t.Errorf("%s differs from the source", p)
		}
	}

	writeSidecar(false)
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("second render: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stream", "clip", "clip.mov")); err == nil {
		t.Error("with copy_sources off, the source is still in the stream")
	}
	if _, err := os.Stat(filepath.Join(dir, "clip.mov")); err != nil {
		t.Errorf("dropping the copy removed the source: %v", err)
	}
}
