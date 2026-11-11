package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/scottlaird/videoencoder/internal/benchqueue"
	"github.com/scottlaird/videoencoder/internal/benchweb"
	"github.com/scottlaird/videoencoder/internal/tui"
)

// webLogLines is how much of the log the web view keeps.
const webLogLines = 500

// queueCmd runs the queue in a directory: in a terminal UI when it
// can, and otherwise logging to stderr. With -http, it also serves a
// web view of the queue, with pause and resume.
func queueCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("queue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	watch := fs.Bool("watch", false, "keep running when the queue is empty, waiting for new work")
	report := fs.Bool("report", true, "write each work file's report when its last unit finishes")
	poll := fs.Duration("poll", 5*time.Second, "how often to look for changes while waiting")
	ui := fs.String("tui", "auto", `show progress in a terminal UI: "auto" when stderr is a terminal, "on" or "off"`)
	httpAddr := fs.String("http", "", `serve a web view of the queue at this address, such as ":8090" or "localhost:8090"; it has no login`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: encbench queue [-watch] [-report=false] [-poll D] [-tui MODE] [-http ADDR] dir")
		return flag.ErrHelp
	}
	dir := fs.Arg(0)
	if st, err := os.Stat(dir); err != nil {
		return err
	} else if !st.IsDir() {
		return fmt.Errorf("%s: not a directory", dir)
	}
	title := "encbench queue " + dir

	r := &benchqueue.Runner{Dir: dir, Watch: *watch, Reports: *report, Poll: *poll}

	// The web view, if asked for, listens before anything runs, so a
	// bad address fails at once. Logs are copied to it as they're
	// written.
	var ring *benchweb.LogRing
	webURL := ""
	if *httpAddr != "" {
		ln, err := net.Listen("tcp", *httpAddr)
		if err != nil {
			return fmt.Errorf("-http: %w", err)
		}
		ring = benchweb.NewLogRing(webLogLines)
		srv := &benchweb.Server{Runner: r, Title: title, Log: ring}
		ring.Changed = srv.Notify
		r.Changed = srv.Notify // the TUI chains its own after this
		hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go hs.Serve(ln)
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			hs.Shutdown(sctx)
		}()
		webURL = "http://" + webAddr(ln.Addr()) + "/"
	}
	tee := func(w io.Writer) io.Writer {
		if ring == nil {
			return w
		}
		return io.MultiWriter(w, ring)
	}

	runQueue := func(ctx context.Context, log io.Writer) error {
		local, err := benchqueue.NewLocal(ctx, log)
		if err != nil {
			return err
		}
		r.Backend = local
		if webURL != "" {
			slog.Info("Web view", "url", webURL)
		}
		return r.Run(ctx)
	}

	useTUI := *ui == "on" || (*ui == "auto" && tui.Available(os.Stderr))
	if !useTUI {
		// slog's default handler writes through the log package, so
		// this copies its lines to the web view too.
		if ring != nil {
			prev := log.Writer()
			log.SetOutput(tee(prev))
			defer log.SetOutput(prev)
		}
		err := runQueue(ctx, tee(stderr))
		if errors.Is(err, context.Canceled) {
			return errors.New("interrupted")
		}
		return err
	}
	logDir := ""
	if cache, err := os.UserCacheDir(); err == nil {
		logDir = filepath.Join(cache, "encbench", "logs")
	}
	err := tui.RunQueue(ctx, title, os.Stderr, stdout, logDir, r, func(ctx context.Context, out tui.Outputs) error {
		w := tee(out.Log)
		prev := slog.Default()
		slog.SetDefault(slog.New(tui.LogHandler(w)))
		defer slog.SetDefault(prev)
		return runQueue(ctx, w)
	})
	if webURL != "" {
		fmt.Fprintf(stdout, "web view was at %s\n", webURL)
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("interrupted")
	}
	return err
}

// webAddr is addr as a browser would want it: an unspecified host
// (":8090", "0.0.0.0") becomes this machine's name.
func webAddr(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		if name, err := os.Hostname(); err == nil {
			host = name
		}
	}
	return net.JoinHostPort(host, port)
}
