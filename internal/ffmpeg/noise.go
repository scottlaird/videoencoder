package ffmpeg

import (
	"bytes"
	"io"
	"sync"
)

// noise are substrings of stderr lines that say nothing useful:
//
//   - Shaka Packager logs NOTIMPLEMENTED, at error level, for sample
//     group types it skips, such as the ones ffmpeg writes for AAC,
//     on every run. Its --minloglevel would hide its real errors too.
//   - Newer ffmpeg warns, once per output, that a re-encode drops the
//     source's stream metadata, such as a camera's vendor_id, which it
//     should; the option it suggests doesn't exist in older builds.
var noise = [][]byte{
	[]byte("] NOTIMPLEMENTED"),
	[]byte("Discarding stream metadata '"),
}

// dropNoise passes writes to w, a line at a time, leaving out the lines
// that contain noise. A line ends at a newline or a carriage return,
// which it keeps, so progress lines that rewrite themselves still do.
type dropNoise struct {
	mu  sync.Mutex
	w   io.Writer
	buf []byte
}

func newDropNoise(w io.Writer) *dropNoise { return &dropNoise{w: w} }

func (d *dropNoise) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.buf = append(d.buf, p...)
	for {
		i := bytes.IndexAny(d.buf, "\r\n")
		if i < 0 {
			break
		}
		if err := d.emit(d.buf[:i+1]); err != nil {
			return len(p), err
		}
		d.buf = d.buf[i+1:]
	}
	return len(p), nil
}

// Flush writes what's left of an unterminated last line.
func (d *dropNoise) Flush() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	err := d.emit(d.buf)
	d.buf = nil
	return err
}

func (d *dropNoise) emit(line []byte) error {
	if len(line) == 0 {
		return nil
	}
	for _, n := range noise {
		if bytes.Contains(line, n) {
			return nil
		}
	}
	_, err := d.w.Write(line)
	return err
}
