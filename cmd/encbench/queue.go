package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/scottlaird/videoencoder/internal/benchqueue"
	"github.com/scottlaird/videoencoder/internal/tui"
)

// queueCmd runs the queue in a directory: in a terminal UI when it
// can, and otherwise logging to stderr.
func queueCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("queue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	watch := fs.Bool("watch", false, "keep running when the queue is empty, waiting for new work")
	report := fs.Bool("report", true, "write each work file's report when its last unit finishes")
	poll := fs.Duration("poll", 5*time.Second, "how often to look for changes while waiting")
	ui := fs.String("tui", "auto", `show progress in a terminal UI: "auto" when stderr is a terminal, "on" or "off"`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: encbench queue [-watch] [-report=false] [-poll D] [-tui MODE] dir")
		return flag.ErrHelp
	}
	dir := fs.Arg(0)
	if st, err := os.Stat(dir); err != nil {
		return err
	} else if !st.IsDir() {
		return fmt.Errorf("%s: not a directory", dir)
	}

	r := &benchqueue.Runner{Dir: dir, Watch: *watch, Reports: *report, Poll: *poll}
	runQueue := func(ctx context.Context, log io.Writer) error {
		local, err := benchqueue.NewLocal(ctx, log)
		if err != nil {
			return err
		}
		r.Backend = local
		return r.Run(ctx)
	}

	useTUI := *ui == "on" || (*ui == "auto" && tui.Available(os.Stderr))
	if !useTUI {
		err := runQueue(ctx, stderr)
		if errors.Is(err, context.Canceled) {
			return errors.New("interrupted")
		}
		return err
	}
	logDir := ""
	if cache, err := os.UserCacheDir(); err == nil {
		logDir = filepath.Join(cache, "encbench", "logs")
	}
	err := tui.RunQueue(ctx, "encbench queue "+dir, os.Stderr, stdout, logDir, r, func(ctx context.Context, out tui.Outputs) error {
		prev := slog.Default()
		slog.SetDefault(slog.New(tui.LogHandler(out.Log)))
		defer slog.SetDefault(prev)
		return runQueue(ctx, out.Log)
	})
	if errors.Is(err, context.Canceled) {
		return errors.New("interrupted")
	}
	return err
}
