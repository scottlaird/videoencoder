package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func node(t *testing.T, s string) *yaml.Node {
	t.Helper()
	n, err := parseLayer([]byte(s), "test")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMergeInto(t *testing.T) {
	base := `
outdir: a
thumbnails: true
encoders:
  x: {codec: libx264, quality_flag: -crf, args: [-preset, fast]}
renditions:
  one: {encoder: x, height: 480, quality: 18}
  two: {encoder: x, height: 720, quality: 18}
`
	over := `
outdir: b
thumbnails: null
encoders:
  x: {args: [-preset, slow]}
renditions:
  one: {quality: 20}
  two: null
  three: {encoder: x, height: 1080, quality: 22}
`
	dst := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	mergeInto(dst, node(t, base), "base")
	mergeInto(dst, node(t, over), "over")

	var got layer
	if err := dst.Load(&got, yaml.WithKnownFields()); err != nil {
		t.Fatal(err)
	}
	if got.Outdir != "b" {
		t.Errorf("outdir = %q, want b (scalar replaced)", got.Outdir)
	}
	if got.Thumbnails {
		t.Error("thumbnails still set; null should delete it")
	}
	x := got.Encoders["x"]
	if x.Codec != "libx264" || !slices.Equal(x.Args, []string{"-preset", "slow"}) {
		t.Errorf("encoder x = %+v; want codec kept and args replaced", x)
	}
	if r := got.Renditions["one"]; r.Quality != 20 || r.Height != 480 {
		t.Errorf("rendition one = %+v; want quality 20 merged over height 480", r)
	}
	if _, ok := got.Renditions["two"]; ok {
		t.Error("rendition two survived a null")
	}
	if _, ok := got.Renditions["three"]; !ok {
		t.Error("rendition three not added")
	}

	out, err := yaml.Dump(dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"outdir: b # over", "height: 480 # base", "quality: 20 # over"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("merged YAML lacks %q:\n%s", want, out)
		}
	}
}

