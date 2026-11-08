package tui

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/scottlaird/videoencoder/internal/progress"
)

var t0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func at(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

// plain renders without color, for comparing text.
func plain() styles {
	r := lipgloss.NewRenderer(nil)
	r.SetColorProfile(termenv.Ascii)
	return newStyles(r)
}

// run is a run of 15 videos: 5 done, the 6th tuning, 9 to come.
func run(t *testing.T) screen {
	t.Helper()
	var tr progress.Tracker
	apply := func(e progress.Event) { tr.Apply(e) }
	for i := range 15 {
		apply(progress.Event{Kind: progress.VideoQueued, Video: fmt.Sprintf("sample-%02d", i), Time: at(0)})
	}
	for i := range 5 {
		v := fmt.Sprintf("sample-%02d", i)
		apply(progress.Event{Kind: progress.VideoStarted, Video: v, Time: at(i * 100)})
		apply(progress.Event{Kind: progress.VideoDone, Video: v, Time: at(i*100 + 95)})
	}
	v := "sample-05"
	apply(progress.Event{Kind: progress.VideoStarted, Video: v, Time: at(500)})
	for _, enc := range []string{"av1", "h264", "h265"} {
		for _, h := range []int{480, 720, 1080, 1440, 2160} {
			apply(progress.Event{Kind: progress.TaskQueued, Video: v, Task: fmt.Sprintf("sdr/%s-%d", enc, h), Detail: "search", Time: at(500)})
		}
	}
	apply(progress.Event{Kind: progress.TaskDone, Video: v, Task: "sdr/av1-480", Detail: "crf 31 · VMAF 96.1", Time: at(560)})
	apply(progress.Event{Kind: progress.TaskStarted, Video: v, Task: "sdr/av1-1440", Time: at(560)})
	apply(progress.Event{Kind: progress.TaskUpdated, Video: v, Task: "sdr/av1-1440", Detail: "trial 2: crf 34 → VMAF 95.2", Time: at(590)})
	apply(progress.Event{Kind: progress.TaskDone, Video: v, Task: "sdr/av1-720", Skipped: true, Detail: "tune: false", Time: at(500)})
	var logs []string
	for i := range 50 {
		logs = append(logs, fmt.Sprintf("log line %d", i))
	}
	return screen{title: "videoencoder tune", started: at(0), now: at(600), videos: tr.Snapshot(), logs: logs, status: "frame=  120 fps= 30"}
}

func TestRender(t *testing.T) {
	s := run(t)
	for _, size := range []struct{ w, h int }{{120, 40}, {80, 24}, {60, 12}, {40, 6}} {
		out := render(s, plain(), size.w, size.h)
		lines := strings.Split(out, "\n")
		if len(lines) > size.h {
			t.Errorf("%dx%d: %d lines, more than fit", size.w, size.h, len(lines))
		}
		for i, l := range lines {
			if w := ansi.StringWidth(l); w > size.w {
				t.Errorf("%dx%d: line %d is %d wide: %q", size.w, size.h, i, w, l)
			}
		}
	}

	out := render(s, plain(), 120, 40)
	for _, want := range []string{
		"videoencoder tune · 5/15 videos · 10m00s",
		"▶ sample-05", "1m40s", // running video and its elapsed time
		"✓ sample-04", "1m35s", // finished, with its total
		"· sample-06",                                   // to come
		"▶ sdr/av1-1440", "trial 2: crf 34 → VMAF 95.2", // running search, its latest trial
		"✓ sdr/av1-480", "crf 31 · VMAF 96.1",
		"– sdr/av1-720", "tune: false",
		"· sdr/h265-2160",
		"log line 49", "frame=  120 fps= 30", // the log's end, and ffmpeg's status
	} {
		if !strings.Contains(out, want) {
			t.Errorf("120x40 frame lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "log line 0\n") {
		t.Errorf("120x40 frame shows the log's start, which can't fit:\n%s", out)
	}

	// With little room, lists say how much they left out.
	small := render(s, plain(), 80, 20)
	if !strings.Contains(small, "more to come") {
		t.Errorf("80x20 frame doesn't count the hidden items:\n%s", small)
	}
}

func TestFit(t *testing.T) {
	line := func(s string) string { return s }
	more := func(what string) func(int) string {
		return func(n int) string { return fmt.Sprintf("+%d %s", n, what) }
	}
	done := []string{"d1", "d2", "d3", "d4"}
	running := []string{"r1"}
	queued := []string{"q1", "q2", "q3", "q4", "q5"}
	for room := 0; room <= 12; room++ {
		got := fit(done, running, queued, room, line, more("done"), more("queued"))
		if len(got) > room {
			t.Errorf("room %d: %d lines: %q", room, len(got), got)
		}
		if room >= 1 && !strings.Contains(strings.Join(got, " "), "r1") && room > 2 {
			t.Errorf("room %d: running item missing: %q", room, got)
		}
		if room >= len(done)+len(running)+len(queued) && len(got) != 10 {
			t.Errorf("room %d: %q, want everything", room, got)
		}
	}
	got := fit(done, running, queued, 6, line, more("done"), more("queued"))
	want := "+3 done d4 r1 q1 q2 +3 queued"
	if strings.Join(got, " ") != want {
		t.Errorf("fit(room 6) = %q, want %q", strings.Join(got, " "), want)
	}
}

func TestLineWriter(t *testing.T) {
	var lines, statuses []string
	var file strings.Builder
	w := &lineWriter{file: &file, line: func(s string) { lines = append(lines, s) }, status: func(s string) { statuses = append(statuses, s) }}
	fmt.Fprint(w, "first\nframe=  10 fps=1\rframe=  20 fps=2\r")
	fmt.Fprint(w, "sec")
	fmt.Fprint(w, "ond\nunterminated")
	w.Flush()
	if want := []string{"first", "second", "unterminated"}; strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Errorf("lines = %q, want %q", lines, want)
	}
	if want := []string{"frame=  10 fps=1", "frame=  20 fps=2"}; strings.Join(statuses, "|") != strings.Join(want, "|") {
		t.Errorf("statuses = %q, want %q", statuses, want)
	}
	if file.String() != "first\nsecond\nunterminated\n" {
		t.Errorf("file = %q; want the lines, without statuses", file.String())
	}
}

func TestPrintSummary(t *testing.T) {
	var tr progress.Tracker
	for _, e := range []progress.Event{
		{Kind: progress.VideoQueued, Video: "a"}, {Kind: progress.VideoQueued, Video: "b"},
		{Kind: progress.VideoQueued, Video: "c"}, {Kind: progress.VideoQueued, Video: "d"},
		{Kind: progress.VideoStarted, Video: "a", Time: at(0)}, {Kind: progress.VideoDone, Video: "a", Time: at(252)},
		{Kind: progress.VideoStarted, Video: "b", Time: at(252)}, {Kind: progress.VideoDone, Video: "b", Time: at(260), Err: errors.New("boom")},
		{Kind: progress.VideoStarted, Video: "c", Time: at(260)},
	} {
		tr.Apply(e)
	}
	var out strings.Builder
	printSummary(&out, tr.Snapshot(), at(0), at(300))
	for _, want := range []string{"2/4 videos in 5m00s", "✓ a", "4m12s", "✗ b", "boom", "! c", "stopped", "· d", "not started"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, out.String())
		}
	}
}

func TestDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0s", 42 * time.Second: "42s", 252 * time.Second: "4m12s",
		time.Hour + 3*time.Minute + 20*time.Second: "1h03m", 1500 * time.Millisecond: "2s",
	} {
		if got := duration(d); got != want {
			t.Errorf("duration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestLogHandler(t *testing.T) {
	var buf strings.Builder
	log := slog.New(LogHandler(&buf))
	log.Info("Trial", "video", "a")
	log.Warn("Odd", "video", "b")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	if !regexp.MustCompile(`^time=\d\d:\d\d:\d\d msg=Trial video=a$`).MatchString(lines[0]) {
		t.Errorf("info line = %q, want the time of day and no level", lines[0])
	}
	if !strings.Contains(lines[1], "level=WARN msg=Odd") {
		t.Errorf("warning line = %q, want its level", lines[1])
	}
}

func TestPruneLogs(t *testing.T) {
	dir := t.TempDir()
	for i := range 5 {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("2026110%d-120000.log", i)), nil, 0o644)
	}
	os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o644)
	pruneLogs(dir, 3)
	got, _ := filepath.Glob(filepath.Join(dir, "*"))
	for i := range got {
		got[i] = filepath.Base(got[i])
	}
	want := []string{"20261102-120000.log", "20261103-120000.log", "20261104-120000.log", "notes.txt"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("after pruning: %q, want %q", got, want)
	}
}
