// Package tui shows a run's progress in the terminal: the videos it's
// working through, the tasks of the ones running now, and the end of
// its log. It's built on the events package progress reports.
package tui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"github.com/scottlaird/videoencoder/internal/progress"
)

// Available reports whether f looks like a terminal a UI can draw on:
// it is one, and $TERM names a real terminal type.
func Available(f *os.File) bool {
	t := os.Getenv("TERM")
	return term.IsTerminal(int(f.Fd())) && t != "" && t != "dumb"
}

// Outputs are where work writes while the UI owns the screen.
type Outputs struct {
	// Log is shown in the UI's log pane, and written to the log file.
	// Lines ending in a carriage return, like ffmpeg's -stats, replace
	// the pane's status line instead of adding to the log.
	Log io.Writer
	// Stdout is held, and written to the real stdout once the UI ends,
	// so reports like tune's sidecar summary aren't lost.
	Stdout io.Writer
}

// maxLogLines is how much log the pane keeps; the file keeps it all.
const maxLogLines = 2000

// Run shows title's progress on screen, which must be a terminal,
// while work runs with a context carrying a reporter for the UI. When
// work returns, it restores the screen and prints a summary of the
// videos, then what work wrote to stdout, to stdout. Interrupting with
// Ctrl-C cancels work's context; the UI stays up until work returns.
//
// The full log is written to a file under logDir, whose path the
// summary gives; "" keeps no file.
func Run(ctx context.Context, title string, screen *os.File, stdout io.Writer, logDir string, work func(ctx context.Context, out Outputs) error) error {
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

	tracker := &progress.Tracker{}
	m := &model{
		title:   title,
		started: time.Now(),
		tracker: tracker,
		cancel:  cancel,
		styles:  newStyles(lipgloss.NewRenderer(screen)),
	}
	opts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithOutput(screen), tea.WithoutSignalHandler()}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		// Without a keyboard, Ctrl-C arrives as a signal instead.
		opts = append(opts, tea.WithInput(nil))
	}
	p := tea.NewProgram(m, opts...)

	logw := &lineWriter{
		file:   logFile,
		line:   func(l string) { p.Send(logMsg(l)) },
		status: func(l string) { p.Send(statusMsg(l)) },
	}
	var held bytes.Buffer
	stdoutw := &lockedWriter{w: &held}
	rep := reporter{tracker: tracker, send: p.Send}

	var workErr error
	go func() {
		workErr = work(progress.WithReporter(ctx, rep), Outputs{Log: logw, Stdout: stdoutw})
		logw.Flush()
		p.Send(doneMsg{})
	}()
	if _, err := p.Run(); err != nil {
		cancel()
		return fmt.Errorf("terminal UI: %w", err)
	}

	printSummary(stdout, tracker.Snapshot(), m.started, time.Now())
	stdout.Write(held.Bytes())
	if logFile != nil {
		fmt.Fprintf(stdout, "log: %s\n", logFile.Name())
	}
	return workErr
}

// keepLogs is how many run logs pruneLogs keeps.
const keepLogs = 50

// pruneLogs deletes all but the newest keep logs in dir. Their names
// start with their time, so the newest sort last.
func pruneLogs(dir string, keep int) {
	names, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil || len(names) <= keep {
		return
	}
	slices.Sort(names)
	for _, n := range names[:len(names)-keep] {
		os.Remove(n)
	}
}

// reporter tracks events and redraws the UI for each.
type reporter struct {
	tracker *progress.Tracker
	send    func(tea.Msg)
}

func (r reporter) Report(e progress.Event) {
	r.tracker.Apply(e)
	r.send(refreshMsg{})
}

type (
	refreshMsg struct{}
	tickMsg    struct{}
	doneMsg    struct{}
	logMsg     string
	statusMsg  string
)

type model struct {
	title    string
	started  time.Time
	tracker  *progress.Tracker
	cancel   context.CancelFunc
	styles   styles
	width    int
	height   int
	logs     []string
	status   string
	stopping bool
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *model) Init() tea.Cmd { return tick() }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			m.stopping = true
			m.cancel()
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

func (m *model) View() string {
	return render(screen{
		title:    m.title,
		started:  m.started,
		now:      time.Now(),
		videos:   m.tracker.Snapshot(),
		logs:     m.logs,
		status:   m.status,
		stopping: m.stopping,
	}, m.styles, m.width, m.height)
}

// LogHandler is a slog handler for the UI's log pane, writing to w:
// text lines with the time of day and without INFO's level, so more
// of each line fits the screen.
func LogHandler(w io.Writer) slog.Handler {
	return slog.NewTextHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.TimeKey:
				return slog.String(slog.TimeKey, a.Value.Time().Format("15:04:05"))
			case slog.LevelKey:
				if a.Value.String() == slog.LevelInfo.String() {
					return slog.Attr{}
				}
			}
			return a
		},
	})
}

// printSummary writes each video's outcome and time, once the UI is
// gone.
func printSummary(w io.Writer, videos []progress.Video, started, now time.Time) {
	finished, total := progress.Counts(videos)
	fmt.Fprintf(w, "%d/%d videos in %s\n", finished, total, duration(now.Sub(started)))
	for _, v := range videos {
		switch v.Status {
		case progress.Done:
			fmt.Fprintf(w, "  ✓ %-28s %8s\n", v.Name, duration(v.Elapsed(now)))
		case progress.Failed:
			fmt.Fprintf(w, "  ✗ %-28s %8s  %v\n", v.Name, duration(v.Elapsed(now)), v.Err)
		case progress.Running:
			fmt.Fprintf(w, "  ! %-28s %8s  stopped\n", v.Name, duration(v.Elapsed(now)))
		default:
			fmt.Fprintf(w, "  · %-28s %8s  not started\n", v.Name, "")
		}
	}
}

// lineWriter splits writes into lines for the log pane, and writes
// everything to file. A line ended by a carriage return is a status
// that the next replaces, like ffmpeg's -stats progress.
type lineWriter struct {
	mu     sync.Mutex
	file   io.Writer // may be nil
	buf    []byte
	line   func(string)
	status func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexAny(w.buf, "\r\n")
		if i < 0 {
			break
		}
		text := string(bytes.TrimRight(w.buf[:i], " "))
		if w.buf[i] == '\n' {
			w.line(text)
			if w.file != nil {
				fmt.Fprintln(w.file, text)
			}
		} else if text != "" {
			w.status(text)
		}
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// Flush sends any unterminated last line.
func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.line(string(w.buf))
		if w.file != nil {
			fmt.Fprintln(w.file, string(w.buf))
		}
		w.buf = nil
	}
}

// lockedWriter makes w safe for concurrent writes.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
