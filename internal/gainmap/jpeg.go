package gainmap

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math"
	"strconv"
)

// isoURN names the ISO 21496-1 metadata segment.
const isoURN = "urn:iso:std:iso:ts:21496:-1"

// xmpNS prefixes an XMP APP1 segment.
const xmpNS = "http://ns.adobe.com/xap/1.0/"

// Quality settings for WriteJPEG.
type Quality struct {
	Base, GainMap int // image/jpeg quality, 1-100
}

// WriteJPEG writes an Ultra HDR JPEG: sdr as an ordinary baseline JPEG,
// followed by gm as a second JPEG, tied together by MPF (Multi-Picture
// Format) and described by both ISO 21496-1 metadata and hdrgm XMP.
//
// The primary image carries the XMP container directory, the ISO
// version marker and the MPF index; the gain map image carries the
// XMP and ISO gain map parameters. Readers that know neither form see
// a normal SDR JPEG.
func WriteJPEG(w io.Writer, sdr image.Image, gm *image.Gray, m Metadata, q Quality) error {
	var base, gain bytes.Buffer
	if err := jpeg.Encode(&base, sdr, &jpeg.Options{Quality: q.Base}); err != nil {
		return err
	}
	if err := jpeg.Encode(&gain, gm, &jpeg.Options{Quality: q.GainMap}); err != nil {
		return err
	}
	iso, err := isoMetadata(m)
	if err != nil {
		return err
	}

	gainImage := withSegments(gain.Bytes(),
		segment(0xE1, append([]byte(xmpNS+"\x00"), gainMapXMP(m)...)),
		segment(0xE2, append([]byte(isoURN+"\x00"), iso...)),
	)

	// The MPF index records each image's offset and size, which depend
	// on the primary's size, which includes the index; its size is
	// fixed, so build it once with placeholder values to measure.
	head := [][]byte{
		segment(0xE1, append([]byte(xmpNS+"\x00"), primaryXMP(len(gainImage))...)),
		segment(0xE2, []byte(isoURN+"\x00\x00\x00\x00\x00")), // version only: min 0, writer 0
	}
	mpfAt := 2
	for _, s := range head {
		mpfAt += len(s)
	}
	primaryLen := len(withSegments(base.Bytes(), append(head, segment(0xE2, mpf(0, 0, 0)))...))
	// Offsets in MPF are from the TIFF header, which follows the
	// marker, length and "MPF\0".
	tiffAt := mpfAt + 4 + 4
	head = append(head, segment(0xE2, mpf(primaryLen, len(gainImage), primaryLen-tiffAt)))
	primary := withSegments(base.Bytes(), head...)
	if len(primary) != primaryLen {
		return fmt.Errorf("internal error: primary image is %d bytes, expected %d", len(primary), primaryLen)
	}

	if _, err := w.Write(primary); err != nil {
		return err
	}
	_, err = w.Write(gainImage)
	return err
}

// segment returns a JPEG marker segment.
func segment(marker byte, body []byte) []byte {
	s := []byte{0xFF, marker, 0, 0}
	binary.BigEndian.PutUint16(s[2:], uint16(len(body)+2))
	return append(s, body...)
}

// withSegments inserts segments right after the SOI marker of a JPEG.
func withSegments(img []byte, segs ...[]byte) []byte {
	out := append([]byte{}, img[:2]...)
	for _, s := range segs {
		out = append(out, s...)
	}
	return append(out, img[2:]...)
}

// mpf returns the body of an MPF APP2 segment indexing a primary image
// of primaryLen bytes at offset 0 and a gain map of gainLen bytes at
// gainOffset (from the MPF TIFF header).
func mpf(primaryLen, gainLen, gainOffset int) []byte {
	const entries = 3
	b := []byte("MPF\x00MM\x00\x2A\x00\x00\x00\x08") // big-endian TIFF header, IFD at 8
	be := binary.BigEndian
	b = be.AppendUint16(b, entries)
	entryOffset := 8 + 2 + entries*12 + 4 // after the IFD and its next-IFD pointer
	tag := func(id, typ uint16, count, value uint32) {
		b = be.AppendUint16(b, id)
		b = be.AppendUint16(b, typ)
		b = be.AppendUint32(b, count)
		b = be.AppendUint32(b, value)
	}
	const undefined, long = 7, 4
	tag(0xB000, undefined, 4, binary.BigEndian.Uint32([]byte("0100"))) // MPFVersion
	tag(0xB001, long, 1, 2)                                            // NumberOfImages
	tag(0xB002, undefined, 32, uint32(entryOffset))                    // MPEntry
	b = be.AppendUint32(b, 0)                                          // no next IFD
	// MP entries: attribute, size, offset, two dependent-image numbers.
	b = be.AppendUint32(b, 0x030000) // baseline MP primary image
	b = be.AppendUint32(b, uint32(primaryLen))
	b = be.AppendUint32(b, 0)
	b = be.AppendUint32(b, 0)
	b = be.AppendUint32(b, 0)
	b = be.AppendUint32(b, uint32(gainLen))
	b = be.AppendUint32(b, uint32(gainOffset))
	b = be.AppendUint32(b, 0)
	return b
}

