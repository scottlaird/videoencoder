package benchweb

import (
	"bytes"
	"sync"
)

// LogRing keeps the last lines written to it, for the page's log. It
// splits writes into lines as the terminal UI does: a line ends at a
// newline, and text ending at a carriage return is a status line that
// the next one replaces, so it's shown but never kept.
type LogRing struct {
	// Changed, if set, is called after a write adds or replaces a
	// line. It must not block.
	Changed func()

	mu     sync.Mutex
	max    int
	lines  []string
	status string
	buf    []byte
}

// NewLogRing returns a LogRing that keeps the last max lines.
func NewLogRing(max int) *LogRing { return &LogRing{max: max} }

func (l *LogRing) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.buf = append(l.buf, p...)
	changed := false
	for {
		i := bytes.IndexAny(l.buf, "\r\n")
		if i < 0 {
			break
		}
		text := string(bytes.TrimRight(l.buf[:i], " "))
		if l.buf[i] == '\n' {
			l.lines = append(l.lines, text)
			if len(l.lines) > l.max {
				l.lines = l.lines[len(l.lines)-l.max:]
			}
			l.status = ""
			changed = true
		} else if text != "" {
			l.status = text
			changed = true
		}
		l.buf = l.buf[i+1:]
	}
	l.mu.Unlock()
	if changed && l.Changed != nil {
		l.Changed()
	}
	return len(p), nil
}

// Lines returns the kept lines, oldest first, and the current status
// line, if any.
func (l *LogRing) Lines() (lines []string, status string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...), l.status
}
