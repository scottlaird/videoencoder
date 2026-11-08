package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/scottlaird/videoencoder/internal/bench"
	"github.com/scottlaird/videoencoder/internal/config"
	"github.com/scottlaird/videoencoder/internal/ffmpeg"
	"github.com/scottlaird/videoencoder/internal/probe"
	"github.com/scottlaird/videoencoder/internal/progress"
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
		{args: []string{"tune", "--jobs", "6"}, mode: "tune", paths: []string{"."}, overrides: map[string]any{"tune": map[string]any{"jobs": 6}}},
		{args: []string{"--nope"}, wantErr: true},
		{args: []string{"plan", "--only", "av1-*"}, mode: "plan", paths: []string{"."}, overrides: map[string]any{}},
		{args: []string{"tune", "--only", "av1-*"}, wantErr: true},
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
	return deps{runner: r, exec: bench.ExecExecutor{Stderr: io.Discard}, prober: probe.FFProbe{}, stdout: stdout}, r
}

// recorder keeps the progress events a run reports.
type recorder struct {
	mu     sync.Mutex
	events []progress.Event
}

func (r *recorder) Report(e progress.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// state is what the events say about video at the end.
func (r *recorder) state(video string) progress.Video {
	var t progress.Tracker
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		t.Apply(e)
	}
	for _, v := range t.Snapshot() {
		if v.Name == video {
			return v
		}
	}
	return progress.Video{}
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
	rec := &recorder{}
	if err := run(progress.WithReporter(context.Background(), rec), o, d); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// The run reports the video and every step, all finished, with the
	// encodes queued before any started.
	v := rec.state("src")
	if v.Status != progress.Done || v.Started.IsZero() || v.Ended.IsZero() {
		t.Errorf("video state = %+v, want done with start and end times", v)
	}
	tasks := map[string]progress.Status{}
	for _, task := range v.Tasks {
		tasks[task.Name] = task.Status
	}
	for _, name := range []string{"0/av1-480", "0/h264-480", "0/h265-480", "thumbs", "audio", "posters", "package"} {
		if tasks[name] != progress.Done {
			t.Errorf("task %s = %v, want done (all tasks: %v)", name, tasks[name], tasks)
		}
	}
	firstStart := slices.IndexFunc(rec.events, func(e progress.Event) bool { return e.Kind == progress.TaskStarted })
	lastQueue := -1
	for i, e := range rec.events {
		if e.Kind == progress.TaskQueued {
			lastQueue = i
		}
	}
	if firstStart < lastQueue {
		t.Errorf("a task started (event %d) before the last was queued (event %d)", firstStart, lastQueue)
	}
	// One batched video encode, audio, one batch of posters.
	if got := runner.calls["ffmpeg"]; got != 3 {
		t.Errorf("first run: %d ffmpeg calls, want 3", got)
	}
	// Defaults put outputs beside the source, under the video's name.
	streamdir := filepath.Join(dir, "stream", "src")
	for _, pattern := range []string{"dash.mpd", "hls.m3u8", "thumbnail.jpg", "poster.json",
		"poster-sdr-320.*.avif", "poster-sdr-320.*.jpg", "audio.*.mp4", "audio.*.m3u8",
		"poster-320.jpg", "poster-320.avif", "poster.jpg", "poster.avif",
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
	rec = &recorder{}
	if err := run(progress.WithReporter(context.Background(), rec), o, d); err != nil {
		t.Fatalf("second run: %v", err)
	}
	for _, task := range rec.state("src").Tasks {
		if strings.HasPrefix(task.Name, "0/") && (task.Status != progress.Skipped || task.Detail != "up to date") {
			t.Errorf("second run: task %s = %+v, want skipped as up to date", task.Name, task)
		}
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
poster: {offset: "0.5", widths: [160], formats: [avif, jpg, webp], hdr: true}
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

	// Fixed names point at the best poster: gain-mapped where there is
	// one, SDR for WebP, never the plain HDR AVIF.
	aliases := map[string]string{}
	for _, im := range idx.Images {
		for _, a := range im.Aliases {
			aliases[a] = im.Tone + " " + im.Format
		}
	}
	wantAliases := map[string]string{
		"poster-160.jpg": "gainmap jpg", "poster-160.avif": "gainmap avif", "poster-160.webp": "SDR webp",
		"poster-320.jpg": "gainmap jpg", "poster-320.avif": "gainmap avif", "poster-320.webp": "SDR webp",
		"poster.jpg": "gainmap jpg", "poster.avif": "gainmap avif", "poster.webp": "SDR webp",
	}
	if !maps.Equal(aliases, wantAliases) {
		t.Errorf("aliases = %v, want %v", aliases, wantAliases)
	}
	for a := range wantAliases {
		if _, err := os.Stat(filepath.Join(dir, "stream", "trip", a)); err != nil {
			t.Errorf("alias %s: %v", a, err)
		}
	}

	// The reference decoders recognize the gain-mapped posters.
	gmJPEG := []string{filepath.Join(dir, "stream", "trip", "poster.jpg")}
	gmAVIF := []string{filepath.Join(dir, "stream", "trip", "poster.avif")}
	if app, err := exec.LookPath("ultrahdr_app"); err == nil {
		out, err := exec.Command(app, "-m", "1", "-P", "-j", gmJPEG[0]).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "Ultra HDR Image: Yes") {
			t.Errorf("ultrahdr_app doesn't recognize %s: %v\n%s", gmJPEG[0], err, out)
		}
	}
	out, err := exec.Command("avifgainmaputil", "printmetadata", gmAVIF[0]).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Alternate headroom") {
		t.Errorf("avifgainmaputil printmetadata %s: %v\n%s", gmAVIF[0], err, out)
	}
	// Its base is sRGB, as the JPEG's is, not the BT.709 transfer in the
	// PNG ffmpeg hands avifgainmaputil.
	vd, err = probe.FFProbe{}.Probe(context.Background(), gmAVIF[0])
	if err != nil {
		t.Fatal(err)
	}
	if vd.ColorTransfer != "iec61966-2-1" {
		t.Errorf("gain-mapped AVIF base has transfer %q, want sRGB (iec61966-2-1)", vd.ColorTransfer)
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

// TestTuneEndToEnd tunes an ad hoc clip with real ffmpeg and libvmaf
// and checks that the sidecar it writes is used by the next render.
func TestTuneEndToEnd(t *testing.T) {
	requireTools(t, "ffmpeg", "ffprobe", "packager")
	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mov")
	makeClip(t, src)
	// Two av1 renditions, so one anchor each end and nothing between.
	if err := os.WriteFile(filepath.Join(dir, "videoencoder.yaml"), []byte(`
tune: {ranges: [{start: 0s, duration: 1s}, {start: 1s, duration: 1s}], target_vmaf: 90}
renditions: {h264-480: null, h264-720: null, h265-480: null, h265-720: null, h265-1080: null,
  h265-1440: null, h265-2160: null, av1-720: null, av1-1080: null, av1-1440: null, av1-2160: null,
  av1-3240: null, av1-4320: null,
  h264-180-capped: {encoder: h264, height: 180, quality: 30, bitrate: 300k, tune: false},
  av1-180: {encoder: av1, height: 180, quality_from: av1-480, quality_offset: -2}}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	o, err := parseArgs([]string{"tune", "--config", "", src}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	d, _ := testDeps(&out)
	rec := &recorder{}
	if err := run(progress.WithReporter(context.Background(), rec), o, d); err != nil {
		t.Fatalf("tune: %v\n%s", err, out.String())
	}
	// Searches report each trial and finish with their result; kept and
	// following renditions are skipped with the reason.
	tuned := map[string]progress.Task{}
	for _, task := range rec.state("clip").Tasks {
		tuned[task.Name] = task
	}
	if task := tuned["sdr/av1-480"]; task.Status != progress.Done || !strings.Contains(task.Detail, "crf ") || !strings.Contains(task.Detail, "trials") {
		t.Errorf("sdr/av1-480 = %+v, want done with its crf and trial count", task)
	}
	if task := tuned["sdr/h264-180-capped"]; task.Status != progress.Skipped || !strings.Contains(task.Detail, "tune: false") {
		t.Errorf("sdr/h264-180-capped = %+v, want skipped for tune: false", task)
	}
	if !slices.ContainsFunc(rec.events, func(e progress.Event) bool {
		return e.Kind == progress.TaskUpdated && e.Task == "sdr/av1-480" && strings.Contains(e.Detail, "trial 1:")
	}) {
		t.Error("no trial updates reported for sdr/av1-480")
	}
	sidecar, err := os.ReadFile(filepath.Join(dir, "clip"+config.SidecarSuffix))
	if err != nil {
		t.Fatalf("no sidecar: %v\n%s", err, out.String())
	}
	for _, want := range []string{"sources:", "av1-480:", "h264-1080:", "bitrate:", "tuned "} {
		if !strings.Contains(string(sidecar), want) {
			t.Errorf("sidecar lacks %q:\n%s", want, sidecar)
		}
	}
	// A rendition with tune: false is reported but left alone.
	if strings.Contains(string(sidecar), "h264-180-capped") {
		t.Errorf("sidecar tunes h264-180-capped despite tune: false:\n%s", sidecar)
	}
	if !strings.Contains(out.String(), "tuned on 1s from 0s and 1s from 1s of ") {
		t.Errorf("tune output doesn't name both ranges:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "h264-180-capped quality 30    kept: tune: false") {
		t.Errorf("tune output doesn't report the kept rendition:\n%s", out.String())
	}
	// One with quality_from follows the tuned value instead.
	if strings.Contains(string(sidecar), "av1-180") {
		t.Errorf("sidecar tunes av1-180 despite quality_from:\n%s", sidecar)
	}
	if !strings.Contains(out.String(), "av1-180    follows av1-480 - 2") {
		t.Errorf("tune output doesn't report the following rendition:\n%s", out.String())
	}
	// The tuned values are what the next render uses.
	o.mode = "plan"
	out.Reset()
	if err := run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "clip.video.yaml") {
		t.Errorf("plan doesn't show values from the tuned sidecar:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "# av1-480 - 2") {
		t.Errorf("plan doesn't show where av1-180's quality comes from:\n%s", out.String())
	}
}

// TestPortablePixelFormats renders a 10-bit 4:2:2 source, like ProRes
// from an editor, and checks that every rendition is 4:2:0 in a format
// browsers decode.
func TestPortablePixelFormats(t *testing.T) {
	requireTools(t, "ffmpeg", "ffprobe", "packager")
	dir := t.TempDir()
	src := filepath.Join(dir, "edit.mov")
	gen := exec.Command("ffmpeg", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=2",
		"-f", "lavfi", "-i", "sine=duration=2",
		"-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709",
		"-c:v", "prores_ks", "-profile:v", "3", "-pix_fmt", "yuv422p10le", "-c:a", "aac", "-shortest", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("generating clip: %v\n%s", err, out)
	}
	o, err := parseArgs([]string{"--config", "", "--thumbnails=false", src}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := testDeps(io.Discard)
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := map[string]string{"h264": "yuv420p", "h265": "yuv420p10le", "av1": "yuv420p10le"}
	for codec, pixFmt := range want {
		files, _ := filepath.Glob(filepath.Join(dir, "stream", "edit", codec+"-*.mp4"))
		if len(files) == 0 {
			t.Errorf("no %s rendition", codec)
		}
		for _, f := range files {
			out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v",
				"-show_entries", "stream=pix_fmt", "-of", "csv=p=0", f).Output()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(string(out)), ",")); got != pixFmt {
				t.Errorf("%s: pixel format %s, want %s", filepath.Base(f), got, pixFmt)
			}
		}
	}
}

// TestSDROnlyH264 checks that H.264 is never PQ: an HDR+SDR pair gets
// H.264 from the SDR source only, and an HDR-only video gets it
// tone-mapped to BT.709.
func TestSDROnlyH264(t *testing.T) {
	requireTools(t, "ffmpeg", "ffprobe", "packager")
	ladder := `
poster: {offset: "0.5", widths: [160], gainmap_formats: []}
thumbnails: {enabled: false}
renditions: {h264-480: null, h264-720: null, h265-480: null, h265-720: null, h265-1080: null,
  h265-1440: null, h265-2160: null, av1-480: null, av1-720: null, av1-1440: null,
  av1-2160: null, av1-3240: null, av1-4320: null}
`
	transfer := func(path string) string {
		out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v",
			"-show_entries", "stream=color_transfer", "-of", "csv=p=0", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Trim(strings.TrimSpace(string(out)), ",")
	}
	for _, tc := range []struct {
		name    string
		sources string
		want    map[string]string // rendition glob -> transfer; "" means absent
	}{
		{"pair", "{hdr: v-hdr.mov, sdr: v-sdr.mov}", map[string]string{
			"h264-1080.sdr.*.mp4": "bt709", "h264-1080.hdr.*.mp4": "", "av1-1080.hdr.*.mp4": "smpte2084"}},
		{"hdr only", "{hdr: v-hdr.mov}", map[string]string{
			"h264-1080.hdr.*.mp4": "bt709", "av1-1080.hdr.*.mp4": "smpte2084"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			makeHDRClip(t, filepath.Join(dir, "v-hdr.mov"))
			makeClip(t, filepath.Join(dir, "v-sdr.mov"))
			if err := os.WriteFile(filepath.Join(dir, "v.video.yaml"), []byte("sources: "+tc.sources+ladder), 0o644); err != nil {
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
			for glob, want := range tc.want {
				m, _ := filepath.Glob(filepath.Join(dir, "stream", "v", glob))
				switch {
				case want == "" && len(m) != 0:
					t.Errorf("%s exists: %q", glob, m)
				case want != "" && len(m) != 1:
					t.Errorf("%s: %q, want one", glob, m)
				case want != "":
					if got := transfer(m[0]); got != want {
						t.Errorf("%s: transfer %s, want %s", glob, got, want)
					}
				}
			}
		})
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

// TestTuneHDRAndSDR checks that a video with HDR and SDR sources has
// its SDR renditions tuned on the SDR source and its HDR ones on the
// HDR source, into hdr.quality, which the next render's HDR encodes use.
func TestTuneHDRAndSDR(t *testing.T) {
	requireTools(t, "ffmpeg", "ffprobe", "packager")
	dir := t.TempDir()
	makeHDRClip(t, filepath.Join(dir, "v-hdr.mov"))
	makeClip(t, filepath.Join(dir, "v-sdr.mov"))
	if err := os.WriteFile(filepath.Join(dir, "v.video.yaml"), []byte(`
sources: {hdr: v-hdr.mov, sdr: v-sdr.mov}
tune: {ranges: [{start: 0s, duration: 1s}], target_vmaf: 90, hdr: {target_vmaf: 85}}
renditions: {h264-720: null, h264-1080: null, h265-480: null, h265-720: null, h265-1080: null,
  h265-1440: null, h265-2160: null, av1-720: null, av1-1080: null, av1-1440: null, av1-2160: null,
  av1-3240: null, av1-4320: null}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	o, err := parseArgs([]string{"tune", "--config", "", dir}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	d, _ := testDeps(&out)
	rec := &recorder{}
	if err := run(progress.WithReporter(context.Background(), rec), o, d); err != nil {
		t.Fatalf("tune: %v\n%s", err, out.String())
	}
	// Each tone's searches are separate tasks, named for the tone.
	tuned := map[string]progress.Status{}
	for _, task := range rec.state("v").Tasks {
		tuned[task.Name] = task.Status
	}
	for _, name := range []string{"sdr/av1-480", "hdr/av1-480", "sdr/h264-480"} {
		if tuned[name] != progress.Done {
			t.Errorf("task %s = %v, want done (tasks: %v)", name, tuned[name], tuned)
		}
	}
	if _, ok := tuned["hdr/h264-480"]; ok {
		t.Errorf("tasks include hdr/h264-480, but H.264 is SDR-only: %v", tuned)
	}
	for _, want := range []string{"tuned on 1s from 0s of sdr", "tuned on 1s from 0s of hdr", "av1-480 hdr", "(target 85)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("tune output lacks %q:\n%s", want, out.String())
		}
	}
	r, err := (&config.Loader{}).Load(config.Video{Name: "v", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	av1, h264 := r.Renditions["av1-480"], r.Renditions["h264-480"]
	if av1.HDR == nil || av1.HDR.Quality == nil {
		t.Fatalf("av1-480 has no tuned HDR quality: %+v", av1)
	}
	if h264.HDR != nil {
		t.Errorf("h264-480, which is SDR only, has HDR settings: %+v", h264.HDR)
	}

	// The render encodes each tone at its own quality.
	b, err := prepare(context.Background(), r, d)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"hdr/av1-480": *av1.HDR.Quality, "sdr/av1-480": av1.Quality, "sdr/h264-480": h264.Quality,
	}
	for _, js := range b.perSource {
		for _, j := range js {
			i := slices.Index(j.Args, "-crf")
			if w, ok := want[j.Slot]; !ok || i < 0 || j.Args[i+1] != fmt.Sprint(w) {
				t.Errorf("%s: args %q, want -crf %v", j.Slot, j.Args, w)
			}
			delete(want, j.Slot)
		}
	}
	if len(want) != 0 {
		t.Errorf("no jobs for %v", slices.Sorted(maps.Keys(want)))
	}
}

// TestOnly checks that --only builds just the matching stale outputs
// and leaves the stream alone, so the rest stay stale for the next full
// render.
func TestOnly(t *testing.T) {
	requireTools(t, "ffmpeg", "ffprobe", "packager")
	dir := t.TempDir()
	makeClip(t, filepath.Join(dir, "v.mov"))
	sidecar := filepath.Join(dir, "v.video.yaml")
	ladder := func(q int) []byte {
		return fmt.Appendf(nil, `
sources: {main: v.mov}
renditions: {h264-720: null, h264-1080: null, h265-480: null, h265-720: null, h265-1080: null,
  h265-1440: null, h265-2160: null, av1-720: null, av1-1080: null, av1-1440: null, av1-2160: null,
  av1-3240: null, av1-4320: null, h264-480: {quality: %d}, av1-480: {quality: %d}}
`, q, q+10)
	}
	if err := os.WriteFile(sidecar, ladder(23), 0o644); err != nil {
		t.Fatal(err)
	}
	o, err := parseArgs([]string{"--config", "", dir}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	d, runner := testDeps(io.Discard)
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("first run: %v", err)
	}
	streamdir := filepath.Join(dir, "stream", "v")
	before := hashMedia(t, streamdir)

	// Change both renditions, then build only one.
	if err := os.WriteFile(sidecar, ladder(25), 0o644); err != nil {
		t.Fatal(err)
	}
	if o, err = parseArgs([]string{"--config", "", "--only", "av1-*", dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	runner.calls = map[string]int{}
	if err := run(context.Background(), o, d); err != nil {
		t.Fatalf("--only run: %v", err)
	}
	if got := runner.calls["ffmpeg"]; got != 1 {
		t.Errorf("--only run: %d ffmpeg calls, want 1", got)
	}
	if got := runner.calls["packager"]; got != 0 {
		t.Errorf("--only run packaged the stream (%d packager calls)", got)
	}
	if after := hashMedia(t, streamdir); !maps.Equal(before, after) {
		t.Errorf("--only changed the stream:\n before %v\n after  %v", before, after)
	}

	// The other rendition is still stale.
	var out bytes.Buffer
	d.stdout = &out
	o.mode, o.only = "plan", ""
	if err := run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"main/h264-480 320x240 -> h264-480.main.", "main/av1-480 320x240 -> av1-480.main."} {
		i := strings.Index(out.String(), want)
		if i < 0 {
			t.Fatalf("plan lacks %q:\n%s", want, out.String())
		}
		line, _, _ := strings.Cut(out.String()[i:], "\n")
		if stale := strings.HasSuffix(line, "settings changed"); stale != strings.Contains(want, "h264") {
			t.Errorf("plan: %s", line)
		}
	}

	// A glob that matches nothing is an error, not a no-op.
	o.mode, o.only = "render", "vp9-*"
	if err := run(context.Background(), o, d); err == nil || !strings.Contains(err.Error(), "matches no outputs") {
		t.Errorf("--only vp9-*: err = %v, want no matches", err)
	}
}

func TestHelp(t *testing.T) {
	tests := []struct {
		args    []string
		want    []string // in the help text
		notWant []string
	}{
		{
			args:    []string{"--help"},
			want:    []string{"Commands:", "plan ", "tune ", "publish ", "--rebuild GLOB", "--prune", `videoencoder help <command>`},
			notWant: []string{"-config string"},
		},
		{args: []string{"help"}, want: []string{"Commands:"}},
		{
			args:    []string{"tune", "--help"},
			want:    []string{"Usage: videoencoder tune", "tune.target_vmaf", "--config FILE", "--jobs N"},
			notWant: []string{"--prune", "--rebuild", "Commands:"},
		},
		{args: []string{"help", "plan"}, want: []string{"Usage: videoencoder plan", "--only GLOB"}, notWant: []string{"--publish"}},
		{args: []string{"help", "publish"}, want: []string{"Usage: videoencoder publish", "--to TARGET,...", "--prune"}},
		{args: []string{"help", "render"}, want: []string{"Usage: videoencoder [flags]", "--publish"}, notWant: []string{"Commands:"}},
	}
	for _, tt := range tests {
		var stderr strings.Builder
		_, err := parseArgs(tt.args, &stderr)
		if !errors.Is(err, flag.ErrHelp) {
			t.Errorf("parseArgs(%q) = %v, want flag.ErrHelp", tt.args, err)
		}
		for _, w := range tt.want {
			if !strings.Contains(stderr.String(), w) {
				t.Errorf("help for %q lacks %q:\n%s", tt.args, w, stderr.String())
			}
		}
		for _, w := range tt.notWant {
			if strings.Contains(stderr.String(), w) {
				t.Errorf("help for %q has %q:\n%s", tt.args, w, stderr.String())
			}
		}
	}
}

func TestFlagsForCommand(t *testing.T) {
	tests := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"tune", "--prune"}, "--prune doesn't apply to tune"},
		{[]string{"publish", "--only", "x"}, "--only doesn't apply to publish"},
		{[]string{"tune", "--publish=false"}, "--publish doesn't apply to tune"},
		{[]string{"plan", "--to", "a"}, "--to doesn't apply to plan"},
		{[]string{"help", "nope"}, `no command "nope"`},
		{[]string{"publish", "--prune", "--to", "a"}, ""},
		{[]string{"plan", "--rebuild", "*"}, ""},
		{[]string{"plan", "--jobs", "4"}, "--jobs doesn't apply to plan"},
	}
	for _, tt := range tests {
		_, err := parseArgs(tt.args, io.Discard)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("parseArgs(%q) = %v, want no error", tt.args, err)
		case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
			t.Errorf("parseArgs(%q) = %v, want an error containing %q", tt.args, err, tt.wantErr)
		}
	}
}
