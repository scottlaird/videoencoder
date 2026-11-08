package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/scottlaird/videoencoder/internal/progress"
)

// styles color a line by its status. They come from a renderer for the
// UI's own output, so a redirected stdout doesn't turn colors off.
type styles struct {
	title, heading, done, running, failed, dim lipgloss.Style
}

func newStyles(r *lipgloss.Renderer) styles {
	return styles{
		title:   r.NewStyle().Bold(true),
		heading: r.NewStyle().Bold(true).Foreground(lipgloss.Color("12")),
		done:    r.NewStyle().Foreground(lipgloss.Color("2")),
		running: r.NewStyle().Bold(true).Foreground(lipgloss.Color("3")),
		failed:  r.NewStyle().Bold(true).Foreground(lipgloss.Color("1")),
		dim:     r.NewStyle().Faint(true),
	}
}

// screen is everything one frame shows.
type screen struct {
	title    string
	started  time.Time
	now      time.Time
	videos   []progress.Video
	logs     []string
	status   string // ffmpeg's latest progress line
	stopping bool
}

// render lays s out in width x height: a title line, then the videos,
// the tasks of each running video, and as much of the log's end as
// fits.
func render(s screen, st styles, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	finished, total := progress.Counts(s.videos)
	title := fmt.Sprintf("%s · %d/%d videos · %s", s.title, finished, total, duration(s.now.Sub(s.started)))
	if s.stopping {
		title += " · stopping…"
	}
	lines := []string{st.title.Render(title)}

	// The log gets whatever the lists leave, but at least a few lines;
	// the lists share the rest, the tasks getting more.
	room := height - 1 - 3 // title, and the three headings
	logMin := min(max(room/3, 4), room)
	listRoom := room - logMin
	videoRoom := max(min(len(s.videos), listRoom/3), min(1, listRoom))
	taskRoom := listRoom - videoRoom

	lines = append(lines, heading(st, "Videos", width))
	lines = append(lines, videoLines(s, st, videoRoom)...)
	lines = append(lines, heading(st, "Tasks", width))
	tl := taskLines(s, st, taskRoom)
	lines = append(lines, tl...)
	lines = append(lines, heading(st, "Log", width))

	logRoom := height - len(lines)
	log := s.logs
	if s.status != "" {
		log = append(slices.Clone(log), st.dim.Render(s.status))
	}
	if len(log) > logRoom {
		log = log[len(log)-logRoom:]
	}
	lines = append(lines, log...)
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, width, "…")
	}
	return strings.Join(lines, "\n")
}

func heading(st styles, name string, width int) string {
	return st.heading.Render("── " + name + " " + strings.Repeat("─", max(0, width-len(name)-4)))
}

// videoLines lists videos in room lines: running ones always, then
// as many finished (most recent) and queued (soonest) as fit, with a
// count of any left out.
func videoLines(s screen, st styles, room int) []string {
	var done, running, queued []progress.Video
	for _, v := range s.videos {
		switch v.Status {
		case progress.Running:
			running = append(running, v)
		case progress.Queued:
			queued = append(queued, v)
		default:
			done = append(done, v)
		}
	}
	line := func(v progress.Video) string {
		switch v.Status {
		case progress.Running:
			return st.running.Render(fmt.Sprintf("▶ %-28s %8s", v.Name, duration(v.Elapsed(s.now))))
		case progress.Failed:
			return st.failed.Render(fmt.Sprintf("✗ %-28s %8s  %v", v.Name, duration(v.Elapsed(s.now)), v.Err))
		case progress.Queued:
			return st.dim.Render("· " + v.Name)
		default:
			return st.done.Render(fmt.Sprintf("✓ %-28s %8s", v.Name, duration(v.Elapsed(s.now))))
		}
	}
	return fit(done, running, queued, room, line,
		func(n int) string { return st.dim.Render(fmt.Sprintf("  … %d more finished", n)) },
		func(n int) string { return st.dim.Render(fmt.Sprintf("  … %d more to come", n)) })
}

