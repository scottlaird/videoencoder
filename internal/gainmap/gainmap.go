// Package gainmap makes HDR images that are also SDR images: an SDR
// base image plus a gain map saying how much brighter each pixel is in
// the HDR rendition. Viewers that don't understand gain maps show the
// SDR base; those that do brighten it as far as the display allows.
//
// This package computes the gain map and writes Ultra HDR JPEGs, which
// carry both the ISO 21496-1 metadata and the older Adobe/Google XMP
// form, so Chrome, Android and Apple platforms all read them.
package gainmap

import (
	"fmt"
	"image"
	"math"
)

// Metadata describes a gain map. Values are in the log2 domain, as in
// ISO 21496-1 and the hdrgm XMP schema, except Gamma and the offsets.
type Metadata struct {
	GainMapMin     float64 // log2 of the smallest gain the map encodes
	GainMapMax     float64 // log2 of the largest
	Gamma          float64
	OffsetSDR      float64 // added to linear values before taking ratios
	OffsetHDR      float64
	HDRCapacityMin float64 // log2 headroom at which the map starts to apply
	HDRCapacityMax float64 // log2 headroom at which it applies fully
}

// MaxHeadroom (log2) is the display headroom at which the gain map is
// fully applied. Viewers scale the gain by how much of this headroom
// the display has, so a value matched to real HDR displays (here 16x
// SDR white, libavif's default) gives brighter results on them than
// the image's full range would, at the cost of clipping its most
// extreme highlights on lesser displays.
const MaxHeadroom = 4.0

// Offset is the default for both offsets: small enough not to matter
// for bright pixels, large enough to keep ratios of near-black pixels
// sane. It is Adobe's default.
const Offset = 1.0 / 64

// Linear is an image in linear light, BT.709 primaries, three float32s
// per pixel, row by row, scaled so that SDR reference white is 1.0.
type Linear struct {
	Width, Height int
	Pix           []float32
}

// luminance returns the BT.709 relative luminance of pixel i.
func (l *Linear) luminance(i int) float64 {
	r, g, b := float64(l.Pix[3*i]), float64(l.Pix[3*i+1]), float64(l.Pix[3*i+2])
	return max(0, 0.2126*r+0.7152*g+0.0722*b)
}

// srgbToLinear is the sRGB EOTF for each 8-bit value.
var srgbToLinear = func() (t [256]float64) {
	for i := range t {
		v := float64(i) / 255
		if v <= 0.04045 {
			t[i] = v / 12.92
		} else {
			t[i] = math.Pow((v+0.055)/1.055, 2.4)
		}
	}
	return t
}()

// Compute returns the single-channel gain map that brightens sdr (as
// viewers will display it, sRGB) into hdr, with its metadata. The two
// must be the same size. The map is 1/scale the size of the images in
// each dimension, each value the mean log gain of its block; decoders
// scale it back up.
func Compute(sdr *image.RGBA, hdr *Linear, scale int) (*image.Gray, Metadata, error) {
	b := sdr.Bounds()
	w, h := b.Dx(), b.Dy()
	if w != hdr.Width || h != hdr.Height || len(hdr.Pix) != 3*w*h {
		return nil, Metadata{}, fmt.Errorf("SDR is %dx%d but HDR is %dx%d", w, h, hdr.Width, hdr.Height)
	}

	if scale < 1 {
		return nil, Metadata{}, fmt.Errorf("gain map scale %d: must be at least 1", scale)
	}
	gw, gh := (w+scale-1)/scale, (h+scale-1)/scale
	logGain := make([]float64, gw*gh)
	count := make([]int, gw*gh)
	for y := range h {
		for x := range w {
			p := sdr.PixOffset(b.Min.X+x, b.Min.Y+y)
			ysdr := 0.2126*srgbToLinear[sdr.Pix[p]] + 0.7152*srgbToLinear[sdr.Pix[p+1]] + 0.0722*srgbToLinear[sdr.Pix[p+2]]
			j := (y/scale)*gw + x/scale
			logGain[j] += math.Log2((hdr.luminance(y*w+x) + Offset) / (ysdr + Offset))
			count[j]++
		}
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for j := range logGain {
		logGain[j] /= float64(count[j])
		lo, hi = min(lo, logGain[j]), max(hi, logGain[j])
	}
	// A map needs a non-empty range, and it only ever brightens: an
	// HDR rendition dimmer than the SDR one somewhere still has a
	// headroom of at least 1x.
	hi = max(hi, lo+1.0/256, 1.0/256)

	m := Metadata{
		GainMapMin:     lo,
		GainMapMax:     hi,
		Gamma:          1,
		OffsetSDR:      Offset,
		OffsetHDR:      Offset,
		HDRCapacityMin: 0,
		HDRCapacityMax: min(hi, MaxHeadroom),
	}
	gm := image.NewGray(image.Rect(0, 0, gw, gh))
	for i, g := range logGain {
		v := (g - lo) / (hi - lo)
		gm.Pix[i] = uint8(math.Round(255 * math.Min(1, math.Max(0, v))))
	}
	return gm, m, nil
}

// Apply reconstructs the HDR rendition's luminance at pixel (x, y) from
// the SDR base and the gain map, fully applied, sampling the map at the
// nearest point. It is the decoder side of Compute, for tests.
func Apply(sdr *image.RGBA, gm *image.Gray, m Metadata, x, y int) float64 {
	p := sdr.PixOffset(x, y)
	ysdr := 0.2126*srgbToLinear[sdr.Pix[p]] + 0.7152*srgbToLinear[sdr.Pix[p+1]] + 0.0722*srgbToLinear[sdr.Pix[p+2]]
	gx := x * gm.Bounds().Dx() / sdr.Bounds().Dx()
	gy := y * gm.Bounds().Dy() / sdr.Bounds().Dy()
	r := math.Pow(float64(gm.GrayAt(gx, gy).Y)/255, 1/m.Gamma)
	g := m.GainMapMin*(1-r) + m.GainMapMax*r
	return (ysdr+m.OffsetSDR)*math.Exp2(g) - m.OffsetHDR
}
