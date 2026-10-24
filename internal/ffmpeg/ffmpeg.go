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
	"io/fs"
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

// Job produces one output from one input.
type Job struct {
	Input  string
	Output string
	// Base is the shared part of the command, including the input:
	// "ffmpeg ... -i input". Jobs batched together must have equal Base.
	Base []string
	// Args are the output options that precede Output on the command line.
	Args []string

	// Slot names what the output is for, such as "main/av1-2160"; Key
	// identifies how it is built and is part of Output's name. Source
	// identifies the input's content.
	Slot, Key, Source string
}

// Cmdline is the job's command, naming the final output rather than
// the partial file ffmpeg actually writes.
func (j *Job) Cmdline() string {
	return strings.Join(j.Base, " ") + " " + strings.Join(append(slices.Clone(j.Args), j.Output), " ")
}

// isPattern reports whether Output is an image2 sequence pattern such as
// "thumbs.KEY/%d.jpg". Its directory is the unit of output.
func (j *Job) isPattern() bool { return strings.Contains(filepath.Base(j.Output), "%") }

// Result is the file or directory that appears when the job finishes.
func (j *Job) Result() string {
	if j.isPattern() {
		return filepath.Dir(j.Output)
	}
	return j.Output
}

// partial is where result is written until it is complete. Files keep
// their extension so ffmpeg can still infer the container.
func (j *Job) partial() string {
	r := j.Result()
	if j.isPattern() {
		return r + ".partial"
	}
	ext := filepath.Ext(r)
	return strings.TrimSuffix(r, ext) + ".partial" + ext
}

// writePath is the output argument ffmpeg is given.
func (j *Job) writePath() string {
	if j.isPattern() {
		return filepath.Join(j.partial(), filepath.Base(j.Output))
	}
	return j.partial()
}

// Batch is a set of jobs run as a single ffmpeg process, recording
// what it builds in Manifest.
type Batch struct {
	Manifest *state.Manifest
	Jobs     []*Job
}

// AddIfNeeded adds j unless its output already exists, and reports
// whether it did.
func (b *Batch) AddIfNeeded(j *Job) (bool, error) {
	_, err := os.Stat(j.Result())
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	reason := b.Manifest.Explain(j.Slot, j.Key, j.Source, exists)
	if reason == state.UpToDate {
		slog.Info("Up to date", "slot", j.Slot, "output", filepath.Base(j.Result()))
		return false, nil
	}
	slog.Info("Building", "slot", j.Slot, "output", filepath.Base(j.Result()), "reason", reason)
	b.Jobs = append(b.Jobs, j)
	return true, nil
}

// Add adds j whether or not its output exists, replacing it.
func (b *Batch) Add(j *Job) {
	slog.Info("Building", "slot", j.Slot, "output", filepath.Base(j.Result()), "reason", "forced")
	os.RemoveAll(j.Result())
	b.Jobs = append(b.Jobs, j)
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
// names, and in the manifest, only if ffmpeg succeeds; on failure no
// partial output is left behind. Each output replaces, and deletes, the
// one previously built for its slot.
func (b *Batch) Run(ctx context.Context, r Runner) error {
	argv, err := b.Argv()
	if err != nil || argv == nil {
		return err
	}
	for _, j := range b.Jobs {
		dir := filepath.Dir(j.writePath())
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating output directory: %w", err)
		}
	}

	slog.Info("Running ffmpeg", "outputs", len(b.Jobs), "argv", argv)
	if err := r.Run(ctx, argv); err != nil {
		b.removePartials()
		return err
	}

	var errs []error
	for _, j := range b.Jobs {
		if err := os.Rename(j.partial(), j.Result()); err != nil {
			errs = append(errs, fmt.Errorf("finishing %q: %w", j.Result(), err))
			continue
		}
		errs = append(errs, b.Manifest.Record(j.Slot, state.Entry{
			Key:    j.Key,
			Output: filepath.Base(j.Result()),
			Source: j.Source,
			Args:   j.Cmdline(),
		}))
	}
	errs = append(errs, b.Manifest.Save())
	return errors.Join(errs...)
}

func (b *Batch) removePartials() {
	for _, j := range b.Jobs {
		os.RemoveAll(j.partial())
	}
}

// RunIfNeeded builds j on its own if it is stale, or if force is set.
func RunIfNeeded(ctx context.Context, r Runner, m *state.Manifest, j *Job, force bool) error {
	b := Batch{Manifest: m}
	if force {
		b.Add(j)
	} else if _, err := b.AddIfNeeded(j); err != nil {
		return err
	}
	return b.Run(ctx, r)
}
