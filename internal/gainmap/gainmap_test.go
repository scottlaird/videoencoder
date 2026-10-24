package gainmap

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// testImages returns an SDR gradient and an HDR rendition whose linear
// luminance is boost times the SDR's, with a hot spot of 16x in the
// middle.
func testImages(w, h int, boost float64) (*image.RGBA, *Linear) {
	sdr := image.NewRGBA(image.Rect(0, 0, w, h))
	hdr := &Linear{Width: w, Height: h, Pix: make([]float32, 3*w*h)}
	for y := range h {
		for x := range w {
			v := uint8(40 + 200*x/w)
			c := color.RGBA{v, uint8(255 - int(v)/2), uint8(100 + y%100), 255}
			sdr.SetRGBA(x, y, c)
			k := boost
			if x > w/3 && x < 2*w/3 && y > h/3 && y < 2*h/3 {
				k = 16
			}
			i := 3 * (y*w + x)
			for ch, cv := range []uint8{c.R, c.G, c.B} {
				hdr.Pix[i+ch] = float32(srgbToLinear[cv] * k)
			}
		}
	}
	return sdr, hdr
}

func TestComputeRoundTrip(t *testing.T) {
	sdr, hdr := testImages(64, 48, 4)
	gm, m, err := Compute(sdr, hdr, 1)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(m.GainMapMax-4) > 0.05 || m.GainMapMin < 1.9 || m.HDRCapacityMax != min(m.GainMapMax, MaxHeadroom) {
		t.Errorf("metadata %+v; want gains from about log2(4)=2 to log2(16)=4", m)
	}
	for _, p := range []image.Point{{2, 2}, {32, 24}, {60, 40}} {
		want := hdr.luminance(p.Y*64 + p.X)
		got := Apply(sdr, gm, m, p.X, p.Y)
		if math.Abs(got-want)/want > 0.02 {
			t.Errorf("at %v: reconstructed %.4f, want %.4f", p, got, want)
		}
	}

	if _, _, err := Compute(sdr, &Linear{Width: 1, Height: 1, Pix: make([]float32, 3)}, 1); err == nil {
		t.Error("Compute with mismatched sizes: nil error")
	}
}

func TestComputeScaled(t *testing.T) {
	sdr, hdr := testImages(64, 48, 4)
	gm, m, err := Compute(sdr, hdr, 4)
	if err != nil {
		t.Fatal(err)
	}
	if b := gm.Bounds(); b.Dx() != 16 || b.Dy() != 12 {
		t.Errorf("gain map is %v, want 16x12", b)
	}
	// Away from the hot spot's edges, a quarter-size map still
	// reconstructs the HDR rendition.
	for _, p := range []image.Point{{2, 2}, {32, 24}, {60, 44}} {
		want := hdr.luminance(p.Y*64 + p.X)
		if got := Apply(sdr, gm, m, p.X, p.Y); math.Abs(got-want)/want > 0.03 {
			t.Errorf("at %v: reconstructed %.4f, want %.4f", p, got, want)
		}
	}
	// Sizes that don't divide evenly round up.
	if gm, _, _ := Compute(sdr, hdr, 5); gm.Bounds().Dx() != 13 || gm.Bounds().Dy() != 10 {
		t.Errorf("scale 5 gain map is %v, want 13x10", gm.Bounds())
	}
	if _, _, err := Compute(sdr, hdr, 0); err == nil {
		t.Error("scale 0: nil error")
	}
}

