package ffmpeg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeRunner records commands and "encodes" by creating every output
// argument, i.e. each argument containing ".partial.".
type fakeRunner struct {
	calls [][]string
	err   error
}

func (f *fakeRunner) Run(_ context.Context, argv []string) error {
	f.calls = append(f.calls, argv)
	for _, a := range argv {
		if strings.Contains(a, ".partial.") {
			if err := os.WriteFile(a, []byte("encoded"), 0o644); err != nil {
				return err
			}
		}
	}
	return f.err
}

func newJob(dir, name string, args ...string) *Job {
	in := filepath.Join(dir, "in.mov")
	return &Job{
		Input:  in,
		Output: filepath.Join(dir, "out", name),
		Base:   []string{"ffmpeg", "-y", "-i", in},
		Args:   args,
	}
}

func writeInput(t *testing.T, dir string) {
	t.Helper()
	in := filepath.Join(dir, "in.mov")
	if err := os.WriteFile(in, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(in, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestCmdline(t *testing.T) {
	j := &Job{Output: "out.mp4", Base: []string{"ffmpeg", "-i", "in.mov"}, Args: []string{"-crf", "20"}}
	if got, want := j.Cmdline(), "ffmpeg -i in.mov -crf 20 out.mp4"; got != want {
		t.Errorf("Cmdline() = %q, want %q", got, want)
	}
	if len(j.Args) != 2 {
		t.Errorf("Cmdline() modified Args: %v", j.Args)
	}
}

func TestWritePath(t *testing.T) {
	tests := []struct{ output, want string }{
		{"d/video.mp4", "d/video.partial.mp4"},
		{"d/thumb-%d.jpg", "d/thumb-%d.jpg"},
		{"d/noext", "d/noext.partial"},
	}
	for _, tt := range tests {
		if got := (&Job{Output: tt.output}).writePath(); got != tt.want {
			t.Errorf("writePath(%q) = %q, want %q", tt.output, got, tt.want)
		}
	}
}

func TestBatchRun(t *testing.T) {
	dir := t.TempDir()
	writeInput(t, dir)
	a := newJob(dir, "a.mp4", "-c:v", "libx264")
	b := newJob(dir, "b.mp4", "-c:v", "libx265")

	var batch Batch
	for _, j := range []*Job{a, b} {
		added, err := batch.AddIfNeeded(j)
		if err != nil || !added {
			t.Fatalf("AddIfNeeded(%s) = %v, %v; want true, nil", j.Output, added, err)
		}
	}

	r := &fakeRunner{}
	if err := batch.Run(context.Background(), r); err != nil {
		t.Fatalf("Run() = %v", err)
	}

	want := []string{
		"ffmpeg", "-y", "-i", filepath.Join(dir, "in.mov"),
		"-c:v", "libx264", filepath.Join(dir, "out", "a.partial.mp4"),
		"-c:v", "libx265", filepath.Join(dir, "out", "b.partial.mp4"),
	}
	if len(r.calls) != 1 || !slices.Equal(r.calls[0], want) {
		t.Errorf("ran %q, want one call %q", r.calls, want)
	}
	for _, j := range []*Job{a, b} {
		if _, err := os.Stat(j.Output); err != nil {
			t.Errorf("output %s not in place: %v", j.Output, err)
		}
		if _, err := os.Stat(j.writePath()); err == nil {
			t.Errorf("partial %s left behind", j.writePath())
		}
	}

	// A second pass over the same jobs finds nothing to do.
	var again Batch
	for _, j := range []*Job{a, b} {
		if added, err := again.AddIfNeeded(j); err != nil || added {
			t.Errorf("second AddIfNeeded(%s) = %v, %v; want false, nil", j.Output, added, err)
		}
	}
	if err := again.Run(context.Background(), r); err != nil || len(r.calls) != 1 {
		t.Errorf("empty Run() = %v with %d calls, want nil and no new call", err, len(r.calls))
	}

	// Changing one job's settings rebuilds only that job.
	b2 := newJob(dir, "b.mp4", "-c:v", "libx265", "-crf", "18")
	var changed Batch
	for _, j := range []*Job{a, b2} {
		if _, err := changed.AddIfNeeded(j); err != nil {
			t.Fatal(err)
		}
	}
	if len(changed.Jobs) != 1 || changed.Jobs[0] != b2 {
		t.Errorf("after changing b: batch has %d jobs, want only b", len(changed.Jobs))
	}
}

func TestBatchRunFailure(t *testing.T) {
	dir := t.TempDir()
	writeInput(t, dir)
	j := newJob(dir, "a.mp4")

	var b Batch
	if _, err := b.AddIfNeeded(j); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("ffmpeg exploded")
	if err := b.Run(context.Background(), &fakeRunner{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("Run() = %v, want %v", err, boom)
	}
	for _, p := range []string{j.Output, j.writePath(), j.Output + ".status"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s exists after a failed run", p)
		}
	}
}

func TestBatchMismatchedBase(t *testing.T) {
	dir := t.TempDir()
	a := newJob(dir, "a.mp4")
	b := newJob(dir, "b.mp4")
	b.Base = []string{"ffmpeg", "-i", "other.mov"}
	batch := Batch{Jobs: []*Job{a, b}}
	if _, err := batch.Argv(); err == nil {
		t.Error("Argv() with different bases: got nil error, want one")
	}
}
