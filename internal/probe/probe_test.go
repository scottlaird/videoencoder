package probe

import (
	"testing"

	"gopkg.in/vansante/go-ffprobe.v2"
)

func TestParseFrameRate(t *testing.T) {
	tests := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{in: "25", want: 25},
		{in: "30/1", want: 30},
		{in: "60000/1001", want: 60000.0 / 1001},
		{in: "0/0", wantErr: true},
		{in: "", wantErr: true},
		{in: "abc/1", wantErr: true},
		{in: "30/x", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseFrameRate(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseFrameRate(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseFrameRate(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestFromStreams(t *testing.T) {
	audio := &ffprobe.Stream{CodecType: "audio", RFrameRate: "0/0"}
	cover := &ffprobe.Stream{CodecType: "video", Width: 600, Height: 600, RFrameRate: "90000/1"}
	cover.Disposition.AttachedPic = 1
	video := &ffprobe.Stream{
		CodecType:      "video",
		Width:          3840,
		Height:         2160,
		RFrameRate:     "30000/1001",
		ColorTransfer:  "smpte2084",
		ColorPrimaries: "bt2020",
	}

	tests := []struct {
		name    string
		streams []*ffprobe.Stream
		want    VideoData
		wantErr bool
	}{
		{
			name:    "audio first, then cover art, then video",
			streams: []*ffprobe.Stream{audio, cover, video},
			want:    VideoData{Width: 3840, Height: 2160, FPS: 30000.0 / 1001, ColorTransfer: "smpte2084", ColorPrimaries: "bt2020"},
		},
		{name: "audio only", streams: []*ffprobe.Stream{audio}, wantErr: true},
		{name: "no streams", wantErr: true},
	}
	for _, tt := range tests {
		got, err := fromStreams("x.mov", tt.streams)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: error = %v, wantErr %v", tt.name, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("%s: got %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestTone(t *testing.T) {
	tests := []struct {
		transfer string
		want     Tone
	}{
		{"smpte2084", HDR},
		{"arib-std-b67", HDR},
		{"bt709", SDR},
		{"", UnknownTone},
	}
	for _, tt := range tests {
		if got := (VideoData{ColorTransfer: tt.transfer}).Tone(); got != tt.want {
			t.Errorf("Tone(%q) = %q, want %q", tt.transfer, got, tt.want)
		}
	}
}
