package bench

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

// ColorStats are percentiles of block color error between an encode
// and its reference, as ΔE_ITP chroma (see Metrics.ColorShiftP99).
type ColorStats struct {
	// Shift is each block's color error: a cast in part of the picture.
	ShiftP99, ShiftP999 float64
	// Flicker is how far a block's color error strays from its average
	// over colorWindow frames: patches whose color comes and goes,
	// which is far more visible than a steady shift of the same size.
	FlickerP99, FlickerP999 float64
}

// colorSpace says how to read block-image pixels.
type colorSpace int

const (
	spaceSDR colorSpace = iota // BT.709 primaries, BT.1886 gamma, white at 100 nits
	spacePQ                    // BT.2020 primaries, SMPTE ST 2084
)

// colorSpaceFor returns how to read block images of a reference with
// the given transfer, and the filter that gets them there: HLG is
// converted to PQ by ffmpeg, so only PQ and SDR need handling here.
func colorSpaceFor(transfer string) (colorSpace, string) {
	switch transfer {
	case "smpte2084":
		return spacePQ, ""
	case "arib-std-b67":
		return spacePQ, "zscale=t=smpte2084:p=bt2020:m=bt2020nc:npl=1000,format=yuv444p10le"
	}
	return spaceSDR, ""
}

// colorWindow is the span, in frames, over which flicker is measured.
// It spans several of SVT-AV1's hierarchical layers (half its 32-frame
// mini-GOP at preset 5), whose cycle of quantizers is what makes a cast
// pulse. Comparing only adjacent frames misses it, since neighbors err
// alike. 16 was chosen by testing 8, 16 and 32 on Pyramid Creek; all
// separated the encoders, 16 the most.
const colorWindow = 16

// ColorShift reads the block-average images scoreArgv wrote (16-bit
// little-endian RGB, bw x bh per frame) and summarizes the color error. Frames are paired by
// position; if one side has more (an encode can end a frame or two
// differently), the extra are ignored, as libvmaf does.
func ColorShift(distPath, refPath string, bw, bh uint64, space colorSpace) (ColorStats, error) {
	dist, err := os.ReadFile(distPath)
	if err != nil {
		return ColorStats{}, err
	}
	ref, err := os.ReadFile(refPath)
	if err != nil {
		return ColorStats{}, err
	}
	frame := int(bw * bh * 6)
	if len(dist)%frame != 0 || len(ref)%frame != 0 {
		return ColorStats{}, fmt.Errorf("block images are %d and %d bytes, want multiples of %d", len(dist), len(ref), frame)
	}
	n := min(len(dist), len(ref))
	if n == 0 {
		return ColorStats{}, fmt.Errorf("no frames to compare")
	}
	blocks := int(bw * bh)
	frames := n / frame
	// errs[f][b] is block b's (Ct/2, Cp) error in sampled frame f.
	type tp struct{ t, p float64 }
	errs := make([][]tp, frames)
	var shifts []float64
	for f := range frames {
		errs[f] = make([]tp, blocks)
		for b := range blocks {
			i := (f*blocks + b) * 6
			_, tr, pr := ictcp(ref[i:], space)
			_, td, pd := ictcp(dist[i:], space)
			e := tp{(td - tr) / 2, pd - pr}
			errs[f][b] = e
			shifts = append(shifts, 720*math.Hypot(e.t, e.p))
		}
	}
	// Windows of colorWindow frames; a short last one counts if it has
	// two frames to compare.
	var flicker []float64
	for w0 := 0; w0+1 < frames; w0 += colorWindow {
		w1 := min(w0+colorWindow, frames)
		for b := range blocks {
			var mean tp
			for f := w0; f < w1; f++ {
				mean.t += errs[f][b].t
				mean.p += errs[f][b].p
			}
			mean.t /= float64(w1 - w0)
			mean.p /= float64(w1 - w0)
			for f := w0; f < w1; f++ {
				flicker = append(flicker, 720*math.Hypot(errs[f][b].t-mean.t, errs[f][b].p-mean.p))
			}
		}
	}
	st := ColorStats{ShiftP99: percentile(shifts, 99), ShiftP999: percentile(shifts, 99.9)}
	if len(flicker) > 0 {
		st.FlickerP99, st.FlickerP999 = percentile(flicker, 99), percentile(flicker, 99.9)
	}
	return st, nil
}

// ictcp converts one 16-bit RGB pixel in space to ICtCp (BT.2100).
func ictcp(p []byte, space colorSpace) (i, ct, cp float64) {
	v := func(k int) float64 { return float64(binary.LittleEndian.Uint16(p[2*k:])) / 65535 }
	r, g, b := v(0), v(1), v(2)
	if space == spaceSDR {
		// BT.1886 display at 100 nits, BT.709 to BT.2020 primaries, in
		// linear light relative to 10000 nits.
		lin := func(x float64) float64 { return math.Pow(x, 2.4) / 100 }
		r, g, b = lin(r), lin(g), lin(b)
		r, g, b = 0.6274*r+0.3293*g+0.0433*b, 0.0691*r+0.9195*g+0.0114*b, 0.0164*r+0.0880*g+0.8956*b
	} else {
		r, g, b = pqEOTF(r), pqEOTF(g), pqEOTF(b)
	}
	l := pqInverseEOTF((1688*r + 2146*g + 262*b) / 4096)
	m := pqInverseEOTF((683*r + 2951*g + 462*b) / 4096)
	s := pqInverseEOTF((99*r + 309*g + 3688*b) / 4096)
	return 0.5*l + 0.5*m, (6610*l - 13613*m + 7003*s) / 4096, (17933*l - 17390*m - 543*s) / 4096
}

const (
	pqM1 = 0.1593017578125
	pqM2 = 78.84375
	pqC1 = 0.8359375
	pqC2 = 18.8515625
	pqC3 = 18.6875
)

// pqEOTF decodes a PQ signal (0-1) to linear light relative to 10000 nits.
func pqEOTF(e float64) float64 {
	p := math.Pow(e, 1/pqM2)
	return math.Pow(math.Max(p-pqC1, 0)/(pqC2-pqC3*p), 1/pqM1)
}

// pqInverseEOTF encodes linear light relative to 10000 nits as PQ.
func pqInverseEOTF(y float64) float64 {
	p := math.Pow(math.Max(y, 0), pqM1)
	return math.Pow((pqC1+pqC2*p)/(1+pqC3*p), pqM2)
}