// taskLines lists the tasks of each running video in room lines, each
// video's under its name when more than one is running.
func taskLines(s screen, st styles, room int) []string {
	var running []progress.Video
	for _, v := range s.videos {
		if v.Status == progress.Running {
			running = append(running, v)
		}
	}
	if len(running) == 0 || room <= 0 {
		return nil
	}
	var out []string
	share := room / len(running)
	for i, v := range running {
		r := share
		if i == len(running)-1 {
			r = room - len(out)
		}
		if len(running) > 1 {
			out = append(out, st.title.Render(v.Name))
			r--
		}
		out = append(out, videoTasks(v, s.now, st, r)...)
	}
	return out
}

func videoTasks(v progress.Video, now time.Time, st styles, room int) []string {
	var done, running, queued []progress.Task
	for _, t := range v.Tasks {
		switch t.Status {
		case progress.Running:
			running = append(running, t)
		case progress.Queued:
			queued = append(queued, t)
		default:
			done = append(done, t)
		}
	}
	width := 12
	for _, t := range v.Tasks {
		width = max(width, len(t.Name))
	}
	line := func(t progress.Task) string {
		name := fmt.Sprintf("%-*s", width, t.Name)
		switch t.Status {
		case progress.Running:
			return st.running.Render(fmt.Sprintf("▶ %s %8s  %s", name, duration(t.Elapsed(now)), t.Detail))
		case progress.Failed:
			return st.failed.Render(fmt.Sprintf("✗ %s %8s  %v", name, duration(t.Elapsed(now)), t.Err))
		case progress.Skipped:
			return st.dim.Render(fmt.Sprintf("– %s %8s  %s", name, "", t.Detail))
		case progress.Queued:
			return st.dim.Render(fmt.Sprintf("· %s %8s  %s", name, "", t.Detail))
		default:
			return st.done.Render(fmt.Sprintf("✓ %s %8s  %s", name, duration(t.Elapsed(now)), t.Detail))
		}
	}
	return fit(done, running, queued, room, line,
		func(n int) string { return st.dim.Render(fmt.Sprintf("  … %d more finished", n)) },
		func(n int) string { return st.dim.Render(fmt.Sprintf("  … %d more to come", n)) })
}

// fit shows running items in room lines, then fills what's left with
// the most recent of done and the first of queued, splitting the space
// between them and saying how many of each it left out.
func fit[T any](done, running, queued []T, room int, line func(T) string, moreDone, moreQueued func(int) string) []string {
	if room <= 0 {
		return nil
	}
	var out []string
	if len(running) > room {
		for _, r := range running[:room-1] {
			out = append(out, line(r))
		}
		return append(out, moreQueued(len(running)-room+1+len(queued)))
	}
	left := room - len(running)
	showDone, showQueued := len(done), len(queued)
	if showDone+showQueued > left {
		// Give queued items at least half the room, then take lines
		// back, from whichever shows more, until the "more" lines fit.
		showQueued = min(len(queued), max(left/2, left-len(done)))
		showDone = min(len(done), left-showQueued)
		lines := func() int {
			n := showDone + showQueued
			if showDone < len(done) {
				n++
			}
			if showQueued < len(queued) {
				n++
			}
			return n
		}
		for lines() > left && showDone+showQueued > 0 {
			if showDone > 0 && (showDone >= showQueued || showQueued == 0) {
				showDone--
			} else {
				showQueued--
			}
		}
	}
	if showDone < len(done) {
		out = append(out, moreDone(len(done)-showDone))
	}
	for _, d := range done[len(done)-showDone:] {
		out = append(out, line(d))
	}
	for _, r := range running {
		out = append(out, line(r))
	}
	for _, q := range queued[:showQueued] {
		out = append(out, line(q))
	}
	if showQueued < len(queued) {
		out = append(out, moreQueued(len(queued)-showQueued))
	}
	if len(out) > room {
		// Too little room even for the "more" lines: drop the first.
		out = out[len(out)-room:]
	}
	return out
}

// duration formats d to the second: "42s", "4m12s", "1h03m".
func duration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
