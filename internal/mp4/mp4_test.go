package mp4

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func box(typ string, body ...[]byte) []byte {
	b := bytes.Join(body, nil)
	out := binary.BigEndian.AppendUint32(nil, uint32(8+len(b)))
	return append(append(out, typ...), b...)
}

// header is a full box body: version, flags, two times, then filler
// standing in for the rest of the header.
func header(version byte, ctime, mtime uint64) []byte {
	b := []byte{version, 0, 0, 0}
	if version == 1 {
		b = binary.BigEndian.AppendUint64(b, ctime)
		b = binary.BigEndian.AppendUint64(b, mtime)
	} else {
		b = binary.BigEndian.AppendUint32(b, uint32(ctime))
		b = binary.BigEndian.AppendUint32(b, uint32(mtime))
	}
	return append(b, bytes.Repeat([]byte{0xEE}, 12)...)
}

// file is a minimal MP4 whose movie, track and media headers have
// creation time ctime and modification times ctime+step, ctime+2*step
// and ctime+3*step, and whose sidx carries sidx in the same position.
func file(ctime, step, sidx uint64) []byte {
	return bytes.Join([][]byte{
		box("ftyp", []byte("isom\x00\x00\x00\x00")),
		box("moov",
			box("mvhd", header(0, ctime, ctime+step)),
			box("trak",
				box("tkhd", header(1, ctime, ctime+2*step)),
				box("mdia", box("mdhd", header(0, ctime, ctime+3*step)), box("hdlr", []byte("vide"))),
			),
		),
		box("sidx", header(0, sidx, sidx)), // not a movie header: untouched
		box("mdat", []byte("frames")),
	}, nil)
}

func TestZeroTimes(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.mp4"), filepath.Join(dir, "b.mp4")
	if err := os.WriteFile(a, file(3_900_000_000, 1, 42), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, file(3_900_000_777, 5, 42), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{a, b} {
		if err := ZeroTimes(p); err != nil {
			t.Fatalf("ZeroTimes(%s) = %v", p, err)
		}
	}
	want := file(0, 0, 42) // sidx keeps its values
	for _, p := range []string{a, b} {
		got, _ := os.ReadFile(p)
		if !bytes.Equal(got, want) {
			t.Errorf("%s zeroed =\n %x\nwant\n %x", filepath.Base(p), got, want)
		}
	}
}

func TestZeroTimesErrors(t *testing.T) {
	dir := t.TempDir()
	tests := map[string][]byte{
		"truncated header": {0, 0, 0},
		"box past the end": box("moov", box("mvhd", header(0, 1, 1)))[:20],
	}
	for name, data := range tests {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ZeroTimes(p); err == nil {
			t.Errorf("%s: ZeroTimes = nil error", name)
		}
	}
	if err := ZeroTimes(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file: nil error")
	}
}
