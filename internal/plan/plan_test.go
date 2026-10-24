package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scottlaird/videoencoder/internal/config"
	"github.com/scottlaird/videoencoder/internal/ffmpeg"
	"github.com/scottlaird/videoencoder/internal/probe"
)

type ffmpegJob = ffmpeg.Job

func describe(ts Targets) []string {
	var out []string
	for _, t := range ts {
		out = append(out, fmt.Sprintf("%s %dx%d", t.EncoderName, t.Width, t.Height))
	}
	return out
}

func TestFilter(t *testing.T) {
	ladder := Targets{
		{Height: 720, EncoderName: "h264"},
		{Height: 1080, EncoderName: "h264"},
		{Height: 1080, EncoderName: "av1"},
		{Height: 2160, EncoderName: "av1"},
		{Height: 3240, EncoderName: "av1"},
	}
	tests := []struct {
		name string
		src  probe.VideoData
		want []string
	}{
		{
			name: "1080p source keeps targets up to 1080p",
			src:  probe.VideoData{Width: 1920, Height: 1080},
			want: []string{"h264 1280x720", "h264 1920x1080", "av1 1920x1080"},
		},
		{
			name: "6K source gets a full-resolution AV1 instead of upscaling to 3240",
			src:  probe.VideoData{Width: 5376, Height: 3024},
			want: []string{"h264 1280x720", "h264 1920x1080", "av1 1920x1080", "av1 3840x2160", "av1 5376x3024"},
		},
		{
			name: "source between rungs gets one native-size rendition per codec",
			src:  probe.VideoData{Width: 1600, Height: 900},
			want: []string{"h264 1280x720", "h264 1600x900", "av1 1600x900"},
		},
		{
			name: "odd widths are rounded down to even",
			src:  probe.VideoData{Width: 1442, Height: 1080}, // 720p would be 961.33
			want: []string{"h264 960x720", "h264 1442x1080", "av1 1442x1080"},
		},
	}
	for _, tt := range tests {
		got := describe(ladder.Filter(tt.src))
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s:\n got %q\nwant %q", tt.name, got, tt.want)
		}
	}
}

func TestFilterDoesNotModifyLadder(t *testing.T) {
	ladder := Targets{{Height: 720, EncoderName: "h264"}, {Height: 1080, EncoderName: "h264"}}
	ladder.Filter(probe.VideoData{Width: 1920, Height: 1080})
	for _, tg := range ladder {
		if tg.Width != 0 {
			t.Fatalf("Filter changed the input ladder: %+v", tg)
		}
	}
}

func TestFromConfig(t *testing.T) {
	c := &config.Config{
		Encoders: map[string]*config.Encoder{
			"h264": {Codec: "libx264", QualityFlag: "-crf"},
			"av1":  {Codec: "libsvtav1", QualityFlag: "-crf"},
		},
		Renditions: map[string]*config.Rendition{
			"h264-1080": {Encoder: "h264", Height: 1080, Quality: 18, Bitrate: "4M"},
			"h264-720":  {Encoder: "h264", Height: 720, Quality: 18, Bitrate: "2M"},
			"av1-1080":  {Encoder: "av1", Height: 1080, Quality: 31},
		},
	}
	got, err := FromConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tg := range got {
		names = append(names, tg.Name)
	}
	if want := []string{"av1-1080", "h264-720", "h264-1080"}; !slices.Equal(names, want) {
		t.Errorf("FromConfig() order = %v, want %v", names, want)
	}
	if got[2].Bitrate != 4000000 || got[2].Encoder.Codec != "libx264" {
		t.Errorf("h264-1080 = %+v", got[2])
	}
}

