package bench

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckFrameCounts(t *testing.T) {
	const bw, bh = 4, 2
	frame := bw * bh * 6
	dir := t.TempDir()
	write := func(name string, frames int) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, frames*frame), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		dist, ref int
		wantErr   bool
	}{
		{1799, 1799, false},
		{1798, 1799, false}, // an encode can end a frame or two differently
		{1801, 1799, false},
		{1796, 1799, true},
		{1802, 1799, true},
		{1673, 1799, true}, // FFmpeg 9.0.2 Theora on Birthday: 126 frames repeated by empty packets
	}
	for _, tt := range tests {
		d, r := write("d.blocks", tt.dist), write("r.blocks", tt.ref)
		err := checkFrameCounts(d, r, bw, bh)
		if got := err != nil; got != tt.wantErr {
			t.Errorf("checkFrameCounts(%d frames, %d frames) = %v, want error %v", tt.dist, tt.ref, err, tt.wantErr)
		}
	}
	if err := checkFrameCounts(filepath.Join(dir, "missing"), filepath.Join(dir, "r.blocks"), bw, bh); err == nil {
		t.Error("checkFrameCounts with a missing block image = nil, want an error")
	}
}
