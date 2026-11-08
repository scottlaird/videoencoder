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
			"av1-1080":  {Encoder: "av1", Height: 1080, Quality: 31, HDR: &config.RenditionHDR{Quality: new(25.0)}},
		},
	}
	c.Renditions["h264-720"].HDR = &config.RenditionHDR{Bitrate: "1M"}
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
	// HDR settings override the SDR ones field by field.
	for i, want := range []struct {
		quality float64
		bitrate uint64
	}{{25, 0}, {18, 1000000}, {18, 4000000}} {
		if h := got[i].ForHDR(); h.Quality != want.quality || h.Bitrate != want.bitrate {
			t.Errorf("%s ForHDR() quality %g bitrate %d, want %g, %d", got[i].Name, h.Quality, h.Bitrate, want.quality, want.bitrate)
		}
	}
	if got[0].Quality != 31 {
		t.Errorf("ForHDR changed av1-1080's SDR quality to %g", got[0].Quality)
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
			wantArgs: "-vf scale=1280:720:flags=lanczos -c:v libx264 -preset faster -crf 18 -b:v 2000000 -maxrate 3000000 -bufsize 3000000 -an",
		},
		{
			name: "uncapped, fractional quality, per-rendition args",
			target: Target{
				Name: "av1-1080", EncoderName: "av1", Width: 1920, Height: 1080, Quality: 30.5, Bitrate: 2000000, Args: []string{"-svtav1-params", "tune=0"},
				Encoder: config.Encoder{Codec: "libsvtav1", QualityFlag: "-crf", Args: []string{"-preset", "5"}},
			},
			wantArgs: "-vf scale=1920:1080:flags=lanczos -c:v libsvtav1 -preset 5 -crf 30.5 -svtav1-params tune=0 -an",
		},
		{
			name: "extra args follow the encoder's",
			target: Target{
				Name: "h265-1080", EncoderName: "h265", Width: 1920, Height: 1080, Quality: 60,
				Encoder: config.Encoder{Codec: "hevc_videotoolbox", QualityFlag: "-q:v", Args: []string{"-tag:v", "hvc1"}, ExtraArgs: []string{"-g", "120"}},
			},
			wantArgs: "-vf scale=1920:1080:flags=lanczos -c:v hevc_videotoolbox -tag:v hvc1 -g 120 -q:v 60 -an",
		},
		{
			name: "keyframes every segment, with the encoder's GOP options before extra args",
			target: Target{
				Name: "h265-1080", EncoderName: "h265", Width: 1920, Height: 1080, Quality: 22, Segment: 4 * time.Second, GOP: 96,
				Encoder: config.Encoder{Codec: "libx265", QualityFlag: "-crf", Args: []string{"-preset", "faster"},
					GOPArgs: []string{"-g", "{gop}", "-flags", "+cgop"}, ExtraArgs: []string{"-g", "48"}},
			},
			wantArgs: "-vf scale=1920:1080:flags=lanczos -c:v libx265 -preset faster -g 96 -flags +cgop " +
				"-force_key_frames expr:gte(t,n_forced*4) -g 48 -crf 22 -an",
		},
		{
			name: "unknown frame rate: forced keyframes, but no GOP options",
			target: Target{
				Name: "av1-720", EncoderName: "av1", Width: 1280, Height: 720, Quality: 31, Segment: 2500 * time.Millisecond,
				Encoder: config.Encoder{Codec: "libsvtav1", QualityFlag: "-crf", GOPArgs: []string{"-g", "{gop}"}},
			},
			wantArgs: "-vf scale=1280:720:flags=lanczos -c:v libsvtav1 -force_key_frames expr:gte(t,n_forced*2.5) -crf 31 -an",
		},
	}
	for _, tt := range tests {
		j := tt.target.Job(src, "out", false)
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
	base := target(30).Job(src, "out", false).Key

	moved := src
	moved.Path = "/b/in.mov"
	edited := src
	edited.ID = "id2"
	tests := []struct {
		name string
		key  string
		same bool
	}{
		{"same inputs", target(30).Job(src, "out", false).Key, true},
		{"moved source", target(30).Job(moved, "out", false).Key, true},
		{"other outdir", target(30).Job(src, "elsewhere", false).Key, true},
		{"new quality", target(31).Job(src, "out", false).Key, false},
		{"edited source", target(30).Job(edited, "out", false).Key, false},
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

func TestSeekThumbnailToneMap(t *testing.T) {
	src := Source{Label: "main", Path: "in.mov", ID: "id1"}
	thumbs := config.Thumbnails{Interval: 5, Height: 180, Columns: 5, Rows: 5}
	tests := []struct {
		transfer string
		want     bool
	}{
		{"bt709", false},
		{"smpte2084", true},
		{"arib-std-b67", true},
	}
	for _, tt := range tests {
		vd := probe.VideoData{Width: 3840, Height: 2160, ColorTransfer: tt.transfer}
		j := SeekThumbnailJob(thumbs, vd, src, "out")
		vf := j.Args[slices.Index(j.Args, "-vf")+1]
		if got := strings.Contains(vf, ToneMapSDR); got != tt.want {
			t.Errorf("%s: tone-mapped = %v, want %v (-vf %s)", tt.transfer, got, tt.want, vf)
		}
		if strings.Contains(vf, "yuvj") || !strings.Contains(vf, "out_range=full,format=yuv420p") {
			t.Errorf("thumbnail tiles aren't full-range yuv420p from the scaler: %s", vf)
		}
		if !strings.Contains(vf, Scale(320, 180)) {
			t.Errorf("%s: -vf %s doesn't scale with %s", tt.transfer, vf, Scale(320, 180))
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
		{"SDR from SDR: 7680 dropped, full width added", sdr, probe.SDR, []string{"SDR 320x180 avif", "SDR 320x180 jpg", "SDR 1920x1080 avif", "SDR 1920x1080 jpg", "SDR 3840x2160 avif", "SDR 3840x2160 jpg"}},
		{"HDR is AVIF only", hdr, probe.HDR, []string{"HDR 320x180 avif", "HDR 1920x1080 avif", "HDR 3840x2160 avif"}},
		{"tiny source gets just its own width", probe.VideoData{Width: 200, Height: 100}, probe.SDR, []string{"SDR 200x100 avif", "SDR 200x100 jpg"}},
		{"source width already listed isn't repeated", probe.VideoData{Width: 1920, Height: 1080}, probe.SDR, []string{"SDR 320x180 avif", "SDR 320x180 jpg", "SDR 1920x1080 avif", "SDR 1920x1080 jpg"}},
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

	// JPEGs are full range, made by the scaler, not by ffmpeg's
	// deprecated yuvj formats; other formats stay limited range.
	for _, poster := range Posters(p, src, sdr, probe.SDR, "out") {
		vf := poster.Job.Args[slices.Index(poster.Job.Args, "-vf")+1]
		if strings.Contains(vf, "yuvj") {
			t.Errorf("%s poster uses a deprecated yuvj format: %s", poster.Format, vf)
		}
		if full := strings.Contains(vf, "out_range=full"); full != (poster.Format == "jpg") {
			t.Errorf("%s poster: full range %v, want it only for JPEG: %s", poster.Format, full, vf)
		}
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

func TestToneMappedJob(t *testing.T) {
	tg := &Target{Name: "h264-720", EncoderName: "h264", Width: 1280, Height: 720, Quality: 18,
		Encoder: config.Encoder{Codec: "libx264", QualityFlag: "-crf", Args: []string{"-pix_fmt", "yuv420p"}, SDROnly: true}}
	src := Source{Label: "hdr", Path: "in.mov", ID: "id1"}
	plain, mapped := tg.Job(src, "out", false), tg.Job(src, "out", true)
	args := strings.Join(mapped.Args, " ")
	if !strings.HasPrefix(args, "-vf "+ToneMapSDR+",scale=1280:720:flags=lanczos ") || !strings.Contains(args, "-color_trc bt709") {
		t.Errorf("tone-mapped args = %q", args)
	}
	if strings.Contains(strings.Join(plain.Args, " "), "tonemap") {
		t.Errorf("plain job is tone-mapped: %q", plain.Args)
	}
	if plain.Key == mapped.Key {
		t.Error("tone-mapped and plain jobs share a key")
	}
}

func TestFilterGOP(t *testing.T) {
	ladder := Targets{{Height: 720, EncoderName: "h265", Segment: 5 * time.Second}}
	tests := []struct {
		fps  float64
		want uint64
	}{
		{24000.0 / 1001, 120}, // 119.88, rounded up
		{30, 150},
		{60000.0 / 1001, 300}, // 299.7
		{25, 125},
		{0, 0}, // unknown
	}
	for _, tt := range tests {
		got := ladder.Filter(probe.VideoData{Width: 1280, Height: 720, FPS: tt.fps})
		if len(got) != 1 || got[0].GOP != tt.want {
			t.Errorf("%g fps: GOP = %d, want %d", tt.fps, got[0].GOP, tt.want)
		}
	}
}
