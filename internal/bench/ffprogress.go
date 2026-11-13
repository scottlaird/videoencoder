package bench

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"time"
)

// Progress is how far a running encode has got.
type Progress struct {
	// Encoded is how much of the sample the encode has written, and
	// Total the sample's duration.
	Encoded, Total time.Duration
	// Elapsed is the encode's wall time so far.
	Elapsed time.Duration
}

// Fraction is the share of the sample encoded, from 0 to 1, or 0 if
// the sample's duration is unknown.
func (p Progress) Fraction() float64 {
	if p.Total <= 0 {
		return 0
	}
	return min(max(float64(p.Encoded)/float64(p.Total), 0), 1)
}

// Remaining estimates the time left, assuming the rest goes as fast
// as the part so far. ok is false until there's enough to go on: some
// of the sample encoded and a few seconds spent.
func (p Progress) Remaining() (d time.Duration, ok bool) {
	f := p.Fraction()
	if f <= 0.01 || p.Elapsed < 3*time.Second {
		return 0, false
	}
	return time.Duration(float64(p.Elapsed) * (1 - f) / f), true
}

// readFFmpegProgress reads ffmpeg's -progress output, blocks of
// key=value lines each ending with a "progress" key, and calls report
// with each block's out_time_us. It returns at EOF.
func readFFmpegProgress(r io.Reader, report func(encoded time.Duration)) {
	sc := bufio.NewScanner(r)
	var encoded time.Duration
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "out_time_us":
			// "N/A" until the first frame is out.
			if us, err := strconv.ParseInt(v, 10, 64); err == nil {
				encoded = time.Duration(us) * time.Microsecond
			}
		case "progress":
			report(encoded)
		}
	}
}