func TestLoadLayers(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "projects", "trip")
	global := filepath.Join(root, "global.yaml")

	write(t, global, `
outdir: /scratch/{name}
renditions:
  av1-4320: null
`)
	write(t, filepath.Join(root, "projects", ProjectFile), `
renditions:
  av1-2160: {quality: 34}
  h264-480: null
`)
	write(t, filepath.Join(proj, ProjectFile), `
streamdir: out/{name}
videos:
  hike:
    sources: {hdr: hike-hdr.mov, sdr: hike-sdr.mov}
    renditions:
      av1-1080: {quality: 29}
`)
	write(t, filepath.Join(proj, "hike"+SidecarSuffix), `
renditions:
  av1-2160: {quality: 30}
`)

	l := &Loader{GlobalPath: global, Overrides: map[string]any{"thumbnails": true}}
	r, err := l.Load(Video{Name: "hike", Dir: proj})
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}

	checks := []struct {
		what      string
		got, want any
	}{
		{"outdir from global, {name} expanded", r.Outdir, "/scratch/hike"},
		{"streamdir from project, made absolute", r.Streamdir, filepath.Join(proj, "out", "hike")},
		{"sources relative to the video", r.Sources["sdr"], filepath.Join(proj, "hike-sdr.mov")},
		{"source order", strings.Join(r.SourceLabels(), ","), "hdr,sdr"},
		{"sidecar beats parent project", r.Renditions["av1-2160"].Quality, 30.0},
		{"video entry applies", r.Renditions["av1-1080"].Quality, 29.0},
		{"defaults kept underneath", r.Renditions["av1-1080"].Height, uint64(1080)},
		{"command line on top", r.Thumbnails, true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.what, c.got, c.want)
		}
	}
	for _, gone := range []string{"av1-4320", "h264-480"} {
		if _, ok := r.Renditions[gone]; ok {
			t.Errorf("rendition %s should have been deleted", gone)
		}
	}

	out, err := yaml.Dump(r.Merged)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "quality: 30 # "+filepath.Join(proj, "hike"+SidecarSuffix)) {
		t.Errorf("provenance missing for the sidecar's value:\n%s", out)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{
			name:    "typo in a project file names the file",
			files:   map[string]string{ProjectFile: "renditons: {}\nvideos: {v: {sources: {a: a.mov}}}"},
			wantErr: ProjectFile,
		},
		{
			name:    "typo inside a video entry",
			files:   map[string]string{ProjectFile: "videos: {v: {sources: {a: a.mov}, qualty: 3}}"},
			wantErr: "videos.v",
		},
		{
			name:    "sidecar can't define videos",
			files:   map[string]string{"v" + SidecarSuffix: "sources: {a: a.mov}\nvideos: {w: {}}"},
			wantErr: "can't define videos",
		},
		{
			name:    "no sources",
			files:   map[string]string{"v" + SidecarSuffix: "thumbnails: true"},
			wantErr: "no sources",
		},
		{
			name:    "rendition with unknown encoder",
			files:   map[string]string{"v" + SidecarSuffix: "sources: {a: a.mov}\nrenditions: {x: {encoder: vp9, height: 720}}"},
			wantErr: `unknown encoder "vp9"`,
		},
		{
			name:    "bad bitrate",
			files:   map[string]string{"v" + SidecarSuffix: "sources: {a: a.mov}\nrenditions: {av1-720: {bitrate: fast}}"},
			wantErr: "bad bitrate",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tt.files {
				write(t, filepath.Join(dir, name), content)
			}
			_, err := (&Loader{}).Load(Video{Name: "v", Dir: dir})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Load() error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "v"+SidecarSuffix), "sources: {main: v.mov}")
	r, err := (&Loader{}).Load(Video{Name: "v", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(r.Renditions); got != 15 {
		t.Errorf("%d default renditions, want 15", got)
	}
	names := r.RenditionNames()
	if names[0] != "av1-480" || names[len(names)-1] != "h265-2160" {
		t.Errorf("RenditionNames() = %v; want sorted by encoder then height", names)
	}
	if r.Outdir != filepath.Join(dir, "transcodes", "v") {
		t.Errorf("default outdir = %q", r.Outdir)
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a", ProjectFile), "videos: {one: {sources: {s: one.mov}}, two: {sources: {s: two.mov}}}")
	write(t, filepath.Join(root, "a", "two"+SidecarSuffix), "renditions: {}")
	write(t, filepath.Join(root, "b", "c", "three"+SidecarSuffix), "sources: {s: three.mov}")
	write(t, filepath.Join(root, ".hidden", "four"+SidecarSuffix), "sources: {s: four.mov}")
	write(t, filepath.Join(root, "b", ProjectFile), "outdir: x")

	videos, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range videos {
		rel, _ := filepath.Rel(root, filepath.Join(v.Dir, v.Name))
		got = append(got, rel)
	}
	want := []string{"a/one", "a/two", "b/c/three"}
	if !slices.Equal(got, want) {
		t.Errorf("Discover() = %v, want %v", got, want)
	}

	one, err := Discover(filepath.Join(root, "b", "c", "three"+SidecarSuffix))
	if err != nil || len(one) != 1 || one[0].Name != "three" {
		t.Errorf("Discover(sidecar) = %v, %v", one, err)
	}
	if _, err := Discover(filepath.Join(root, "b", ProjectFile)); err == nil {
		t.Error("Discover(project file) = nil error, want one")
	}
}

func TestAdHoc(t *testing.T) {
	v, overrides, err := AdHoc([]string{"/v/trip-hdr.mov", "/v/trip-sdr.mov"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "trip-hdr" || v.Dir != "/v" {
		t.Errorf("AdHoc video = %+v", v)
	}
	sources := overrides["sources"].(map[string]any)
	if sources["0"] != "/v/trip-hdr.mov" || sources["1"] != "/v/trip-sdr.mov" {
		t.Errorf("AdHoc sources = %v", sources)
	}
}

func TestParseBitrate(t *testing.T) {
	tests := []struct {
		in      string
		want    uint64
		wantErr bool
	}{
		{"", 0, false},
		{"1500000", 1500000, false},
		{"800k", 800000, false},
		{"2.5M", 2500000, false},
		{"fast", 0, true},
		{"-1M", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseBitrate(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseBitrate(%q) = %d, %v; want %d, err %v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}
