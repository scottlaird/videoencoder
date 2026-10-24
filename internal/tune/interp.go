// Package tune picks each rendition's quality setting by encoding a
// short sample of the video and searching for the setting that just
// reaches a target VMAF. Only a few anchor renditions per encoder are
// searched; the rest are interpolated.
package tune

import (
	"math"
	"slices"
	"strconv"

	"github.com/scottlaird/videoencoder/internal/plan"
)

// Anchors returns, for each encoder, the indexes into targets of the
// renditions to search: the shortest and tallest, plus the middle one
// if n >= 3. targets must be ordered by encoder, then height, as
// plan.FromConfig returns them.
func Anchors(targets plan.Targets, n int) map[string][]int {
	byEncoder := map[string][]int{}
	for i, t := range targets {
		byEncoder[t.EncoderName] = append(byEncoder[t.EncoderName], i)
	}
	out := map[string][]int{}
	for enc, idx := range byEncoder {
		pick := []int{idx[0], idx[len(idx)-1]}
		if n >= 3 && len(idx) >= 3 {
			pick = append(pick, idx[len(idx)/2])
		}
		slices.Sort(pick)
		out[enc] = slices.Compact(pick)
	}
	return out
}

// Point is a searched rendition: its height, the quality found, and
// the bitrate it produced.
type Point struct {
	Height  uint64
	Quality float64
	Bitrate float64 // bits per second
}

// Interpolate estimates quality and bitrate at height from points (of
// one encoder), linearly in log height, and log bitrate. Outside the
// points it holds the nearest one's values rather than extrapolating.
func Interpolate(points []Point, height uint64) (quality, bitrate float64) {
	ps := slices.Clone(points)
	slices.SortFunc(ps, func(a, b Point) int { return int(a.Height) - int(b.Height) })
	switch {
	case len(ps) == 0:
		return math.NaN(), math.NaN()
	case height <= ps[0].Height:
		return ps[0].Quality, ps[0].Bitrate
	case height >= ps[len(ps)-1].Height:
		last := ps[len(ps)-1]
		return last.Quality, last.Bitrate
	}
	i := slices.IndexFunc(ps, func(p Point) bool { return p.Height >= height }) - 1
	a, b := ps[i], ps[i+1]
	f := (math.Log2(float64(height)) - math.Log2(float64(a.Height))) /
		(math.Log2(float64(b.Height)) - math.Log2(float64(a.Height)))
	quality = a.Quality + f*(b.Quality-a.Quality)
	bitrate = math.Exp2(math.Log2(a.Bitrate) + f*(math.Log2(b.Bitrate)-math.Log2(a.Bitrate)))
	return quality, bitrate
}

// roundBitrate rounds bits per second to two significant figures, which
// is all a sample of a few seconds can justify.
func roundBitrate(bps float64) uint64 {
	if bps <= 0 {
		return 0
	}
	e := math.Pow(10, math.Floor(math.Log10(bps))-1)
	return uint64(math.Round(bps/e) * e)
}

// FormatBitrate writes bits per second the way config files do: "2.4M",
// "850k".
func FormatBitrate(bps uint64) string {
	switch {
	case bps >= 1e6:
		return trimFloat(float64(bps)/1e6) + "M"
	case bps >= 1e3:
		return trimFloat(float64(bps)/1e3) + "k"
	}
	return trimFloat(float64(bps))
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(math.Round(f*100)/100, 'f', -1, 64)
}
