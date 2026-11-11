package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"golang.org/x/term"

	"github.com/scottlaird/videoencoder/internal/benchqueue"
)

// RunQueue shows r's progress on screen, which must be a terminal,
// while work runs with a context the UI can cancel; work should run r
// (having pointed its logs at the Outputs) and return what it returns.
// When work returns, it restores the screen and prints the units that
// finished, then what work wrote to stdout, to stdout.
//
// Keys: p pauses after the current unit or resumes; q quits after the
// current unit (r.Stop); Ctrl-C abandons the current unit and quits
// (cancelling work's context), as does an interrupt without a
// keyboard. The full log is written to a file under logDir, as Run's
// is; "" keeps no file.
func RunQueue(ctx context.Context, title string, screen *os.File, stdout io.Writer, logDir string, r *benchqueue.Runner, work func(ctx context.Context, out Outputs) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var logFile *os.File
	if logDir != "" {
		if err := os.MkdirAll(logDir, 0o755); err == nil {
			name := filepath.Join(logDir, time.Now().Format("20060102-150405")+".log")
			logFile, _ = os.Create(name)
		}
	}
	if logFile != nil {
		defer logFile.Close()
		pruneLogs(logDir, keepLogs)
	}

	m := &queueModel{
		title:  title,
		runner: r,
		cancel: cancel,
		styles: newStyles(lipgloss.NewRenderer(screen)),
	}
	opts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithOutput(screen), tea.WithoutSignalHandler()}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		opts = append(opts, tea.WithInput(nil))
	}
	p := tea.NewProgram(m, opts...)
	// Changed may be called from Update (by SetPaused, say), where
	// p.Send would deadlock, so it only flags a redraw, which a
	// goroutine sends; flags while one is pending merge. A Changed
	// already set (a web view, say) still hears every change.
	redraw := make(chan struct{}, 1)
	prev := r.Changed
	r.Changed = func() {
		if prev != nil {
			prev()
		}
		select {
		case redraw <- struct{}{}:
		default:
		}
	}
	go func() {
		for {
			select {
			case <-redraw:
				p.Send(refreshMsg{})
			case <-ctx.Done():
				return
			}
		}
	}()

	logw := &lineWriter{
		file:   logFile,
		line:   func(l string) { p.Send(logMsg(l)) },
		status: func(l string) { p.Send(statusMsg(l)) },
	}
	var held bytes.Buffer
	stdoutw := &lockedWriter{w: &held}

	var workErr error
	go func() {
		workErr = work(ctx, Outputs{Log: logw, Stdout: stdoutw})
		logw.Flush()
		p.Send(doneMsg{})
	}()
	if _, err := p.Run(); err != nil {
		cancel()
		return fmt.Errorf("terminal UI: %w", err)
	}

	s := r.Snapshot()
	printQueueSummary(stdout, s, time.Now())
	stdout.Write(held.Bytes())
	if logFile != nil {
		fmt.Fprintf(stdout, "log: %s\n", logFile.Name())
	}
	return workErr
}

type queueModel struct {
	title      string
	runner     *benchqueue.Runner
	cancel     context.CancelFunc
	styles     styles
	width      int
	height     int
	logs       []string
	status     string
	abandoning bool
}

func (m *queueModel) Init() tea.Cmd { return tick() }

func (m *queueModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			m.abandoning = true
			m.cancel()
		case "q":
			m.runner.Stop()
		case "p", " ":
			m.runner.SetPaused(!m.runner.Snapshot().Pausing)
		}
	case tickMsg:
		return m, tick()
	case logMsg:
		m.logs = append(m.logs, string(msg))
		if len(m.logs) > maxLogLines {
			m.logs = m.logs[len(m.logs)-maxLogLines:]
		}
		m.status = ""
	case statusMsg:
		m.status = string(msg)
	case doneMsg:
		return m, tea.Quit
	}
	return m, nil
}

func (m *queueModel) View() string {
	return renderQueue(queueScreen{
		title:      m.title,
		now:        time.Now(),
		state:      m.runner.Snapshot(),
		logs:       m.logs,
		status:     m.status,
		abandoning: m.abandoning,
	}, m.styles, m.width, m.height)
}

// queueScreen is everything one frame of the queue UI shows.
type queueScreen struct {
	title      string
	now        time.Time
	state      benchqueue.State
	logs       []string
	status     string
	abandoning bool
}

