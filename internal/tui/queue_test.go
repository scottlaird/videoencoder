package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/scottlaird/videoencoder/internal/benchqueue"
)

// queue is a queue part way through: one file done, one running with
// a failure, a broken file, one changed mid-unit, and one to come.
func queue() queueScreen {
	u := func(file, flagset, sample string) benchqueue.Unit {
		return benchqueue.Unit{File: file, Flagset: flagset, Sample: sample}
	}
	var film []benchqueue.Unit
	for _, p := range []string{"fast", "slow", "slower", "veryslow"} {
		for i := range 5 {
			film = append(film, u("01-film.yaml", "x264-"+p+"-film", fmt.Sprintf("clip-%d.mov", i)))
		}
	}
	var logs []string
	for i := range 50 {
		logs = append(logs, fmt.Sprintf("log line %d", i))
	}
	return queueScreen{
		title: "encbench queue",
		now:   at(3600),
		state: benchqueue.State{
			Dir:     "work",
			Phase:   benchqueue.Running,
			Started: at(0),
			Pausing: true,
			Current: &benchqueue.Current{
				Unit: film[0], Started: at(3000), Round: 3, Sample: "clip-0@1080p.mov",
				Detail:  "crf 23.4 → VMAF 95.31 pass",
				Answers: []string{"clip-0@720p.mov crf 24 · 1800 kbps · VMAF 95.10"},
			},
			Files: []benchqueue.FileState{
				{Name: "00-h264.yaml", Done: 12},
				{Name: "01-film.yaml", Done: 3, Failed: 1, Remaining: film},
				{Name: "02-bad.yaml", Err: errors.New("parsing: yaml: line 3: did not find expected key")},
				{Name: "05-new.yaml", Pending: true},
				{Name: "10-h265.yaml", Remaining: []benchqueue.Unit{u("10-h265.yaml", "x265-slow", "clip-0.mov")}},
			},
			Recent: []benchqueue.Result{
				{Unit: u("00-h264.yaml", "x264-medium", "clip-4.mov"), Elapsed: 300e9, Answers: []string{"clip-4.mov crf 22 · 2400 kbps · VMAF 95.02"}},
				{Unit: u("01-film.yaml", "x264-fast-film", "clip-9.mov"), Elapsed: 60e9, Err: errors.New("encoder exploded")},
				{Unit: u("01-film.yaml", "x264-fast-film", "clip-8.mov"), Elapsed: 30e9, Err: context.Canceled},
			},
		},
		logs:   logs,
		status: "frame=  120 fps= 30",
	}
}

func TestRenderQueue(t *testing.T) {
	s := queue()
	for _, size := range []struct{ w, h int }{{140, 50}, {80, 24}, {60, 12}, {40, 6}, {20, 3}} {
		out := renderQueue(s, plain(), size.w, size.h)
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

	out := renderQueue(s, plain(), 140, 50)
	for _, want := range []string{
		"encbench queue · 15/37 units · running · pausing after this unit · 1h00m",
		"▶ x264-fast-film on clip-0.mov   10m00s  01-film.yaml",
		"round 3 · clip-0@1080p.mov · crf 23.4 → VMAF 95.31 pass",
		"✓ clip-0@720p.mov crf 24",
		"✓ 00-h264.yaml",
		"▶ 01-film.yaml", "3/24   1 failed",
		"✗ 02-bad.yaml   parsing: yaml: line 3",
		"… 05-new.yaml   counted after this unit",
		"· 10-h265.yaml",
		"next:", "· x264-fast-film on clip-1.mov  01-film.yaml",
		"✓ x264-medium on clip-4.mov",
		"✗ x264-fast-film on clip-9.mov    1m00s  encoder exploded",
		"! x264-fast-film on clip-8.mov", "abandoned",
		"log line 49", "frame=  120 fps= 30",
		"p resume · q quit after this unit · ctrl-c abandon it and quit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("140x50 frame lacks %q:\n%s", want, out)
		}
	}
	// The running unit isn't listed again among the next ones.
	if strings.Contains(out, "· x264-fast-film on clip-0.mov") {
		t.Errorf("running unit listed as next:\n%s", out)
	}

	// Short of room, errors and the running file still show, and the
	// units say how many they left out.
	small := renderQueue(s, plain(), 80, 24)
	for _, want := range []string{"02-bad.yaml", "▶ 01-film.yaml", "more units"} {
		if !strings.Contains(small, want) {
			t.Errorf("80x24 frame lacks %q:\n%s", want, small)
		}
	}
}

func TestRenderQueueIdle(t *testing.T) {
	for _, tt := range []struct {
		phase   benchqueue.Phase
		waiting string
		want    string
	}{
		{benchqueue.Waiting, "Resolve is rendering", "waiting: Resolve is rendering"},
		{benchqueue.Paused, "", "paused; p resumes"},
		{benchqueue.Idle, "", "nothing to run; add or edit a file in work"},
	} {
		s := queueScreen{title: "encbench queue", now: at(10), state: benchqueue.State{Dir: "work", Phase: tt.phase, Waiting: tt.waiting, Started: at(0)}}
		out := renderQueue(s, plain(), 100, 20)
		if !strings.Contains(out, tt.want) || !strings.Contains(out, "no work files in work") {
			t.Errorf("phase %v: frame lacks %q:\n%s", tt.phase, tt.want, out)
		}
	}
}
