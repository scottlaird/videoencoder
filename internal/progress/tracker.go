package progress

import (
	"slices"
	"sync"
	"time"
)

// Status is where a video or task stands.
type Status int

const (
	Queued Status = iota
	Running
	Done
	Failed
	Skipped
)

// Task is one task's state.
type Task struct {
	Name    string
	Detail  string
	Status  Status
	Err     error
	Started time.Time // zero until it starts
	Ended   time.Time // zero until it ends
}

// Video is one video's state, and its tasks in the order they were
// first reported.
type Video struct {
	Name    string
	Status  Status
	Err     error
	Started time.Time
	Ended   time.Time
	Tasks   []Task
}

// Elapsed is how long v has run: until it ended, or until now.
func (v Video) Elapsed(now time.Time) time.Duration { return elapsed(v.Started, v.Ended, now) }

// Elapsed is how long t has run: until it ended, or until now.
func (t Task) Elapsed(now time.Time) time.Duration { return elapsed(t.Started, t.Ended, now) }

func elapsed(start, end, now time.Time) time.Duration {
	switch {
	case start.IsZero():
		return 0
	case end.IsZero():
		return now.Sub(start)
	default:
		return end.Sub(start)
	}
}

// Tracker keeps the state that a run's events describe. It's safe for
// concurrent use.
type Tracker struct {
	mu     sync.Mutex
	videos []*Video
}

func (t *Tracker) video(name string) *Video {
	i := slices.IndexFunc(t.videos, func(v *Video) bool { return v.Name == name })
	if i >= 0 {
		return t.videos[i]
	}
	v := &Video{Name: name}
	t.videos = append(t.videos, v)
	return v
}

func (v *Video) task(name string) *Task {
	i := slices.IndexFunc(v.Tasks, func(t Task) bool { return t.Name == name })
	if i < 0 {
		v.Tasks = append(v.Tasks, Task{Name: name})
		i = len(v.Tasks) - 1
	}
	return &v.Tasks[i]
}

// Apply updates the state with e.
func (t *Tracker) Apply(e Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := t.video(e.Video)
	switch e.Kind {
	case VideoQueued:
	case VideoStarted:
		v.Status, v.Started = Running, e.Time
	case VideoDone:
		v.Status, v.Ended, v.Err = Done, e.Time, e.Err
		if e.Err != nil {
			v.Status = Failed
		}
		if v.Started.IsZero() {
			v.Started = e.Time
		}
	case TaskQueued:
		task := v.task(e.Task)
		if task.Status == Queued {
			task.Detail = e.Detail
		}
	case TaskStarted:
		task := v.task(e.Task)
		task.Status, task.Started, task.Detail = Running, e.Time, e.Detail
	case TaskUpdated:
		task := v.task(e.Task)
		task.Detail = e.Detail
		if task.Status == Queued {
			task.Status, task.Started = Running, e.Time
		}
	case TaskDone:
		task := v.task(e.Task)
		task.Detail, task.Ended, task.Err = e.Detail, e.Time, e.Err
		switch {
		case e.Err != nil:
			task.Status = Failed
		case e.Skipped:
			task.Status = Skipped
		default:
			task.Status = Done
		}
		if task.Started.IsZero() && !e.Skipped {
			task.Started = e.Time
		}
	}
}

// Snapshot returns a copy of every video's state, in the order they
// were first reported.
func (t *Tracker) Snapshot() []Video {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Video, len(t.videos))
	for i, v := range t.videos {
		out[i] = *v
		out[i].Tasks = slices.Clone(v.Tasks)
	}
	return out
}

// Counts reports how many videos have finished (done or failed), and
// how many there are.
func Counts(videos []Video) (finished, total int) {
	for _, v := range videos {
		if v.Status == Done || v.Status == Failed {
			finished++
		}
	}
	return finished, len(videos)
}
