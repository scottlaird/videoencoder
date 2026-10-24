package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNeeds(t *testing.T) {
	const cmd = "ffmpeg -i in.mov out.mp4"
	old := time.Now().Add(-time.Hour)
	now := time.Now()

	tests := []struct {
		name       string
		status     string // "" means no status file
		output     bool
		inputTime  time.Time
		outputTime time.Time
		want       string
	}{
		{name: "never built", output: true, inputTime: old, outputTime: now, want: NoStatus},
		{name: "settings changed", status: "ffmpeg -crf 20", output: true, inputTime: old, outputTime: now, want: CommandChanged},
		{name: "status but no output", status: cmd, inputTime: old, want: OutputMissing},
		{name: "input touched since", status: cmd, output: true, inputTime: now, outputTime: old, want: InputNewer},
		{name: "same mtime is fresh", status: cmd, output: true, inputTime: old, outputTime: old, want: ""},
		{name: "up to date", status: cmd, output: true, inputTime: old, outputTime: now, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			in := filepath.Join(dir, "in.mov")
			out := filepath.Join(dir, "out.mp4")
			touch(t, in, tt.inputTime)
			if tt.output {
				touch(t, out, tt.outputTime)
			}
			if tt.status != "" {
				if err := os.WriteFile(out+".status", []byte(tt.status), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			got, err := Needs(in, out, cmd)
			if err != nil {
				t.Fatalf("Needs() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Needs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNeedsMissingInput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")
	touch(t, out, time.Now())
	if err := Record(out, "cmd"); err != nil {
		t.Fatal(err)
	}
	if _, err := Needs(filepath.Join(dir, "gone.mov"), out, "cmd"); err == nil {
		t.Error("Needs() with a missing input: got nil error, want one")
	}
}

func TestRecordAndClear(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mov")
	out := filepath.Join(dir, "out.mp4")
	touch(t, in, time.Now().Add(-time.Hour))
	touch(t, out, time.Now())

	if err := Record(out, "cmd"); err != nil {
		t.Fatal(err)
	}
	if got, _ := Needs(in, out, "cmd"); got != "" {
		t.Errorf("after Record: Needs() = %q, want up to date", got)
	}
	if err := Clear(out); err != nil {
		t.Fatal(err)
	}
	if got, _ := Needs(in, out, "cmd"); got != NoStatus {
		t.Errorf("after Clear: Needs() = %q, want %q", got, NoStatus)
	}
	if err := Clear(out); err != nil {
		t.Errorf("second Clear() = %v, want nil", err)
	}
}

func touch(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}
