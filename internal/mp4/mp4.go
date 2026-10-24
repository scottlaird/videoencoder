// Package mp4 makes in-place fixes to ISO BMFF (MP4) files.
package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// ZeroTimes sets the creation and modification times in the movie,
// track and media headers (mvhd, tkhd, mdhd) to zero, in place.
//
// Shaka Packager stamps these with the wall clock, so packaging the
// same inputs twice gives files that differ only in those bytes. Zeroed,
// unchanged renditions are byte-identical across runs, so a sync to
// object storage can skip them. Nothing else in the file changes, and
// no box moves.
func ZeroTimes(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	err = walk(f, 0, st.Size(), func(typ string, body, end int64) error {
		switch typ {
		case "moov", "trak", "mdia":
			return errDescend
		case "mvhd", "tkhd", "mdhd":
			return zeroHeaderTimes(f, body)
		}
		return nil
	})
	return errors.Join(err, f.Close())
}

var errDescend = errors.New("descend")

// walk calls fn for each box in [start, end). If fn returns errDescend,
// walk visits the box's children.
func walk(r io.ReaderAt, start, end int64, fn func(typ string, body, end int64) error) error {
	var hdr [16]byte
	for pos := start; pos < end; {
		if end-pos < 8 {
			return fmt.Errorf("truncated box header at %d", pos)
		}
		if _, err := r.ReadAt(hdr[:8], pos); err != nil {
			return err
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		body := pos + 8
		switch size {
		case 0:
			size = end - pos
		case 1:
			if _, err := r.ReadAt(hdr[8:16], pos+8); err != nil {
				return err
			}
			size = int64(binary.BigEndian.Uint64(hdr[8:16]))
			body = pos + 16
		}
		if size < body-pos || pos+size > end {
			return fmt.Errorf("bad size %d for %q box at %d", size, typ, pos)
		}
		switch err := fn(typ, body, pos+size); {
		case errors.Is(err, errDescend):
			if err := walk(r, body, pos+size, fn); err != nil {
				return err
			}
		case err != nil:
			return err
		}
		pos += size
	}
	return nil
}

// zeroHeaderTimes zeroes the two times that open a full box of the
// mvhd/tkhd/mdhd kind: 32-bit each in version 0, 64-bit in version 1.
func zeroHeaderTimes(f *os.File, body int64) error {
	var v [1]byte
	if _, err := f.ReadAt(v[:], body); err != nil {
		return err
	}
	n := 8
	if v[0] == 1 {
		n = 16
	}
	_, err := f.WriteAt(make([]byte, n), body+4)
	return err
}
