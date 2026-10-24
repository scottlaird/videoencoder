package bench

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

// ColourShift reads the block-average images scoreArgv wrote (16-bit
// little-endian RGB, bw x bh per frame, sRGB-encoded) and returns the
// 99th and 99.9th-percentile a*b* distances between corresponding
// blocks. Frames are paired by position; if one side has more (an
// encode can end a frame or two differently), the extra are ignored,
// as libvmaf does.
func ColourShift(distPath, refPath string, bw, bh uint64) (p99, p999 float64, err error) {
	dist, err := os.ReadFile(distPath)
	if err != nil {
		return 0, 0, err
	}
	ref, err := os.ReadFile(refPath)
	if err != nil {
		return 0, 0, err
	}
	frame := int(bw * bh * 6)
	if len(dist)%frame != 0 || len(ref)%frame != 0 {
		return 0, 0, fmt.Errorf("block images are %d and %d bytes, want multiples of %d", len(dist), len(ref), frame)
	}
	n := min(len(dist), len(ref))
	if n == 0 {
		return 0, 0, fmt.Errorf("no frames to compare")
	}
	dist, ref = dist[:n], ref[:n]
	shifts := make([]float64, 0, len(ref)/6)
	for i := 0; i+6 <= len(ref); i += 6 {
		a1, b1 := abFrom16(ref[i:])
		a2, b2 := abFrom16(dist[i:])
		shifts = append(shifts, math.Hypot(a1-a2, b1-b2))
	}
	return percentile(shifts, 99), percentile(shifts, 99.9), nil
}

// abFrom16 converts one 16-bit sRGB pixel to CIELAB a* and b* (D65).
func abFrom16(p []byte) (float64, float64) {
	lin := func(v uint16) float64 {
		c := float64(v) / 65535
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	r := lin(binary.LittleEndian.Uint16(p))
	g := lin(binary.LittleEndian.Uint16(p[2:]))
	b := lin(binary.LittleEndian.Uint16(p[4:]))
	x := (0.4124*r + 0.3576*g + 0.1805*b) / 0.95047
	y := 0.2126*r + 0.7152*g + 0.0722*b
	z := (0.0193*r + 0.1192*g + 0.9505*b) / 1.08883
	f := func(t float64) float64 {
		if t > 0.008856 {
			return math.Cbrt(t)
		}
		return 7.787*t + 16.0/116
	}
	return 500 * (f(x) - f(y)), 200 * (f(y) - f(z))
}