func TestFlatImage(t *testing.T) {
	// HDR identical to SDR still makes a valid, non-empty range.
	sdr, hdr := testImages(8, 8, 1)
	for i := range hdr.Pix {
		hdr.Pix[i] = float32(srgbToLinear[sdr.Pix[i/3*4+i%3]])
	}
	_, m, err := Compute(sdr, hdr, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !(m.GainMapMax > m.GainMapMin) || m.GainMapMax <= 0 {
		t.Errorf("flat image metadata %+v", m)
	}
}

func TestHeadroomCap(t *testing.T) {
	sdr, hdr := testImages(16, 16, 50) // up to 50x: more than MaxHeadroom
	_, m, err := Compute(sdr, hdr, 1)
	if err != nil {
		t.Fatal(err)
	}
	if m.GainMapMax < 5.5 || m.HDRCapacityMax != MaxHeadroom {
		t.Errorf("metadata %+v; want the map's full range but capacity capped at %v", m, MaxHeadroom)
	}
}

func TestFractions(t *testing.T) {
	for _, v := range []float64{0, 1.0 / 64, -3.738311, 8.094525, 1e-7, 1000.5, -1000.5} {
		n, d, err := signedFraction(v)
		if err != nil {
			t.Fatal(err)
		}
		if got := float64(n) / float64(d); math.Abs(got-v) > 1e-6*math.Max(1, math.Abs(v)) {
			t.Errorf("signedFraction(%v) = %d/%d = %v", v, n, d, got)
		}
	}
	if _, _, err := unsignedFraction(-1); err == nil {
		t.Error("unsignedFraction(-1): nil error")
	}
	if _, _, err := signedFraction(math.NaN()); err == nil {
		t.Error("signedFraction(NaN): nil error")
	}
}

// jpegSegments returns the APPn segments of the JPEG at the start of b
// and the offset of its end.
func jpegSegments(t *testing.T, b []byte) (map[string][]byte, int) {
	t.Helper()
	if !bytes.HasPrefix(b, []byte{0xFF, 0xD8}) {
		t.Fatalf("no SOI")
	}
	segs := map[string][]byte{}
	for p := 2; p+4 <= len(b); {
		if b[p] != 0xFF {
			t.Fatalf("bad marker at %d", p)
		}
		m := b[p+1]
		if m == 0xDA {
			end := bytes.Index(b[p:], []byte{0xFF, 0xD9})
			return segs, p + end + 2
		}
		n := int(binary.BigEndian.Uint16(b[p+2:]))
		body := b[p+4 : p+2+n]
		switch {
		case bytes.HasPrefix(body, []byte(xmpNS)):
			segs["xmp"] = body
		case bytes.HasPrefix(body, []byte(isoURN)):
			segs["iso"] = body[len(isoURN)+1:]
		case bytes.HasPrefix(body, []byte("MPF\x00")):
			segs["mpf"] = body
			segs["mpf@"] = binary.BigEndian.AppendUint32(nil, uint32(p+4+4))
		}
		p += 2 + n
	}
	t.Fatal("no SOS")
	return nil, 0
}

func writeTestJPEG(t *testing.T, w, h int) ([]byte, Metadata) {
	t.Helper()
	sdr, hdr := testImages(w, h, 4)
	gm, m, err := Compute(sdr, hdr, 1)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteJPEG(&buf, sdr, gm, m, Quality{Base: 95, GainMap: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), m
}

func TestWriteJPEGStructure(t *testing.T) {
	data, m := writeTestJPEG(t, 64, 48)

	primary, end := jpegSegments(t, data)
	for _, k := range []string{"xmp", "iso", "mpf"} {
		if primary[k] == nil {
			t.Fatalf("primary image lacks %s", k)
		}
	}
	if !bytes.Equal(primary["iso"], []byte{0, 0, 0, 0}) {
		t.Errorf("primary ISO segment = %x, want the version marker", primary["iso"])
	}

	// MPF: entry 2's offset, from the TIFF header, lands on the gain
	// map's SOI, and the sizes add up to the file.
	mpf := primary["mpf"]
	tiffAt := int(binary.BigEndian.Uint32(primary["mpf@"]))
	entries := mpf[4+8+2+3*12+4:]
	primaryLen := int(binary.BigEndian.Uint32(entries[4:]))
	gainLen := int(binary.BigEndian.Uint32(entries[16+4:]))
	gainAt := tiffAt + int(binary.BigEndian.Uint32(entries[16+8:]))
	if primaryLen != end || gainAt != end || primaryLen+gainLen != len(data) {
		t.Fatalf("MPF says primary %d, gain map %d at %d; file has primary %d of %d", primaryLen, gainLen, gainAt, end, len(data))
	}
	if !regexp.MustCompile(`Item:Length="` + strconv.Itoa(gainLen) + `"`).Match(primary["xmp"]) {
		t.Errorf("primary XMP doesn't give the gain map length %d", gainLen)
	}

	gain, _ := jpegSegments(t, data[gainAt:])
	if !bytes.Contains(gain["xmp"], []byte(`hdrgm:GainMapMax="`+strconv.FormatFloat(m.GainMapMax, 'f', -1, 64))) {
		t.Errorf("gain map XMP lacks GainMapMax:\n%s", gain["xmp"])
	}
	if len(gain["iso"]) != 5+16+40 {
		t.Errorf("ISO gain map metadata is %d bytes, want %d", len(gain["iso"]), 5+16+40)
	}

	// Both halves decode as ordinary JPEGs.
	base, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil || base.Bounds().Dx() != 64 {
		t.Errorf("primary decode: %v", err)
	}
	gmImg, err := jpeg.Decode(bytes.NewReader(data[gainAt:]))
	if err != nil {
		t.Fatalf("gain map decode: %v", err)
	}
	if _, ok := gmImg.(*image.Gray); !ok {
		t.Errorf("gain map decodes as %T, want grayscale", gmImg)
	}
}

// TestReferenceDecoder checks the output with libultrahdr's
// ultrahdr_app, when it is installed: it must recognise the file and
// read back the metadata we wrote.
func TestReferenceDecoder(t *testing.T) {
	app, err := exec.LookPath("ultrahdr_app")
	if err != nil {
		t.Skip("ultrahdr_app (libultrahdr) not installed")
	}
	data, m := writeTestJPEG(t, 64, 48)
	dir := t.TempDir()
	p := filepath.Join(dir, "uhdr.jpg")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(app, "-m", "1", "-P", "-j", p).CombinedOutput()
	if err != nil {
		t.Fatalf("ultrahdr_app probe: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("Ultra HDR Image: Yes")) {
		t.Fatalf("ultrahdr_app doesn't recognise the file:\n%s", out)
	}
	field := func(name string) float64 {
		mm := regexp.MustCompile(`--` + name + ` ([-0-9.e]+)`).FindSubmatch(out)
		if mm == nil {
			t.Fatalf("probe output lacks %s:\n%s", name, out)
		}
		v, _ := strconv.ParseFloat(string(mm[1]), 64)
		return v
	}
	checks := []struct {
		name      string
		got, want float64
	}{
		{"maxContentBoost", field("maxContentBoost"), math.Exp2(m.GainMapMax)},
		{"minContentBoost", field("minContentBoost"), math.Exp2(m.GainMapMin)},
		{"hdrCapacityMax", field("hdrCapacityMax"), math.Exp2(m.HDRCapacityMax)},
		{"offsetSdr", field("offsetSdr"), m.OffsetSDR},
		{"gamma", field("gamma"), 1},
	}
	for _, c := range checks {
		if math.Abs(c.got-c.want) > 1e-3*math.Max(1, c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	// Fully decoded to linear light, the HDR rendition must match the
	// HDR image the gain map was computed from. libultrahdr's linear
	// output is relative to SDR white, as ours is.
	raw := filepath.Join(dir, "linear.raw")
	if out, err := exec.Command(app, "-m", "1", "-j", p, "-o", "0", "-O", "4", "-z", raw).CombinedOutput(); err != nil {
		t.Fatalf("ultrahdr_app decode: %v\n%s", err, out)
	}
	pix, err := os.ReadFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	const w, h = 64, 48
	if len(pix) != w*h*4*2 {
		t.Fatalf("decoded %d bytes, want %d (RGBA half floats)", len(pix), w*h*4*2)
	}
	_, hdr := testImages(w, h, 4)
	for _, pt := range []image.Point{{4, 4}, {32, 24}, {60, 44}, {10, 30}} {
		i := pt.Y*w + pt.X
		rgb := [3]float64{}
		for c := range 3 {
			rgb[c] = halfToFloat(binary.LittleEndian.Uint16(pix[(4*i+c)*2:]))
		}
		got := 0.2126*rgb[0] + 0.7152*rgb[1] + 0.0722*rgb[2]
		want := hdr.luminance(i)
		if math.Abs(got-want)/want > 0.08 {
			t.Errorf("at %v: libultrahdr reconstructs luminance %.3f, want %.3f", pt, got, want)
		}
	}
}

// halfToFloat decodes an IEEE 754 half-precision float.
func halfToFloat(h uint16) float64 {
	sign := 1.0
	if h&0x8000 != 0 {
		sign = -1
	}
	exp := int(h>>10) & 0x1F
	frac := float64(h & 0x3FF)
	switch exp {
	case 0:
		return sign * math.Ldexp(frac, -24)
	case 31:
		return sign * math.Inf(1)
	}
	return sign * math.Ldexp(1+frac/1024, exp-15)
}