// renderQueue lays s out in width x height: a title line, the unit
// running now, the work files and their next units, recent results,
// as much of the log's end as fits, and a line of keys.
func renderQueue(s queueScreen, st styles, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	st0 := s.state
	done, total := 0, 0
	for _, f := range st0.Files {
		done += f.Done
		total += f.Total()
	}
	title := fmt.Sprintf("%s · %d/%d units · %s · %s", s.title, done, total, phaseText(s), duration(s.now.Sub(st0.Started)))
	lines := []string{st.title.Render(title)}

	room := height - 1 - 4 - 1 // title, four headings, keys
	now := nowLines(s, st)
	now = now[:min(len(now), max(room/4, 2), max(room, 0))]
	room -= len(now)
	logMin := min(max(room/4, 2), max(room, 0))
	listRoom := room - logMin
	results := resultLines(s.state.Recent, st)
	resultRoom := min(len(results), listRoom/3)
	queueRoom := listRoom - resultRoom

	lines = append(lines, heading(st, "Now", width))
	lines = append(lines, now...)
	lines = append(lines, heading(st, "Queue", width))
	lines = append(lines, queueLines(s, st, queueRoom)...)
	lines = append(lines, heading(st, "Results", width))
	if resultRoom > 0 {
		lines = append(lines, results[len(results)-resultRoom:]...)
	}
	lines = append(lines, heading(st, "Log", width))

	logRoom := height - len(lines) - 1
	log := s.logs
	if s.status != "" {
		log = append(append([]string(nil), log...), st.dim.Render(s.status))
	}
	if logRoom <= 0 {
		log = nil
	} else if len(log) > logRoom {
		log = log[len(log)-logRoom:]
	}
	lines = append(lines, log...)
	for len(lines) < height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, st.dim.Render(keysText(s)))
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, width, "…")
	}
	return strings.Join(lines, "\n")
}

func phaseText(s queueScreen) string {
	st := s.state
	var t string
	switch st.Phase {
	case benchqueue.Starting:
		t = "starting"
	case benchqueue.Running:
		t = "running"
	case benchqueue.Waiting:
		t = "waiting: " + st.Waiting
	case benchqueue.Paused:
		t = "paused"
	case benchqueue.Idle:
		t = "watching for work"
	case benchqueue.Finished:
		t = "finished"
	}
	switch {
	case st.Phase == benchqueue.Finished:
	case s.abandoning:
		t += " · abandoning…"
	case st.Stopping:
		t += " · quitting after this unit"
	case st.Pausing && st.Phase == benchqueue.Running:
		t += " · pausing after this unit"
	}
	return t
}

func keysText(s queueScreen) string {
	pause := "p pause"
	if s.state.Pausing {
		pause = "p resume"
	}
	return pause + " · q quit after this unit · ctrl-c abandon it and quit"
}

// nowLines describe the unit running, or why none is.
func nowLines(s queueScreen, st styles) []string {
	c := s.state.Current
	if c == nil {
		var why string
		switch s.state.Phase {
		case benchqueue.Waiting:
			why = "waiting: " + s.state.Waiting
		case benchqueue.Paused:
			why = "paused; p resumes"
		case benchqueue.Idle:
			why = "nothing to run; add or edit a file in " + s.state.Dir
		default:
			why = "nothing running"
		}
		return []string{st.dim.Render(why)}
	}
	out := []string{st.running.Render(fmt.Sprintf("▶ %s %8s  %s", c.Unit, duration(s.now.Sub(c.Started)), c.File))}
	detail := c.Detail
	if c.Round > 0 {
		detail = fmt.Sprintf("round %d · %s · %s", c.Round, c.Sample, c.Detail)
	}
	if detail != "" {
		out = append(out, "  "+detail)
	}
	for _, a := range c.Answers {
		out = append(out, st.done.Render("  ✓ "+a))
	}
	return out
}

