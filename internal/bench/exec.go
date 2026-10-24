package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// Measurement is the resource use of one command.
type Measurement struct {
	Wall, User, Sys time.Duration
	MaxRSSKB        int64
}

// Executor runs a command and measures it. It exists so the bench can
// be tested without ffmpeg.
type Executor interface {
	Exec(ctx context.Context, argv []string) (Measurement, error)
}

// ExecExecutor runs commands as subprocesses.
type ExecExecutor struct {
	Stderr io.Writer
}

func (e ExecExecutor) Exec(ctx context.Context, argv []string) (Measurement, error) {
	if len(argv) == 0 {
		return Measurement{}, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stderr = e.Stderr
	start := time.Now()
	err := cmd.Run()
	m := Measurement{Wall: time.Since(start)}
	if ps := cmd.ProcessState; ps != nil {
		m.User = ps.UserTime()
		m.Sys = ps.SystemTime()
		m.MaxRSSKB = maxRSSKB(ps)
	}
	if err != nil {
		return m, fmt.Errorf("running %s: %w", argv[0], err)
	}
	return m, nil
}
