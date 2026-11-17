package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/scottlaird/videoencoder/internal/ffmpeg"
)

// Measurement is the resource use of one command.
type Measurement struct {
	Wall, User, Sys time.Duration
	MaxRSSKB        int64
	// CPU is what turbostat measured over an encode; nil without it
	// (see ExecExecutor.Turbostat).
	CPU *CPUStats
}

// Executor runs a command and measures it. It exists so the bench can
// be tested without ffmpeg.
type Executor interface {
	Exec(ctx context.Context, argv []string) (Measurement, error)
}

// An Encoder is an Executor that can also run an encode, an ffmpeg
// command, reporting how far it has got. total is the sample's
// duration; progress is called every second or so while the encode
// runs, and never after Encode returns.
type Encoder interface {
	Executor
	Encode(ctx context.Context, argv []string, total time.Duration, progress func(Progress)) (Measurement, error)
}

// A ProgressExecutor is an Executor that can also run an ffmpeg
// command reporting how far it has got through its input, which lasts
// total at fps frames a second. Unlike Encode it counts frames read
// out, not output time, so it suits commands whose output timestamps
// aren't the input's (scoring renumbers frames), and it never measures
// the machine. progress is called every second or so while the command
// runs, and never after ExecProgress returns.
type ProgressExecutor interface {
	Executor
	ExecProgress(ctx context.Context, argv []string, total time.Duration, fps float64, progress func(Progress)) (Measurement, error)
}

// ExecExecutor runs commands as subprocesses.
type ExecExecutor struct {
	Stderr io.Writer
	// Turbostat, if set, is the turbostat that encodes run under (see
	// FindTurbostat), to measure the machine's CPUs while they run.
	// Wall time then includes turbostat's start and finish, a few
	// milliseconds, and user and system time its own small share.
	Turbostat string
}

func (e ExecExecutor) Exec(ctx context.Context, argv []string) (Measurement, error) {
	if len(argv) == 0 {
		return Measurement{}, errors.New("empty command")
	}
	argv = ffmpeg.Quiet(argv)
	cmd, tail := e.command(ctx, argv)
	start := time.Now()
	return measure(cmd, argv[0], tail, start, cmd.Run())
}

// Encode runs the ffmpeg command argv with -progress writing to a
// pipe, which it reads to report progress.
func (e ExecExecutor) Encode(ctx context.Context, argv []string, total time.Duration, progress func(Progress)) (Measurement, error) {
	return e.runProgress(ctx, argv, total, 0, e.Turbostat, progress)
}

// ExecProgress is Encode, counting frames, and without turbostat.
func (e ExecExecutor) ExecProgress(ctx context.Context, argv []string, total time.Duration, fps float64, progress func(Progress)) (Measurement, error) {
	return e.runProgress(ctx, argv, total, fps, "", progress)
}

// runProgress runs argv under turbostat (if set) and reports progress
// by output time, or with fps set by frames.
func (e ExecExecutor) runProgress(ctx context.Context, argv []string, total time.Duration, fps float64, turbostat string, progress func(Progress)) (Measurement, error) {
	if len(argv) == 0 || filepath.Base(argv[0]) != "ffmpeg" {
		return e.Exec(ctx, argv)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return Measurement{}, err
	}
	defer pr.Close()
	// The child's fd 3 is ExtraFiles[0].
	argv = slices.Insert(ffmpeg.Quiet(argv), 1, ffmpeg.ProgressArgs...)
	// What failed, if it does, is argv[0], not the turbostat around it.
	name := argv[0]
	var stats string
	if turbostat != "" {
		f, err := os.CreateTemp("", "turbostat-*.txt")
		if err != nil {
			pw.Close()
			return Measurement{}, err
		}
		f.Close()
		stats = f.Name()
		defer os.Remove(stats)
		argv = turbostatArgv(turbostat, stats, argv)
	}
	cmd, tail := e.command(ctx, argv)
	cmd.ExtraFiles = []*os.File{pw}
	if stats != "" {
		// Canceling kills turbostat's ffmpeg too, not just turbostat.
		killGroup(cmd)
	}
	start := time.Now()
	err = cmd.Start()
	// The child has its own copy now; closing ours lets the reader see
	// EOF when ffmpeg exits.
	pw.Close()
	if err != nil {
		return measure(cmd, name, tail, start, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ffmpeg.ReadProgress(pr, func(encoded time.Duration, frames int64, end bool) {
			if fps > 0 {
				encoded = time.Duration(float64(frames) / fps * float64(time.Second))
			}
			// The last report's out_time stops a frame short of the
			// end; it's done.
			if end && total > 0 {
				encoded = total
			}
			progress(Progress{Encoded: encoded, Total: total, Elapsed: time.Since(start)})
		})
	}()
	err = cmd.Wait()
	<-done
	m, err := measure(cmd, name, tail, start, err)
	if stats != "" && err == nil {
		if m.CPU, err = readTurbostat(stats); err != nil {
			slog.Warn("Couldn't read turbostat's measurements", "err", err)
			err = nil
		}
	}
	return m, err
}

// command makes the exec.Cmd for argv, its stderr going to e.Stderr
// and to the tail it returns, for errors to quote.
func (e ExecExecutor) command(ctx context.Context, argv []string) (*exec.Cmd, *tailWriter) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = ffmpeg.QuietEnv()
	tail := &tailWriter{max: stderrTail}
	cmd.Stderr = tail
	if e.Stderr != nil {
		cmd.Stderr = io.MultiWriter(e.Stderr, tail)
	}
	return cmd, tail
}

// measure is cmd's resource use, once it has exited with err.
// measure is cmd's resource use, and err said in terms of name, the
// command cmd runs (perhaps under turbostat), quoting the end of its
// stderr from tail.
func measure(cmd *exec.Cmd, name string, tail *tailWriter, start time.Time, err error) (Measurement, error) {
	m := Measurement{Wall: time.Since(start)}
	if ps := cmd.ProcessState; ps != nil {
		m.User = ps.UserTime()
		m.Sys = ps.SystemTime()
		m.MaxRSSKB = maxRSSKB(ps)
	}
	if err == nil {
		return m, nil
	}
	msg := fmt.Sprintf("running %s", filepath.Base(name))
	if wrapped := name != cmd.Args[0]; wrapped {
		msg += " under " + filepath.Base(cmd.Args[0])
		// turbostat exits with its command's status, 128 plus the
		// signal for one that was killed.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if c := ee.ExitCode(); c > 128 && c < 128+32 {
				err = fmt.Errorf("%w (killed by %v)", err, syscall.Signal(c-128))
			}
		}
	}
	if last := tail.lastLines(stderrLines); last != "" {
		return m, fmt.Errorf("%s: %w: %s", msg, err, last)
	}
	return m, fmt.Errorf("%s: %w", msg, err)
}

// stderrTail is how much of a command's stderr is kept to quote, and
// stderrLines how many of its last lines are quoted.
const (
	stderrTail  = 4096
	stderrLines = 3
)

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

// lastLines is the last n non-blank lines kept, joined with " / ".
func (t *tailWriter) lastLines(n int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	lines := strings.Split(string(t.buf), "\n")
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			out = append(out, l)
		}
	}
	slices.Reverse(out)
	return strings.Join(out, " / ")
}
