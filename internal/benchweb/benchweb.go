// Package benchweb serves a web view of an encbench queue: the same
// state the terminal UI shows, as a page that updates itself, plus a
// small JSON API with pause and resume.
//
//	GET  /             the page
//	GET  /api/state    the queue's state, as JSON (see State)
//	GET  /api/events   server-sent events: the state, on every change
//	GET  /api/results  every row of the queue's results logs, as JSON
//	                   (see Results)
//	POST /api/pause    pause after the current unit
//	POST /api/resume   resume
//
// There is no authentication: anyone who can reach the address can see
// the queue and pause it. Pause and resume are the only actions.
package benchweb

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/scottlaird/videoencoder/internal/benchqueue"
)

//go:embed index.html
var indexHTML []byte

// maxNext is how many of the next units State lists.
const maxNext = 20

// Server is the web view of a Runner.
type Server struct {
	Runner *benchqueue.Runner
	// Title names the queue on the page, such as "encbench queue q".
	Title string
	// Log, if set, is shown under the results.
	Log *LogRing
	// Now is for tests.
	Now func() time.Time

	mu   sync.Mutex
	subs map[chan struct{}]bool
}

// Notify tells the page that the state changed. Set it as the Runner's
// Changed (chained with any other) and the LogRing's Changed. It never
// blocks: a page that hasn't caught up gets one update for several
// changes.
func (s *Server) Notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.subs {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

func (s *Server) subscribe() chan struct{} {
	c := make(chan struct{}, 1)
	s.mu.Lock()
	if s.subs == nil {
		s.subs = map[chan struct{}]bool{}
	}
	s.subs[c] = true
	s.mu.Unlock()
	return c
}

func (s *Server) unsubscribe(c chan struct{}) {
	s.mu.Lock()
	delete(s.subs, c)
	s.mu.Unlock()
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handler serves the page and the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(s.State())
	})
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/results", s.results)
	mux.HandleFunc("POST /api/pause", func(w http.ResponseWriter, r *http.Request) { s.setPaused(w, true) })
	mux.HandleFunc("POST /api/resume", func(w http.ResponseWriter, r *http.Request) { s.setPaused(w, false) })
	return mux
}

func (s *Server) setPaused(w http.ResponseWriter, p bool) {
	s.Runner.SetPaused(p)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.State())
}

// events streams the state as server-sent events: once on connecting,
// then after every change, and every 15 s with nothing new so proxies
// keep the connection open.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	c := s.subscribe()
	defer s.unsubscribe(c)
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	send := func() bool {
		data, err := json.Marshal(s.State())
		if err != nil {
			return false
		}
		if _, err := w.Write(append(append([]byte("data: "), data...), '\n', '\n')); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-c:
			// Changes come in bursts (a unit ending rescans every
			// file); let them settle so the page redraws once.
			time.Sleep(100 * time.Millisecond)
			select {
			case <-c:
			default:
			}
			if !send() {
				return
			}
		case <-keepalive.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// State is the queue as the API returns it. Times are RFC 3339 and
// durations seconds; the page computes running times from Now.
type State struct {
	Title    string    `json:"title"`
	Dir      string    `json:"dir"`
	Now      time.Time `json:"now"`
	Started  time.Time `json:"started"`
	Phase    string    `json:"phase"`
	Waiting  string    `json:"waiting,omitempty"`
	Pausing  bool      `json:"pausing"`
	Stopping bool      `json:"stopping"`
	Err      string    `json:"error,omitempty"`
	Done     int       `json:"done"`
	Total    int       `json:"total"`
	Current  *Current  `json:"current"`
	Files    []File    `json:"files"`
	// Next are the units after the current one, in run order, at most
	// maxNext of NextTotal.
	Next      []Unit   `json:"next"`
	NextTotal int      `json:"next_total"`
	Recent    []Result `json:"recent"`
	Log       []string `json:"log"`
	Status    string   `json:"status,omitempty"`
}

// Unit is a flagset on a sample, from a work file.
type Unit struct {
	File    string `json:"file"`
	Sample  string `json:"sample"`
	Flagset string `json:"flagset"`
}

// Current is the unit running.
type Current struct {
	Unit
	Started time.Time `json:"started"`
	Round   int       `json:"round"`
	Version string    `json:"version,omitempty"` // the sample version (a scaled copy, say) being encoded
	Detail  string    `json:"detail,omitempty"`
	Answers []string  `json:"answers"`
}

// File is a work file's progress.
type File struct {
	Name      string `json:"name"`
	Err       string `json:"error,omitempty"`
	Pending   bool   `json:"pending"`
	Done      int    `json:"done"`
	Failed    int    `json:"failed"`
	Remaining int    `json:"remaining"`
	Total     int    `json:"total"`
	Running   bool   `json:"running"`
}

// Result is a finished unit.
type Result struct {
	Unit
	Elapsed   float64  `json:"elapsed"`
	Answers   []string `json:"answers"`
	Err       string   `json:"error,omitempty"`
	Abandoned bool     `json:"abandoned"`
}

var phases = map[benchqueue.Phase]string{
	benchqueue.Starting: "starting",
	benchqueue.Running:  "running",
	benchqueue.Waiting:  "waiting",
	benchqueue.Paused:   "paused",
	benchqueue.Idle:     "idle",
	benchqueue.Finished: "finished",
}

func unit(u benchqueue.Unit) Unit { return Unit{File: u.File, Sample: u.Sample, Flagset: u.Flagset} }

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// State returns the queue's state now.
func (s *Server) State() State {
	q := s.Runner.Snapshot()
	out := State{
		Title:    s.Title,
		Dir:      q.Dir,
		Now:      s.now(),
		Started:  q.Started,
		Phase:    phases[q.Phase],
		Waiting:  q.Waiting,
		Pausing:  q.Pausing,
		Stopping: q.Stopping,
		Err:      errText(q.Err),
		Files:    []File{},
		Next:     []Unit{},
		Recent:   []Result{},
		Log:      []string{},
	}
	if c := q.Current; c != nil {
		out.Current = &Current{
			Unit:    unit(c.Unit),
			Started: c.Started,
			Round:   c.Round,
			Version: c.Sample,
			Detail:  c.Detail,
			Answers: append([]string{}, c.Answers...),
		}
	}
	var next []benchqueue.Unit
	for _, f := range q.Files {
		out.Done += f.Done
		out.Total += f.Total()
		out.Files = append(out.Files, File{
			Name:      f.Name,
			Err:       errText(f.Err),
			Pending:   f.Pending,
			Done:      f.Done,
			Failed:    f.Failed,
			Remaining: len(f.Remaining),
			Total:     f.Total(),
			Running:   q.Current != nil && q.Current.File == f.Name,
		})
		if f.Err == nil && !f.Pending {
			next = append(next, f.Remaining...)
		}
	}
	if q.Current != nil && len(next) > 0 && next[0] == q.Current.Unit {
		next = next[1:]
	}
	out.NextTotal = len(next)
	for _, u := range next[:min(len(next), maxNext)] {
		out.Next = append(out.Next, unit(u))
	}
	for _, r := range q.Recent {
		out.Recent = append(out.Recent, Result{
			Unit:      unit(r.Unit),
			Elapsed:   r.Elapsed.Seconds(),
			Answers:   append([]string{}, r.Answers...),
			Err:       errText(r.Err),
			Abandoned: errors.Is(r.Err, context.Canceled),
		})
	}
	if s.Log != nil {
		out.Log, out.Status = s.Log.Lines()
	}
	return out
}
