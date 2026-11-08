package progress

import (
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// LogReporter turns events into log lines, for runs without a terminal
// UI: each video's position in the run when it starts, and how long it
// took when it ends. Task events are logged at debug level.
type LogReporter struct {
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	t      Tracker
}

func (l *LogReporter) Report(e Event) {
	l.t.Apply(e)
	log := l.Logger
	if log == nil {
		log = slog.Default()
	}
	switch e.Kind {
	case VideoStarted:
		videos := l.t.Snapshot()
		n := slices.IndexFunc(videos, func(v Video) bool { return v.Name == e.Video }) + 1
		log.Info("Video", "video", e.Video, "n", fmt.Sprintf("%d/%d", n, len(videos)))
	case VideoDone:
		videos := l.t.Snapshot()
		finished, total := Counts(videos)
		var took time.Duration
		if i := slices.IndexFunc(videos, func(v Video) bool { return v.Name == e.Video }); i >= 0 {
			took = videos[i].Elapsed(e.Time)
		}
		attrs := []any{"video", e.Video, "elapsed", took.Round(time.Second), "finished", fmt.Sprintf("%d/%d", finished, total)}
		if e.Err != nil {
			log.Error("Video failed", append(attrs, "err", e.Err)...)
		} else {
			log.Info("Video done", attrs...)
		}
	case TaskDone:
		if !e.Skipped {
			log.Debug("Task done", "video", e.Video, "task", e.Task, "detail", e.Detail, "err", e.Err)
		}
	}
}
