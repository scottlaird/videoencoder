package tune

import (
	"math"
	"slices"
	"testing"

	"github.com/scottlaird/videoencoder/internal/plan"
)

func TestAnchors(t *testing.T) {
	targets := plan.Targets{
		{EncoderName: "av1", Height: 480}, {EncoderName: "av1", Height: 720}, {EncoderName: "av1", Height: 1080},
		{EncoderName: "av1", Height: 2160}, {EncoderName: "h264", Height: 720},
	}
	got := Anchors(targets, 2)
	if !slices.Equal(got["av1"], []int{0, 3}) || !slices.Equal(got["h264"], []int{4}) {
		t.Errorf("Anchors(2) = %v", got)
	}
	if got := Anchors(targets, 3); !slices.Equal(got["av1"], []int{0, 2, 3}) {
		t.Errorf("Anchors(3) = %v", got)
	}
}

func TestInterpolate(t *testing.T) {
	points := []Point{{Height: 2160, Quality: 34, Bitrate: 8e6}, {Height: 540, Quality: 30, Bitrate: 1e6}}
	tests := []struct {
		height      uint64
		quality     float64
		bitrateWant float64
	}{
		{540, 30, 1e6},
		{2160, 34, 8e6},
		{1080, 32, 2.828e6}, // halfway in log height: halfway in quality, geometric mean bitrate
		{360, 30, 1e6},      // held, not extrapolated
		{4320, 34, 8e6},
	}
	for _, tt := range tests {
		q, b := Interpolate(points, tt.height)
		if math.Abs(q-tt.quality) > 1e-9 || math.Abs(b-tt.bitrateWant)/tt.bitrateWant > 1e-3 {
			t.Errorf("Interpolate(%d) = %v, %v; want %v, %v", tt.height, q, b, tt.quality, tt.bitrateWant)
		}
	}
	if q, _ := Interpolate(nil, 720); !math.IsNaN(q) {
		t.Errorf("Interpolate(no points) = %v, want NaN", q)
	}
}

func TestBitrateFormatting(t *testing.T) {
	tests := []struct {
		bps  float64
		want string
	}{
		{2437123, "2.4M"},
		{849999, "850k"},
		{12345678, "12M"},
		{95123, "95k"},
		{0, "0"},
	}
	for _, tt := range tests {
		if got := FormatBitrate(roundBitrate(tt.bps)); got != tt.want {
			t.Errorf("bitrate %v → %q, want %q", tt.bps, got, tt.want)
		}
	}
}