func TestJob(t *testing.T) {
	src := Source{Label: "main", Path: "in.mov", ID: "id1"}
	tests := []struct {
		name     string
		target   Target
		wantArgs string
	}{
		{
			name: "capped bitrate",
			target: Target{
				Name: "h264-720", EncoderName: "h264", Width: 1280, Height: 720, Quality: 18, Bitrate: 2000000,
				Encoder: config.Encoder{Codec: "libx264", QualityFlag: "-crf", Args: []string{"-preset", "faster"}, CapBitrate: true},
			},
			wantArgs: "-s 1280x720 -c:v libx264 -preset faster -crf 18 -b:v 2000000 -maxrate 3000000 -bufsize 3000000 -an",
		},
		{
			name: "uncapped, fractional quality, per-rendition args",
			target: Target{
				Name: "av1-1080", EncoderName: "av1", Width: 1920, Height: 1080, Quality: 30.5, Bitrate: 2000000, Args: []string{"-svtav1-params", "tune=0"},
				Encoder: config.Encoder{Codec: "libsvtav1", QualityFlag: "-crf", Args: []string{"-preset", "5"}},
			},
			wantArgs: "-s 1920x1080 -c:v libsvtav1 -preset 5 -crf 30.5 -svtav1-params tune=0 -an",
		},
	}
	for _, tt := range tests {
		j := tt.target.Job(src, "out")
		if got := strings.Join(j.Args, " "); got != tt.wantArgs {
			t.Errorf("%s: args =\n %q\nwant\n %q", tt.name, got, tt.wantArgs)
		}
		if want := "out/" + tt.target.Name + ".main." + j.Key + ".mp4"; j.Output != want {
			t.Errorf("%s: Output = %q, want %q", tt.name, j.Output, want)
		}
		if j.Slot != "main/"+tt.target.Name || j.Source != "id1" || len(j.Key) != keyLength {
			t.Errorf("%s: slot %q source %q key %q", tt.name, j.Slot, j.Source, j.Key)
		}
	}
}

func TestJobKey(t *testing.T) {
	target := func(q float64) *Target {
		return &Target{Name: "av1-1080", Width: 1920, Height: 1080, Quality: q,
			Encoder: config.Encoder{Codec: "libsvtav1", QualityFlag: "-crf"}}
	}
	src := Source{Label: "main", Path: "/a/in.mov", ID: "id1"}
	base := target(30).Job(src, "out").Key

	moved := src
	moved.Path = "/b/in.mov"
	edited := src
	edited.ID = "id2"
	tests := []struct {
		name string
		key  string
		same bool
	}{
		{"same inputs", target(30).Job(src, "out").Key, true},
		{"moved source", target(30).Job(moved, "out").Key, true},
		{"other outdir", target(30).Job(src, "elsewhere").Key, true},
		{"new quality", target(31).Job(src, "out").Key, false},
		{"edited source", target(30).Job(edited, "out").Key, false},
	}
	for _, tt := range tests {
		if (tt.key == base) != tt.same {
			t.Errorf("%s: key %s vs %s, want same=%v", tt.name, tt.key, base, tt.same)
		}
	}
}

func TestNewSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "in.mov")
	if err := os.WriteFile(path, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := NewSource("main", path)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	b, err := NewSource("main", path)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Error("touching the source didn't change its ID")
	}
	if _, err := NewSource("main", path+".missing"); err == nil {
		t.Error("NewSource(missing) = nil error")
	}
}

func TestAuxJobs(t *testing.T) {
	src := Source{Label: "hdr", Path: "in.mov", ID: "id1"}
	vd := probe.VideoData{Width: 3840, Height: 2160}
	tests := []struct {
		job        *ffmpegJob
		slot, name string
	}{
		{AudioJob(src, "out"), "audio", "audio.%s.mp4"},
		{SeekThumbnailJob(config.Thumbnails{Interval: 5, Height: 180, Columns: 5, Rows: 5}, vd, src, "out"), "thumbs", "thumbs.%s/tile-%%d.jpg"},
	}
	for _, tt := range tests {
		if want := "out/" + fmt.Sprintf(tt.name, tt.job.Key); tt.job.Slot != tt.slot || tt.job.Output != want {
			t.Errorf("%s: slot %q output %q, want %q", tt.slot, tt.job.Slot, tt.job.Output, want)
		}
	}
}

