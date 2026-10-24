package plan

import (
	"fmt"
	"slices"
	"testing"

	"github.com/scottlaird/videoencoder/internal/config"
	"github.com/scottlaird/videoencoder/internal/probe"
)

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
	tests := []struct {
		name   string
		target Target
		want   string
	}{
		{
			name: "capped bitrate",
			target: Target{
				EncoderName: "h264", Width: 1280, Height: 720, Quality: 18, Bitrate: 2000000,
				Encoder: config.Encoder{Codec: "libx264", QualityFlag: "-crf", Args: []string{"-preset", "faster"}, CapBitrate: true},
			},
			want: "ffmpeg -loglevel warning -stats -y -i in.mov -s 1280x720 -c:v libx264 -preset faster -crf 18 -b:v 2000000 -maxrate 3000000 -bufsize 3000000 -an out/video_1_h264_1280x720_2000000.mp4",
		},
		{
			name: "uncapped, fractional quality, per-rendition args",
			target: Target{
				EncoderName: "av1", Width: 1920, Height: 1080, Quality: 30.5, Bitrate: 2000000, Args: []string{"-svtav1-params", "tune=0"},
				Encoder: config.Encoder{Codec: "libsvtav1", QualityFlag: "-crf", Args: []string{"-preset", "5"}},
			},
			want: "ffmpeg -loglevel warning -stats -y -i in.mov -s 1920x1080 -c:v libsvtav1 -preset 5 -crf 30.5 -svtav1-params tune=0 -an out/video_1_av1_1920x1080_2000000.mp4",
		},
	}
	for _, tt := range tests {
		if got := tt.target.Job("in.mov", "out", 1).Cmdline(); got != tt.want {
			t.Errorf("%s: Cmdline() =\n %q\nwant\n %q", tt.name, got, tt.want)
		}
	}
}