func primaryXMP(gainLen int) []byte {
	return []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="videoencoder">
 <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
  <rdf:Description rdf:about=""
    xmlns:Container="http://ns.google.com/photos/1.0/container/"
    xmlns:Item="http://ns.google.com/photos/1.0/container/item/"
    xmlns:hdrgm="http://ns.adobe.com/hdr-gain-map/1.0/"
   hdrgm:Version="1.0">
   <Container:Directory>
    <rdf:Seq>
     <rdf:li rdf:parseType="Resource">
      <Container:Item Item:Semantic="Primary" Item:Mime="image/jpeg"/>
     </rdf:li>
     <rdf:li rdf:parseType="Resource">
      <Container:Item Item:Semantic="GainMap" Item:Mime="image/jpeg" Item:Length="` + strconv.Itoa(gainLen) + `"/>
     </rdf:li>
    </rdf:Seq>
   </Container:Directory>
  </rdf:Description>
 </rdf:RDF>
</x:xmpmeta>`)
}

func gainMapXMP(m Metadata) []byte {
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	return []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="videoencoder">
 <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
  <rdf:Description rdf:about=""
    xmlns:hdrgm="http://ns.adobe.com/hdr-gain-map/1.0/"
   hdrgm:Version="1.0"
   hdrgm:GainMapMin="` + f(m.GainMapMin) + `"
   hdrgm:GainMapMax="` + f(m.GainMapMax) + `"
   hdrgm:Gamma="` + f(m.Gamma) + `"
   hdrgm:OffsetSDR="` + f(m.OffsetSDR) + `"
   hdrgm:OffsetHDR="` + f(m.OffsetHDR) + `"
   hdrgm:HDRCapacityMin="` + f(m.HDRCapacityMin) + `"
   hdrgm:HDRCapacityMax="` + f(m.HDRCapacityMax) + `"
   hdrgm:BaseRenditionIsHDR="False"/>
 </rdf:RDF>
</x:xmpmeta>`)
}

// isoMetadata encodes m as ISO 21496-1 version 0 binary metadata: a
// single channel, computed in the base image's color space, every
// value a fraction.
func isoMetadata(m Metadata) ([]byte, error) {
	be := binary.BigEndian
	b := be.AppendUint16(nil, 0) // minimum version
	b = be.AppendUint16(b, 0)    // writer version
	b = append(b, 0x40)          // flags: one channel, use base color space
	for _, v := range []float64{m.HDRCapacityMin, m.HDRCapacityMax} {
		n, d, err := unsignedFraction(v)
		if err != nil {
			return nil, err
		}
		b = be.AppendUint32(be.AppendUint32(b, n), d)
	}
	for i, v := range []float64{m.GainMapMin, m.GainMapMax, m.Gamma, m.OffsetSDR, m.OffsetHDR} {
		if i == 2 {
			n, d, err := unsignedFraction(v)
			if err != nil {
				return nil, err
			}
			b = be.AppendUint32(be.AppendUint32(b, n), d)
			continue
		}
		n, d, err := signedFraction(v)
		if err != nil {
			return nil, err
		}
		b = be.AppendUint32(be.AppendUint32(b, uint32(n)), d)
	}
	return b, nil
}

// signedFraction represents v as n/d with the largest power-of-two d
// that keeps n in an int32.
func signedFraction(v float64) (int32, uint32, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) >= 1<<30 {
		return 0, 0, fmt.Errorf("can't represent %v as a fraction", v)
	}
	d := uint32(1 << 30)
	for math.Abs(v)*float64(d) >= 1<<31-1 {
		d >>= 1
	}
	return int32(math.Round(v * float64(d))), d, nil
}

func unsignedFraction(v float64) (uint32, uint32, error) {
	if v < 0 {
		return 0, 0, fmt.Errorf("can't represent %v as an unsigned fraction", v)
	}
	n, d, err := signedFraction(v)
	return uint32(n), d, err
}
