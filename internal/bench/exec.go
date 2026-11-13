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
	cmd := e.command(ctx, argv)
	start := time.Now()
	return measure(cmd, start, cmd.Run())
}

// Encode runs the ffmpeg command argv with -progress writing to a
// pipe, which it reads to report progress.
func (e ExecExecutor) Encode(ctx context.Context, argv []string, total time.Duration, progress func(Progress)) (Measurement, error) {
	if len(argv) == 0 || filepath.Base(argv[0]) != "ffmpeg" {
		return e.Exec(ctx, argv)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return Measurement{}, err
	}
	defer pr.Close()
	// The child's fd 3 is ExtraFiles[0].
	argv = slices.Insert(ffmpeg.Quiet(argv), 1, "-progress", "pipe:3", "-stats_period", "1")
	var stats string
	if e.Turbostat != "" {
		f, err := os.CreateTemp("", "turbostat-*.txt")
		if err != nil {
			pw.Close()
			return Measurement{}, err
		}
		f.Close()
		stats = f.Name()
		defer os.Remove(stats)
		argv = turbostatArgv(e.Turbostat, stats, argv)
	}
	cmd := e.command(ctx, argv)
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
		return measure(cmd, start, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		readFFmpegProgress(pr, func(encoded time.Duration, end bool) {
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
	m, err := measure(cmd, start, err)
	if stats != "" && err == nil {
		if m.CPU, err = readTurbostat(stats); err != nil {
			slog.Warn("Couldn't read turbostat's measurements", "err", err)
			err = nil
		}
	}
	return m, err
}

func (e ExecExecutor) command(ctx context.Context, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = ffmpeg.QuietEnv()
	cmd.Stderr = e.Stderr
	return cmd
}

// measure is cmd's resource use, once it has exited with err.
func measure(cmd *exec.Cmd, start time.Time, err error) (Measurement, error) {
	m := Measurement{Wall: time.Since(start)}
	if ps := cmd.ProcessState; ps != nil {
		m.User = ps.UserTime()
		m.Sys = ps.SystemTime()
		m.MaxRSSKB = maxRSSKB(ps)
	}
	if err != nil {
		return m, fmt.Errorf("running %s: %w", cmd.Args[0], err)
	}
	return m, nil
}
