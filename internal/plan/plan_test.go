package plan

import (
	"fmt"
	"slices"
	"testing"

	"github.com/scottlaird/videoencoder/internal/probe"
)

func describe(ts Targets) []string {
	var out []string
	for _, t := range ts {
		out = append(out, fmt.Sprintf("%s %dx%d", t.Codec, t.Width, t.Height))
	}
	return out
}

func TestFilter(t *testing.T) {
	ladder := Targets{
		{Height: 720, Codec: "h264"},
		{Height: 1080, Codec: "h264"},
		{Height: 1080, Codec: "av1"},
		{Height: 2160, Codec: "av1"},
		{Height: 3240, Codec: "av1"},
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
	ladder := DefaultTargets(false)
	ladder.Filter(probe.VideoData{Width: 1920, Height: 1080})
	for _, tg := range ladder {
		if tg.Width != 0 {
			t.Fatalf("Filter changed the input ladder: %+v", tg)
		}
	}
}

func TestJob(t *testing.T) {
	tg := &Target{Width: 1920, Height: 1080, Codec: "av1", Bitrate: 2000000, CodecQuality: 31}
	j, err := tg.Job("in.mov", "out", 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := "out/video_1_av1_1920x1080_2000000.mp4"; j.Output != want {
		t.Errorf("Output = %q, want %q", j.Output, want)
	}
	if want := "ffmpeg -loglevel warning -stats -y -i in.mov -s 1920x1080 -c:v libsvtav1 -crf 31 -preset 5 -an out/video_1_av1_1920x1080_2000000.mp4"; j.Cmdline() != want {
		t.Errorf("Cmdline() =\n %q\nwant\n %q", j.Cmdline(), want)
	}

	for _, codec := range []string{"h264", "h265", "jpg"} {
		if _, err := (&Target{Codec: codec}).Job("in.mov", "out", 0); err != nil {
			t.Errorf("Job(%s) error = %v", codec, err)
		}
	}
	if _, err := (&Target{Codec: "vp9"}).Job("in.mov", "out", 0); err == nil {
		t.Error("Job(vp9): got nil error, want one")
	}
}
