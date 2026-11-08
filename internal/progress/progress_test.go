package progress

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func at(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

func TestTracker(t *testing.T) {
	var tr Tracker
	boom := errors.New("boom")
	for _, e := range []Event{
		{Kind: VideoQueued, Video: "a", Time: at(0)},
		{Kind: VideoQueued, Video: "b", Time: at(0)},
		{Kind: VideoStarted, Video: "a", Time: at(1)},
		{Kind: TaskQueued, Video: "a", Task: "sdr/av1-480", Detail: "search", Time: at(1)},
		{Kind: TaskQueued, Video: "a", Task: "sdr/av1-720", Detail: "interpolated", Time: at(1)},
		{Kind: TaskDone, Video: "a", Task: "sdr/h264-180", Detail: "tune: false", Skipped: true, Time: at(1)},
		{Kind: TaskStarted, Video: "a", Task: "sdr/av1-480", Time: at(2)},
		{Kind: TaskUpdated, Video: "a", Task: "sdr/av1-480", Detail: "trial 1", Time: at(5)},
		{Kind: TaskDone, Video: "a", Task: "sdr/av1-480", Detail: "crf 31", Time: at(9)},
		// Updating a queued task starts it.
		{Kind: TaskUpdated, Video: "a", Task: "sdr/av1-720", Detail: "trial 1", Time: at(10)},
		{Kind: TaskDone, Video: "a", Task: "sdr/av1-720", Err: boom, Time: at(12)},
		{Kind: VideoDone, Video: "a", Err: boom, Time: at(12)},
	} {
		tr.Apply(e)
	}
	videos := tr.Snapshot()
	if len(videos) != 2 || videos[0].Name != "a" || videos[1].Name != "b" {
		t.Fatalf("videos = %+v, want a then b", videos)
	}
	a, b := videos[0], videos[1]
	if a.Status != Failed || a.Err != boom || a.Elapsed(at(99)) != 11*time.Second {
		t.Errorf("a = %+v (elapsed %v), want failed after 11s", a, a.Elapsed(at(99)))
	}
	if b.Status != Queued || b.Elapsed(at(99)) != 0 {
		t.Errorf("b = %+v, want queued, not started", b)
	}
	want := []struct {
		name   string
		status Status
		detail string
		took   time.Duration
	}{
		{"sdr/av1-480", Done, "crf 31", 7 * time.Second},
		{"sdr/av1-720", Failed, "", 2 * time.Second},
		{"sdr/h264-180", Skipped, "tune: false", 0},
	}
	if len(a.Tasks) != len(want) {
		t.Fatalf("tasks = %+v, want %d", a.Tasks, len(want))
	}
	for i, w := range want {
		got := a.Tasks[i]
		if got.Name != w.name || got.Status != w.status || got.Detail != w.detail || got.Elapsed(at(99)) != w.took {
			t.Errorf("task %d = %+v (took %v), want %s %v %q %v", i, got, got.Elapsed(at(99)), w.name, w.status, w.detail, w.took)
		}
	}
	if finished, total := Counts(videos); finished != 1 || total != 2 {
		t.Errorf("Counts() = %d, %d; want 1, 2", finished, total)
	}

	// A snapshot is a copy: later events don't change it.
	tr.Apply(Event{Kind: TaskQueued, Video: "a", Task: "late", Time: at(13)})
	if len(a.Tasks) != 3 {
		t.Errorf("snapshot changed after a later event: %+v", a.Tasks)
	}
}

func TestLogReporter(t *testing.T) {
	var buf bytes.Buffer
	l := &LogReporter{Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))}
	ctx := WithReporter(context.Background(), l)
	for _, v := range []string{"a", "b", "c"} {
		QueueVideo(ctx, v)
	}
	l.Report(Event{Kind: VideoStarted, Video: "b", Time: at(0)})
	l.Report(Event{Kind: VideoDone, Video: "b", Time: at(75)})
	l.Report(Event{Kind: VideoStarted, Video: "c", Time: at(75)})
	l.Report(Event{Kind: VideoDone, Video: "c", Err: errors.New("boom"), Time: at(80)})
	got := buf.String()
	for _, want := range []string{
		`level=INFO msg=Video video=b n=2/3`,
		`level=INFO msg="Video done" video=b elapsed=1m15s finished=1/3`,
		`level=ERROR msg="Video failed" video=c elapsed=5s finished=2/3 err=boom`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
}

func TestReportWithoutReporter(t *testing.T) {
	// Reporting with no reporter in the context does nothing, safely.
	StartVideo(context.Background(), "a")
	FinishTask(context.Background(), "a", "t", "", nil)
}
