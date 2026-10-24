package ffmpeg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/scottlaird/videoencoder/internal/state"
)

// fakeRunner records commands and "encodes" by creating every output
// argument, i.e. each argument containing ".partial".
type fakeRunner struct {
	calls [][]string
	err   error
}

func (f *fakeRunner) Run(_ context.Context, argv []string) error {
	f.calls = append(f.calls, argv)
	for _, a := range argv {
		if !strings.Contains(a, ".partial") {
			continue
		}
		if strings.Contains(a, "%") {
			a = strings.Replace(a, "%d", "1", 1)
		}
		if err := os.WriteFile(a, []byte("encoded"), 0o644); err != nil {
			return err
		}
	}
	return f.err
}

func newJob(dir, slot, key string, args ...string) *Job {
	in := filepath.Join(dir, "in.mov")
	return &Job{
		Input:  in,
		Output: filepath.Join(dir, "out", slot+"."+key+".mp4"),
		Base:   []string{"ffmpeg", "-y", "-i", in},
		Args:   args,
		Slot:   slot,
		Key:    key,
		Source: "src1",
	}
}

func manifest(t *testing.T, dir string) *state.Manifest {
	t.Helper()
	m, err := state.Load(filepath.Join(dir, "out"), "v")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
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

func TestPaths(t *testing.T) {
	tests := []struct{ output, result, partial, write string }{
		{"d/video.k.mp4", "d/video.k.mp4", "d/video.k.partial.mp4", "d/video.k.partial.mp4"},
		{"d/thumbs.k/%d.jpg", "d/thumbs.k", "d/thumbs.k.partial", "d/thumbs.k.partial/%d.jpg"},
	}
	for _, tt := range tests {
		j := &Job{Output: tt.output}
		if j.Result() != tt.result || j.partial() != tt.partial || j.writePath() != tt.write {
			t.Errorf("%s: result %q partial %q write %q", tt.output, j.Result(), j.partial(), j.writePath())
		}
	}
}

func TestBatchRun(t *testing.T) {
	dir := t.TempDir()
	a := newJob(dir, "a", "k1", "-c:v", "libx264")
	b := newJob(dir, "b", "k1", "-c:v", "libx265")

	batch := Batch{Manifest: manifest(t, dir)}
	for _, j := range []*Job{a, b} {
		added, err := batch.AddIfNeeded(j)
		if err != nil || !added {
			t.Fatalf("AddIfNeeded(%s) = %v, %v; want true, nil", j.Slot, added, err)
		}
	}
	r := &fakeRunner{}
	if err := batch.Run(context.Background(), r); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	want := []string{
		"ffmpeg", "-y", "-i", filepath.Join(dir, "in.mov"),
		"-c:v", "libx264", filepath.Join(dir, "out", "a.k1.partial.mp4"),
		"-c:v", "libx265", filepath.Join(dir, "out", "b.k1.partial.mp4"),
	}
	if len(r.calls) != 1 || !slices.Equal(r.calls[0], want) {
		t.Errorf("ran %q, want one call %q", r.calls, want)
	}
	for _, j := range []*Job{a, b} {
		if !exists(j.Output) || exists(j.partial()) {
			t.Errorf("%s: output in place %v, partial left %v", j.Slot, exists(j.Output), exists(j.partial()))
		}
	}

	// A fresh run, with the manifest reloaded from disk, has nothing to do.
	again := Batch{Manifest: manifest(t, dir)}
	for _, j := range []*Job{a, b} {
		if added, err := again.AddIfNeeded(j); err != nil || added {
			t.Errorf("second AddIfNeeded(%s) = %v, %v; want false, nil", j.Slot, added, err)
		}
	}

	// New settings for b build b under a new name and delete the old one.
	b2 := newJob(dir, "b", "k2", "-c:v", "libx265", "-crf", "18")
	changed := Batch{Manifest: manifest(t, dir)}
	for _, j := range []*Job{a, b2} {
		if _, err := changed.AddIfNeeded(j); err != nil {
			t.Fatal(err)
		}
	}
	if len(changed.Jobs) != 1 || changed.Jobs[0] != b2 {
		t.Fatalf("after changing b: batch has %d jobs, want only b", len(changed.Jobs))
	}
	if reason := changed.Manifest.Explain("b", "k2", "src1", false); reason != state.SettingsChange {
		t.Errorf("Explain() = %q, want %q", reason, state.SettingsChange)
	}
	if err := changed.Run(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if exists(b.Output) || !exists(b2.Output) {
		t.Errorf("old output kept %v, new output present %v", exists(b.Output), exists(b2.Output))
	}
}

func TestBatchRunPattern(t *testing.T) {
	dir := t.TempDir()
	j := &Job{
		Input:  filepath.Join(dir, "in.mov"),
		Output: filepath.Join(dir, "out", "thumbs.k1", "%d.jpg"),
		Base:   []string{"ffmpeg", "-i", "in.mov"},
		Slot:   "thumbs", Key: "k1",
	}
	b := Batch{Manifest: manifest(t, dir)}
	if err := RunIfNeeded(context.Background(), &fakeRunner{}, b.Manifest, j, false); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "out", "thumbs.k1", "1.jpg")) || exists(filepath.Join(dir, "out", "thumbs.k1.partial")) {
		t.Error("pattern output wasn't moved into place as a directory")
	}
}

func TestBatchRunFailure(t *testing.T) {
	dir := t.TempDir()
	j := newJob(dir, "a", "k1")
	b := Batch{Manifest: manifest(t, dir)}
	if _, err := b.AddIfNeeded(j); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("ffmpeg exploded")
	if err := b.Run(context.Background(), &fakeRunner{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("Run() = %v, want %v", err, boom)
	}
	if exists(j.Output) || exists(j.partial()) {
		t.Error("output or partial exists after a failed run")
	}
	if got := manifest(t, dir).Explain("a", "k1", "src1", false); got != state.New {
		t.Errorf("after failure Explain() = %q, want %q", got, state.New)
	}
}

func TestBatchMismatchedBase(t *testing.T) {
	dir := t.TempDir()
	a := newJob(dir, "a", "k")
	b := newJob(dir, "b", "k")
	b.Base = []string{"ffmpeg", "-i", "other.mov"}
	batch := Batch{Jobs: []*Job{a, b}}
	if _, err := batch.Argv(); err == nil {
		t.Error("Argv() with different bases: got nil error, want one")
	}
}