// queueLines list the work files, with their counts and errors, then
// the next units to run. Files with errors and the file running now
// always show; finished files give way first.
func queueLines(s queueScreen, st styles, room int) []string {
	if room <= 0 {
		return nil
	}
	files := s.state.Files
	if len(files) == 0 {
		if s.state.Err != nil {
			return []string{st.failed.Render(fmt.Sprintf("✗ %v", s.state.Err))}
		}
		return []string{st.dim.Render("no work files in " + s.state.Dir)}
	}
	width := 12
	for _, f := range files {
		width = max(width, len(f.Name))
	}
	current := ""
	if c := s.state.Current; c != nil {
		current = c.File
	}
	line := func(f benchqueue.FileState) string {
		name := fmt.Sprintf("%-*s", width, f.Name)
		counts := fmt.Sprintf("%4d/%-4d", f.Done, f.Total())
		if f.Failed > 0 {
			counts += fmt.Sprintf(" %d failed", f.Failed)
		}
		switch {
		case f.Err != nil:
			return st.failed.Render(fmt.Sprintf("✗ %s  %v", name, f.Err))
		case f.Pending:
			return st.dim.Render(fmt.Sprintf("… %s  counted after this unit", name))
		case f.Name == current:
			return st.running.Render(fmt.Sprintf("▶ %s %s", name, counts))
		case len(f.Remaining) == 0:
			return st.done.Render(fmt.Sprintf("✓ %s %s", name, counts))
		default:
			return fmt.Sprintf("· %s %s", name, counts)
		}
	}
	var done, shown, queued []benchqueue.FileState
	for _, f := range files {
		switch {
		case f.Err != nil || f.Name == current:
			shown = append(shown, f)
		case !f.Pending && len(f.Remaining) == 0:
			done = append(done, f)
		default:
			queued = append(queued, f)
		}
	}
	// Keep the file order: fit picks which to show, then they're put
	// back in place.
	picked := map[string]bool{}
	for _, l := range fit(done, shown, queued, room, func(f benchqueue.FileState) string { return f.Name }, func(int) string { return "" }, func(int) string { return "" }) {
		picked[l] = true
	}
	var out []string
	hidden := 0
	for _, f := range files {
		if picked[f.Name] {
			out = append(out, line(f))
		} else {
			hidden++
		}
	}
	if hidden > 0 && len(out) < room {
		out = append(out, st.dim.Render(fmt.Sprintf("  … %d more files", hidden)))
	}

	// The next units, in run order, after the one running.
	var next []benchqueue.Unit
	for _, f := range files {
		if f.Err == nil && !f.Pending {
			next = append(next, f.Remaining...)
		}
	}
	if c := s.state.Current; c != nil && len(next) > 0 && next[0] == c.Unit {
		next = next[1:]
	}
	left := room - len(out)
	if left > 1 && len(next) > 0 {
		out = append(out, st.dim.Render("next:"))
		left--
		n := min(len(next), left)
		if n < len(next) && n > 0 {
			n--
		}
		for _, u := range next[:n] {
			out = append(out, st.dim.Render(fmt.Sprintf("  · %s  %s", u, u.File)))
		}
		if n < len(next) {
			out = append(out, st.dim.Render(fmt.Sprintf("  … %d more units", len(next)-n)))
		}
	}
	return out
}

// resultLines describe finished units, oldest first.
func resultLines(results []benchqueue.Result, st styles) []string {
	var out []string
	for _, r := range results {
		out = append(out, resultLine(r, st))
	}
	return out
}

func resultLine(r benchqueue.Result, st styles) string {
	head := fmt.Sprintf("%s %8s", r.Unit, duration(r.Elapsed))
	answers := strings.Join(r.Answers, "; ")
	switch {
	case errors.Is(r.Err, context.Canceled):
		return st.failed.Render("! " + head + "  abandoned")
	case r.Err != nil:
		return st.failed.Render(fmt.Sprintf("✗ %s  %v", head, r.Err))
	default:
		return st.done.Render("✓ " + head + "  " + answers)
	}
}

// printQueueSummary writes the units that finished, once the UI is
// gone.
func printQueueSummary(w io.Writer, s benchqueue.State, now time.Time) {
	fmt.Fprintf(w, "%d units in %s\n", len(s.Recent), duration(now.Sub(s.Started)))
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.Ascii)
	plain := newStyles(r)
	for _, res := range s.Recent {
		fmt.Fprintf(w, "  %s  (%s)\n", resultLine(res, plain), res.File)
	}
	for _, f := range s.Files {
		if f.Err != nil {
			fmt.Fprintf(w, "  ✗ %s: %v\n", f.Name, f.Err)
		}
	}
}
