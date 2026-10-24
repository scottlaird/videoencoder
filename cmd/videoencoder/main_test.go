package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/scottlaird/videoencoder/internal/ffmpeg"
	"github.com/scottlaird/videoencoder/internal/probe"
)

func TestForced(t *testing.T) {
	tests := []struct {
		glob, slot string
		want       bool
	}{
		{"", "main/av1-1080", false},
		{"*", "main/av1-1080", true},
		{"*", "audio", true},
		{"av1-*", "hdr/av1-1080", true},
		{"av1-*", "hdr/h264-1080", false},
		{"sdr/*", "hdr/av1-1080", false},
		{"sdr/*", "sdr/av1-1080", true},
		{"poster", "poster", true},
	}
	for _, tt := range tests {
		if got := forced(tt.glob, &ffmpeg.Job{Slot: tt.slot}); got != tt.want {
			t.Errorf("forced(%q, %q) = %v, want %v", tt.glob, tt.slot, got, tt.want)
		}
	}
}

func TestParseArgs(t *testing.T) {
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	tests := []struct {
		args      []string
		mode      string
		paths     []string
		overrides map[string]any
		wantErr   bool
	}{
		{args: nil, mode: "render", paths: []string{"."}, overrides: map[string]any{}},
		{args: []string{"plan"}, mode: "plan", paths: []string{"."}, overrides: map[string]any{}},
		{args: []string{"publish", "--to", "a, b"}, mode: "publish", paths: []string{"."}, overrides: map[string]any{"publish": map[string]any{"to": []string{"a", "b"}}}},
		{
			args:      []string{"--outdir", "t", "--thumbnails", "hdr.mov", "sdr.mov"},
			mode:      "render",
			paths:     []string{"hdr.mov", "sdr.mov"},
			overrides: map[string]any{"outdir": abs("t"), "thumbnails": map[string]any{"enabled": true}},
		},
		{
			args:      []string{"plan", "--streamdir", "/s", "proj"},
			mode:      "plan",
			paths:     []string{"proj"},
			overrides: map[string]any{"streamdir": "/s"},
		},
		{args: []string{"--nope"}, wantErr: true},
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
		if got.mode != tt.mode || !slices.Equal(got.paths, tt.paths) || len(got.overrides) != len(tt.overrides) {
			t.Errorf("parseArgs(%q) = %+v", tt.args, got)
			continue
		}
		for k, v := range tt.overrides {
			if fmt.Sprint(got.overrides[k]) != fmt.Sprint(v) {
				t.Errorf("parseArgs(%q) override %s = %v, want %v", tt.args, k, got.overrides[k], v)
			}
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

// makeClip writes a two-second 320x240 SDR test clip with audio.
func makeClip(t *testing.T, path string) {
	t.Helper()
	gen := exec.Command("ffmpeg", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", path)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generating clip: %v\n%s", err, out)
	}
}

func testDeps(stdout io.Writer) (deps, *countingRunner) {
	r := &countingRunner{next: ffmpeg.ExecRunner{Stderr: io.Discard}, calls: map[string]int{}}
	return deps{runner: r, prober: probe.FFProbe{}, stdout: stdout}, r
}

// TestAdHocEndToEnd encodes a clip given directly on the command line,
// then runs again to check that nothing is re-encoded, then deletes one
// output and checks that only it is rebuilt.
func TestAdHocEndToEnd(t *testing.T) {
	requireTools(t, "ffmpeg", "ffprobe", "packager")
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mov")
	makeClip(t, src)

	o, err := parseArgs([]string{"--config", "", src}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	d, runner := testDeps(io.Discard)
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// One batched video encode, audio, one batch of posters.
	if got := runner.calls["ffmpeg"]; got != 3 {
		t.Errorf("first run: %d ffmpeg calls, want 3", got)
	}
	// Defaults put outputs beside the source, under the video's name.
	streamdir := filepath.Join(dir, "stream", "src")
	for _, pattern := range []string{"dash.mpd", "hls.m3u8", "thumbnail.jpg", "poster.json",
		"poster-sdr-320.*.avif", "poster-sdr-320.*.jpg", "audio.*.mp4", "audio.*.m3u8",
		"thumbnails.m3u8", "thumbnails.vtt", "thumbs.*/tile-1.jpg"} {
		if m, _ := filepath.Glob(filepath.Join(streamdir, pattern)); len(m) != 1 {
			t.Errorf("stream outputs matching %s: %q, want one", pattern, m)
		}
	}
	mpd, err := os.ReadFile(filepath.Join(streamdir, "dash.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mpd), "SDR") || !strings.Contains(string(mpd), "thumbnail_tile") {
		t.Errorf("dash.mpd lacks the SDR label or the thumbnail track:\n%s", mpd)
	}

	before := hashMedia(t, streamdir)
	runner.calls = map[string]int{}
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := runner.calls["ffmpeg"]; got != 0 {
		t.Errorf("second run: %d ffmpeg calls, want 0", got)
	}
	// Repackaging unchanged inputs gives byte-identical media, so a
	// sync to object storage can skip it.
	if after := hashMedia(t, streamdir); !maps.Equal(before, after) {
		t.Errorf("repackaged media changed:\n before %v\n after  %v", before, after)
	}

	// Deleting one output (leaving its status file) rebuilds just that one.
	matches, _ := filepath.Glob(filepath.Join(dir, "transcodes", "src", "av1-*.0.*.mp4"))
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
}

// TestConfiguredEndToEnd renders a directory of configured videos with
// no flags, then changes one rendition's quality in a sidecar and checks
// that plan reports, and render rebuilds, only that rendition.
func TestConfiguredEndToEnd(t *testing.T) {
	requireTools(t, "ffmpeg", "ffprobe", "packager")
	dir := t.TempDir()
	makeClip(t, filepath.Join(dir, "clip.mov"))
	writeFile := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The project trims the ladder to two renditions; the video entry
	// names the source.
	writeFile("videoencoder.yaml", `
renditions:
  h264-480: null
  h264-720: null
  h264-1080: {quality: 23}
  h265-480: null
  h265-720: null
  h265-1080: null
  h265-1440: null
  h265-2160: null
  av1-480: null
  av1-720: null
  av1-1440: null
  av1-2160: null
  av1-3240: null
  av1-4320: null
videos:
  clip:
    sources: {main: clip.mov}
`)

	o, err := parseArgs([]string{"--config", "", dir}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	d, runner := testDeps(io.Discard)
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("first run: %v", err)
	}
	transcodes := filepath.Join(dir, "transcodes", "clip")
	stream := filepath.Join(dir, "stream", "clip")
	encoded, _ := filepath.Glob(filepath.Join(transcodes, "*.main.*.mp4"))
	if len(encoded) != 2 {
		t.Errorf("encoded %q, want an h264 and an av1 rendition", encoded)
	}
	oldAV1, _ := filepath.Glob(filepath.Join(transcodes, "av1-1080.main.*.mp4"))

	writeFile("clip.video.yaml", "renditions: {av1-1080: {quality: 40}}\n")

	var out bytes.Buffer
	o.mode = "plan"
	d.stdout = &out
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("plan: %v", err)
	}
	plan := out.String()
	for _, want := range []string{
		"quality: 40 # " + filepath.Join(dir, "clip.video.yaml"),
		"quality: 23 # " + filepath.Join(dir, "videoencoder.yaml"),
		"main/av1-1080 320x240 -> av1-1080.main.",
		".mp4: build: settings changed",
		"main/h264-1080 320x240 -> h264-1080.main.",
		".mp4: up to date",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan output lacks %q:\n%s", want, plan)
		}
	}

	o.mode = "render"
	runner.calls = map[string]int{}
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := runner.calls["ffmpeg"]; got != 1 {
		t.Errorf("after changing one rendition: %d ffmpeg calls, want 1", got)
	}
	newAV1, _ := filepath.Glob(filepath.Join(transcodes, "av1-1080.main.*.mp4"))
	if len(oldAV1) != 1 || len(newAV1) != 1 || oldAV1[0] == newAV1[0] {
		t.Fatalf("av1 outputs before %q, after %q; want one each, differently named", oldAV1, newAV1)
	}
	if _, err := os.Stat(filepath.Join(stream, filepath.Base(oldAV1[0]))); err == nil {
		t.Error("the replaced rendition is still in the stream directory")
	}
	if _, err := os.Stat(filepath.Join(stream, filepath.Base(newAV1[0]))); err != nil {
		t.Errorf("the new rendition isn't in the stream directory: %v", err)
	}

	// --rebuild forces one rendition despite being up to date.
	runner.calls = map[string]int{}
	forced := o
	forced.rebuild = "h264-*"
	if err := run(context.Background(), forced, d); err != nil {
		t.Fatalf("forced run: %v", err)
	}
	if got := runner.calls["ffmpeg"]; got != 1 {
		t.Errorf("--rebuild 'h264-*': %d ffmpeg calls, want 1", got)
	}

	// Dropping a rendition deletes its files.
	writeFile("clip.video.yaml", "renditions: {av1-1080: null}\n")
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("third run: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(transcodes, "av1-*")); len(left) != 0 {
		t.Errorf("dropped rendition left %q", left)
	}
	if left, _ := filepath.Glob(filepath.Join(stream, "av1-*")); len(left) != 0 {
		t.Errorf("dropped rendition left %q in the stream", left)
	}
}

// hashMedia returns the SHA-256 of every .mp4 in dir, by name.
func hashMedia(t *testing.T, dir string) map[string]string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.mp4"))
	if len(files) == 0 {
		t.Fatalf("no media in %s", dir)
	}
	sums := map[string]string{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sums[filepath.Base(f)] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	return sums
}

// makeHDRClip writes a two-second 320x240 PQ-tagged 10-bit clip.
func makeHDRClip(t *testing.T, path string) {
	t.Helper()
	gen := exec.Command("ffmpeg", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-vf", "setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc,format=yuv420p10le",
		"-c:v", "libx265", "-x265-params", "log-level=error",
		"-c:a", "aac", "-shortest", path)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generating HDR clip: %v\n%s", err, out)
	}
}

// TestHDRAndSDRPosters renders an HDR+SDR pair and checks that both
// tones get posters, with HDR as AVIF only, listed in poster.json.
func TestHDRAndSDRPosters(t *testing.T) {
	requireTools(t, "ffmpeg", "ffprobe", "packager", "avifgainmaputil")
	dir := t.TempDir()
	makeHDRClip(t, filepath.Join(dir, "trip-hdr.mov"))
	makeClip(t, filepath.Join(dir, "trip-sdr.mov"))
	if err := os.WriteFile(filepath.Join(dir, "trip.video.yaml"), []byte(`
sources: {hdr: trip-hdr.mov, sdr: trip-sdr.mov}
poster: {offset: "0.5", widths: [160, 320], formats: [avif, jpg, webp]}
renditions: {h264-480: null, h264-720: null, h265-480: null, h265-720: null, h265-1080: null,
  h265-1440: null, h265-2160: null, av1-480: null, av1-720: null, av1-1080: null, av1-1440: null,
  av1-2160: null, av1-3240: null, av1-4320: null}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	o, err := parseArgs([]string{"--config", "", dir}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := testDeps(io.Discard)
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "stream", "trip", "poster.json"))
	if err != nil {
		t.Fatal(err)
	}
	var idx posterIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, im := range idx.Images {
		got = append(got, fmt.Sprintf("%s %d %s", im.Tone, im.Width, im.Format))
		if _, err := os.Stat(filepath.Join(dir, "stream", "trip", im.Src)); err != nil {
			t.Errorf("poster.json lists %s, which isn't in the stream: %v", im.Src, err)
		}
	}
	want := []string{
		"gainmap 160 jpg", "gainmap 160 avif", "gainmap 320 jpg", "gainmap 320 avif",
		"SDR 160 avif", "SDR 160 jpg", "SDR 160 webp", "SDR 320 avif", "SDR 320 jpg", "SDR 320 webp",
		"HDR 160 avif", "HDR 320 avif",
	}
	if !slices.Equal(got, want) {
		t.Errorf("poster.json images = %q, want %q", got, want)
	}
	if idx.Offset != "0.5" {
		t.Errorf("poster.json offset = %q", idx.Offset)
	}

	hdrPosters, _ := filepath.Glob(filepath.Join(dir, "stream", "trip", "poster-hdr-320.*.avif"))
	if len(hdrPosters) != 1 {
		t.Fatalf("HDR posters %q, want one", hdrPosters)
	}
	vd, err := probe.FFProbe{}.Probe(context.Background(), hdrPosters[0])
	if err != nil {
		t.Fatal(err)
	}
	if vd.Tone() != probe.HDR {
		t.Errorf("HDR poster has transfer %q, want PQ", vd.ColorTransfer)
	}

	// The reference decoders recognise the gain-mapped posters.
	gmJPEG, _ := filepath.Glob(filepath.Join(dir, "stream", "trip", "poster-gainmap-320.*.jpg"))
	gmAVIF, _ := filepath.Glob(filepath.Join(dir, "stream", "trip", "poster-gainmap-320.*.avif"))
	if len(gmJPEG) != 1 || len(gmAVIF) != 1 {
		t.Fatalf("gain-mapped posters %q %q, want one of each", gmJPEG, gmAVIF)
	}
	if app, err := exec.LookPath("ultrahdr_app"); err == nil {
		out, err := exec.Command(app, "-m", "1", "-P", "-j", gmJPEG[0]).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "Ultra HDR Image: Yes") {
			t.Errorf("ultrahdr_app doesn't recognise %s: %v\n%s", gmJPEG[0], err, out)
		}
	}
	out, err := exec.Command("avifgainmaputil", "printmetadata", gmAVIF[0]).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Alternate headroom") {
		t.Errorf("avifgainmaputil printmetadata %s: %v\n%s", gmAVIF[0], err, out)
	}
}

// TestPublishDir renders into a directory target, then republishes
// after a change and checks that only the change is copied and --prune
// removes what it replaced.
func TestPublishDir(t *testing.T) {
	requireTools(t, "ffmpeg", "ffprobe", "packager")
	dir := t.TempDir()
	makeClip(t, filepath.Join(dir, "clip.mov"))
	www := filepath.Join(dir, "www")
	writeSidecar := func(quality int) {
		t.Helper()
		content := fmt.Sprintf(`
sources: {main: clip.mov}
renditions: {h264-480: null, h264-720: null, h265-480: null, h265-720: null, h265-1080: null,
  h265-1440: null, h265-2160: null, av1-480: null, av1-720: null, av1-1440: null,
  av1-2160: null, av1-3240: null, av1-4320: null, av1-1080: {quality: %d}}
publish:
  to: [www]
  targets: {www: {dir: "www/{name}"}, shared: {dir: www}}
`, quality)
		if err := os.WriteFile(filepath.Join(dir, "clip.video.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSidecar(35)
	o, err := parseArgs([]string{"--config", "", dir}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := testDeps(io.Discard)
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("render: %v", err)
	}
	pub := filepath.Join(www, "clip")
	for _, pattern := range []string{"dash.mpd", "hls.m3u8", "poster.json", "av1-1080.main.*.mp4", "thumbs.*/tile-1.jpg"} {
		if m, _ := filepath.Glob(filepath.Join(pub, pattern)); len(m) != 1 {
			t.Errorf("published %s: %q", pattern, m)
		}
	}
	old, _ := filepath.Glob(filepath.Join(pub, "av1-1080.main.*.mp4"))

	writeSidecar(36)
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("second render: %v", err)
	}
	if _, err := os.Stat(old[0]); err != nil {
		t.Error("without --prune, the replaced rendition should stay published")
	}
	o.mode, o.prune = "publish", true
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("publish --prune: %v", err)
	}
	if _, err := os.Stat(old[0]); err == nil {
		t.Error("--prune left the replaced rendition")
	}
	if m, _ := filepath.Glob(filepath.Join(pub, "av1-1080.main.*.mp4")); len(m) != 1 {
		t.Errorf("after prune: %q", m)
	}

	// Pruning a target without {name} is refused.
	o.overrides["publish"] = map[string]any{"to": []string{"shared"}}
	if err := run(context.Background(), o, d); err == nil || !strings.Contains(err.Error(), "{name}") {
		t.Errorf("prune to a shared target: %v, want a refusal", err)
	}
}

// TestSilentSource renders a video with no audio track.
func TestSilentSource(t *testing.T) {
	requireTools(t, "ffmpeg", "ffprobe", "packager")
	dir := t.TempDir()
	src := filepath.Join(dir, "timelapse.mov")
	gen := exec.Command("ffmpeg", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=2",
		"-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generating clip: %v\n%s", err, out)
	}
	o, err := parseArgs([]string{"--config", "", src}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := testDeps(io.Discard)
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("run: %v", err)
	}
	stream := filepath.Join(dir, "stream", "timelapse")
	if _, err := os.Stat(filepath.Join(stream, "dash.mpd")); err != nil {
		t.Error(err)
	}
	if m, _ := filepath.Glob(filepath.Join(stream, "audio.*")); len(m) != 0 {
		t.Errorf("silent video has audio outputs %q", m)
	}
}

func TestRunMissingSource(t *testing.T) {
	requireTools(t, "ffprobe")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "v.video.yaml"), []byte("sources: {main: nonexistent.mov}"), 0o644); err != nil {
		t.Fatal(err)
	}
	o, err := parseArgs([]string{"--config", "", dir}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := testDeps(io.Discard)
	err = run(context.Background(), o, d)
	if err == nil || !strings.Contains(err.Error(), "nonexistent.mov") {
		t.Errorf("run() with a missing source = %v, want an error naming it", err)
	}
}
