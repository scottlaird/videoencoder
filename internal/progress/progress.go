// Package progress reports what a run is doing: which videos it will
// process, which it's working on, and the tasks within each, such as
// rendition encodes or tune searches. Code that does the work reports
// events through the context; a Reporter turns them into log lines, a
// terminal UI, or nothing.
package progress

import (
	"context"
	"time"
)

// Kind is what an Event says happened.
type Kind int

const (
	// VideoQueued: the run will process Video.
	VideoQueued Kind = iota
	// VideoStarted: work on Video began.
	VideoStarted
	// VideoDone: work on Video ended, with Err if it failed.
	VideoDone
	// TaskQueued: Task, part of Video, is still to come.
	TaskQueued
	// TaskStarted: Task began.
	TaskStarted
	// TaskUpdated: Task is still running, and Detail says how far it is.
	TaskUpdated
	// TaskDone: Task ended, with Err if it failed, or Skipped if it
	// needed no work.
	TaskDone
)

// Event is one thing that happened. Tasks are named within their
// video, by a slot like "sdr/av1-1080" or a step like "posters".
type Event struct {
	Kind    Kind
	Time    time.Time
	Video   string
	Task    string
	Detail  string // the task's current state or result, for people
	Err     error
	Skipped bool
}

// Reporter receives events, possibly from several goroutines at once.
type Reporter interface {
	Report(Event)
}

type ctxKey struct{}

// WithReporter returns ctx carrying r, which Report sends events to.
func WithReporter(ctx context.Context, r Reporter) context.Context {
	return context.WithValue(ctx, ctxKey{}, r)
}

// Report sends e to ctx's reporter, if it has one, timestamping it if
// it isn't already.
func Report(ctx context.Context, e Event) {
	r, ok := ctx.Value(ctxKey{}).(Reporter)
	if !ok {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	r.Report(e)
}

// QueueVideo reports that video is to be processed.
func QueueVideo(ctx context.Context, video string) {
	Report(ctx, Event{Kind: VideoQueued, Video: video})
}

// StartVideo reports that work on video began.
func StartVideo(ctx context.Context, video string) {
	Report(ctx, Event{Kind: VideoStarted, Video: video})
}

// FinishVideo reports that work on video ended, failing if err isn't nil.
func FinishVideo(ctx context.Context, video string, err error) {
	Report(ctx, Event{Kind: VideoDone, Video: video, Err: err})
}

// QueueTask reports that task, part of video, is still to come.
func QueueTask(ctx context.Context, video, task, detail string) {
	Report(ctx, Event{Kind: TaskQueued, Video: video, Task: task, Detail: detail})
}

// StartTask reports that task began.
func StartTask(ctx context.Context, video, task, detail string) {
	Report(ctx, Event{Kind: TaskStarted, Video: video, Task: task, Detail: detail})
}

// UpdateTask reports how far a running task has got.
func UpdateTask(ctx context.Context, video, task, detail string) {
	Report(ctx, Event{Kind: TaskUpdated, Video: video, Task: task, Detail: detail})
}

// FinishTask reports that task ended, with its result in detail, or
// failing if err isn't nil.
func FinishTask(ctx context.Context, video, task, detail string, err error) {
	Report(ctx, Event{Kind: TaskDone, Video: video, Task: task, Detail: detail, Err: err})
}

// SkipTask reports that task needed no work, and why.
func SkipTask(ctx context.Context, video, task, detail string) {
	Report(ctx, Event{Kind: TaskDone, Video: video, Task: task, Detail: detail, Skipped: true})
}