func TestPosters(t *testing.T) {
	p := config.Poster{Offset: "2s", Widths: []uint64{320, 1920, 7680}, Formats: []string{"avif", "jpg"}}
	src := Source{Label: "hdr", Path: "in.mov", ID: "id1"}
	hdr := probe.VideoData{Width: 3840, Height: 2160, ColorTransfer: "smpte2084"}
	sdr := probe.VideoData{Width: 3840, Height: 2160, ColorTransfer: "bt709"}

	describe := func(ps []Poster) []string {
		var out []string
		for _, p := range ps {
			out = append(out, fmt.Sprintf("%s %dx%d %s", p.Tone, p.Width, p.Height, p.Format))
		}
		return out
	}
	tests := []struct {
		name string
		vd   probe.VideoData
		tone probe.Tone
		want []string
	}{
		{"SDR from SDR, 7680 dropped", sdr, probe.SDR, []string{"SDR 320x180 avif", "SDR 320x180 jpg", "SDR 1920x1080 avif", "SDR 1920x1080 jpg"}},
		{"HDR is AVIF only", hdr, probe.HDR, []string{"HDR 320x180 avif", "HDR 1920x1080 avif"}},
		{"tiny source falls back to its own width", probe.VideoData{Width: 200, Height: 100}, probe.SDR, []string{"SDR 200x100 avif", "SDR 200x100 jpg"}},
	}
	for _, tt := range tests {
		if got := describe(Posters(p, src, tt.vd, tt.tone, "out")); !slices.Equal(got, tt.want) {
			t.Errorf("%s:\n got %q\nwant %q", tt.name, got, tt.want)
		}
	}

	// The SDR poster of an HDR source is tone-mapped; its HDR poster is
	// tagged PQ; both seek before decoding.
	sdrFromHDR := Posters(p, src, hdr, probe.SDR, "out")[0].Job
	hdrPoster := Posters(p, src, hdr, probe.HDR, "out")[0].Job
	if !strings.Contains(strings.Join(sdrFromHDR.Args, " "), "tonemap=") {
		t.Errorf("SDR poster from HDR isn't tone-mapped: %q", sdrFromHDR.Args)
	}
	if !strings.Contains(strings.Join(hdrPoster.Args, " "), "-color_trc smpte2084") {
		t.Errorf("HDR poster isn't tagged PQ: %q", hdrPoster.Args)
	}
	if !strings.Contains(strings.Join(hdrPoster.Base, " "), "-ss 2s -i in.mov") {
		t.Errorf("poster doesn't seek before the input: %q", hdrPoster.Base)
	}
	if want := "out/poster-hdr-320." + hdrPoster.Key + ".avif"; hdrPoster.Output != want || hdrPoster.Slot != "poster/hdr-320-avif" {
		t.Errorf("HDR poster output %q slot %q", hdrPoster.Output, hdrPoster.Slot)
	}
	if sdrFromHDR.Key == Posters(p, src, sdr, probe.SDR, "out")[0].Job.Key {
		t.Error("tone-mapped and direct SDR posters share a key")
	}
}

func TestGainMapPosterKeys(t *testing.T) {
	p := config.Poster{Offset: "2s", Widths: []uint64{640}, GainMapFormats: []string{"jpg"}}
	sdr := Source{Label: "sdr", ID: "s1"}
	hdr := Source{Label: "hdr", ID: "h1"}
	vd := probe.VideoData{Width: 1920, Height: 1080, ColorTransfer: "bt709"}
	key := func() string { return GainMapPosters(p, sdr, vd, hdr, "out")[0].Key }
	base := key()

	saved := GainMapJPEGQuality
	GainMapJPEGQuality.GainMap++
	if key() == base {
		t.Error("changing the encoder settings didn't change the key")
	}
	GainMapJPEGQuality = saved

	if g := GainMapPosters(p, sdr, vd, hdr, "out")[0]; g.ToneMap || g.Output != "out/poster-gainmap-640."+base+".jpg" {
		t.Errorf("poster %+v", g)
	}
	hdrVD := vd
	hdrVD.ColorTransfer = "smpte2084"
	if g := GainMapPosters(p, hdr, hdrVD, hdr, "out")[0]; !g.ToneMap || g.Key == base {
		t.Errorf("HDR-only poster: tonemap %v, key %s", g.ToneMap, g.Key)
	}
}
