package ffmpeg

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Progress is how far a running ffmpeg command has got.
type Progress struct {
	// Encoded is how much of the input the command has written, and
	// Total the input's duration.
	Encoded, Total time.Duration
	// Elapsed is the command's wall time so far.
	Elapsed time.Duration
}

// Fraction is the share of the input encoded, from 0 to 1, or 0 if
// the input's duration is unknown.
func (p Progress) Fraction() float64 {
	if p.Total <= 0 {
		return 0
	}
	return min(max(float64(p.Encoded)/float64(p.Total), 0), 1)
}

// Remaining estimates the time left, assuming the rest goes as fast
// as the part so far. ok is false until there's enough to go on: some
// of the input encoded and a few seconds spent.
func (p Progress) Remaining() (d time.Duration, ok bool) {
	f := p.Fraction()
	if f <= 0.01 || p.Elapsed < 3*time.Second {
		return 0, false
	}
	return time.Duration(float64(p.Elapsed) * (1 - f) / f), true
}

// String says how far the command has got, for people: "15% · 2:00,
// about 12:30 left".
func (p Progress) String() string {
	s := fmt.Sprintf("%d%% · %s", int(p.Fraction()*100), clock(p.Elapsed))
	if left, ok := p.Remaining(); ok {
		s += ", about " + clock(left) + " left"
	}
	return s
}

// clock writes d as m:ss, or h:mm:ss from an hour.
func clock(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// ProgressArgs are the options that make ffmpeg write its progress to
// fd 3 (the command's ExtraFiles[0]) once a second, for ReadProgress.
// They go right after the command name.
var ProgressArgs = []string{"-progress", "pipe:3", "-stats_period", "1"}

// ReadProgress reads ffmpeg's -progress output, blocks of key=value
// lines each ending with a "progress" key, and calls report with each
// block's out_time_us and frame count, and end true for the last
// ("progress=end"). It returns at EOF.
func ReadProgress(r io.Reader, report func(encoded time.Duration, frames int64, end bool)) {
	sc := bufio.NewScanner(r)
	var encoded time.Duration
	var frames int64
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
		case "frame":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				frames = n
			}
		case "progress":
			report(encoded, frames, v == "end")
		}
	}
}

// A ProgressRunner is a Runner that can also run an ffmpeg command
// reporting how far it has got through its input, which lasts total.
// progress is called every second or so while the command runs, and
// never after RunProgress returns.
type ProgressRunner interface {
	Runner
	RunProgress(ctx context.Context, argv []string, total time.Duration, progress func(Progress)) error
}

// RunProgress runs argv as Run does, with -progress writing to a pipe
// that it reads to report progress. Commands other than ffmpeg just
// run.
func (r ExecRunner) RunProgress(ctx context.Context, argv []string, total time.Duration, progress func(Progress)) error {
	if len(argv) == 0 || filepath.Base(argv[0]) != "ffmpeg" {
		return r.Run(ctx, argv)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	defer pr.Close()
	argv = slices.Insert(slices.Clone(argv), 1, ProgressArgs...)
	start := time.Now()
	started := false
	done := make(chan struct{})
	err = r.run(ctx, argv, pw, func() {
		started = true
		go func() {
			defer close(done)
			ReadProgress(pr, func(encoded time.Duration, _ int64, end bool) {
				// The last report's out_time stops a frame short of
				// the end; it's done.
				if end && total > 0 {
					encoded = total
				}
				progress(Progress{Encoded: encoded, Total: total, Elapsed: time.Since(start)})
			})
		}()
	})
	if started {
		<-done
	}
	return err
}
