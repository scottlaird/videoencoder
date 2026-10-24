// Package ffmpeg runs ffmpeg jobs, rebuilding only stale outputs.
//
// Several jobs that read the same input can share one ffmpeg process,
// so the input is decoded once for all of them.
package ffmpeg

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

	"github.com/scottlaird/videoencoder/internal/state"
)

// Runner runs an external command given as an argument vector.
type Runner interface {
	Run(ctx context.Context, argv []string) error
}

// ExecRunner runs commands as subprocesses.
type ExecRunner struct {
	Stdout, Stderr io.Writer
}

func (r ExecRunner) Run(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = r.Stdout
	cmd.Stderr = r.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w", argv[0], err)
	}
	return nil
}

// Job produces one output file from one input.
type Job struct {
	Input  string
	Output string
	// Base is the shared part of the command, including the input:
	// "ffmpeg ... -i input". Jobs batched together must have equal Base.
	Base []string
	// Args are the output options that precede Output on the command line.
	Args []string
}

// Cmdline is the command that identifies how Output is built. It names
// the final output, not the partial file ffmpeg actually writes.
func (j *Job) Cmdline() string {
	return strings.Join(j.Base, " ") + " " + strings.Join(append(slices.Clone(j.Args), j.Output), " ")
}

// isPattern reports whether Output is an image2 sequence pattern such as
// "thumb-%d.jpg", which can't be written atomically.
func (j *Job) isPattern() bool { return strings.Contains(filepath.Base(j.Output), "%") }

// writePath is where ffmpeg writes the output until it is complete. It
// keeps the extension so ffmpeg can still infer the container.
func (j *Job) writePath() string {
	if j.isPattern() {
		return j.Output
	}
	ext := filepath.Ext(j.Output)
	return strings.TrimSuffix(j.Output, ext) + ".partial" + ext
}

// Batch is a set of jobs run as a single ffmpeg process.
type Batch struct {
	Jobs []*Job
}

// AddIfNeeded adds j unless its output is up to date, and reports
// whether it did.
func (b *Batch) AddIfNeeded(j *Job) (bool, error) {
	reason, err := state.Needs(j.Input, j.Output, j.Cmdline())
	if err != nil {
		return false, err
	}
	if reason == "" {
		slog.Info("Up to date", "output", j.Output)
		return false, nil
	}
	slog.Info("Rebuilding", "output", j.Output, "reason", reason)
	b.Jobs = append(b.Jobs, j)
	return true, nil
}

// Argv is the combined command line for every job in the batch.
func (b *Batch) Argv() ([]string, error) {
	if len(b.Jobs) == 0 {
		return nil, nil
	}
	first := b.Jobs[0]
	argv := slices.Clone(first.Base)
	for _, j := range b.Jobs {
		if !slices.Equal(first.Base, j.Base) {
			return nil, fmt.Errorf("can't batch %q with %q: base commands differ", j.Output, first.Output)
		}
		argv = append(argv, j.Args...)
		argv = append(argv, j.writePath())
	}
	return argv, nil
}

// Run builds every job in the batch. Outputs appear under their final
// names, with status recorded, only if ffmpeg succeeds; on failure no
// partial output is left behind looking finished.
func (b *Batch) Run(ctx context.Context, r Runner) error {
	argv, err := b.Argv()
	if err != nil || argv == nil {
		return err
	}
	for _, j := range b.Jobs {
		if err := os.MkdirAll(filepath.Dir(j.Output), 0o755); err != nil {
			return fmt.Errorf("creating output directory: %w", err)
		}
		if err := state.Clear(j.Output); err != nil {
			return err
		}
	}

	slog.Info("Running ffmpeg", "outputs", len(b.Jobs), "argv", argv)
	if err := r.Run(ctx, argv); err != nil {
		b.removePartials()
		return err
	}

	for _, j := range b.Jobs {
		if p := j.writePath(); p != j.Output {
			if err := os.Rename(p, j.Output); err != nil {
				return fmt.Errorf("finishing %q: %w", j.Output, err)
			}
		}
		if err := state.Record(j.Output, j.Cmdline()); err != nil {
			return err
		}
	}
	return nil
}

func (b *Batch) removePartials() {
	for _, j := range b.Jobs {
		if p := j.writePath(); p != j.Output {
			os.Remove(p)
		}
	}
}

// RunIfNeeded builds j on its own if it is stale.
func RunIfNeeded(ctx context.Context, r Runner, j *Job) error {
	var b Batch
	if _, err := b.AddIfNeeded(j); err != nil {
		return err
	}
	return b.Run(ctx, r)
}
