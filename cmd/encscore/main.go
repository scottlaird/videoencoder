// Command encscore scores encodes that encbench queues serve over HTTP
// with metrics encbench doesn't run inline (cvvdp, so far), apart from
// the encoding, and logs the scores where encdb can join them to
// encbench's rows.
//
//	encscore [-dir DIR] [-cache-gb 200] [-watch] [-poll 1m] [-http ADDR] JOB.yaml|JOBDIR...
//
// Each job names queues (encbench queue -http) and which of their
// encodes to score, and how. A directory stands for every .yaml file
// in it, read again on every pass, so jobs can be added while it runs.
// Scores go to DIR/scores.csv, one line per encode and scorer; encodes
// already scored (by the same scorer, sample and output) are skipped,
// whichever queue or row they came from. Fetched files are cached in
// DIR/cache.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/scottlaird/videoencoder/internal/score"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "encscore:", err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("encscore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "encscore", "where scores.csv and the file cache go")
	cacheGB := fs.Float64("cache-gb", 200, "keep the file cache under this many GB (decimal); 0 for no limit")
	watch := fs.Bool("watch", false, "keep running, looking for new encodes to score")
	poll := fs.Duration("poll", time.Minute, "with -watch, how often to look")
	batch := fs.Int("batch", 8, "the most encodes scored in one run of cvvdp")
	cvvdp := fs.String("cvvdp", "cvvdp", "the cvvdp command")
	version := fs.String("cvvdp-version", "", "cvvdp's version, if it can't be read from its Python package")
	httpAddr := fs.String("http", "", `serve the state and scores as JSON at this address, such as ":8091"; it has no login`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: encscore [-dir DIR] [-cache-gb N] [-watch] [-poll D] [-batch N] [-http ADDR] JOB.yaml|JOBDIR...")
		return flag.ErrHelp
	}
	paths := fs.Args()
	jobs := func() ([]*score.Job, error) { return loadJobs(paths) }
	if _, err := jobs(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(*dir, "tmp"), 0o755); err != nil {
		return err
	}
	ex := score.Command{}
	v := *version
	if v == "" {
		var err error
		if v, err = score.CVVDPVersion(ctx, ex, *cvvdp); err != nil {
			return err
		}
	}
	log, err := score.OpenLog(filepath.Join(*dir, "scores.csv"))
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")
	r := &score.Runner{
		Jobs:  jobs,
		Log:   log,
		Cache: &score.Cache{Dir: filepath.Join(*dir, "cache"), MaxBytes: int64(*cacheGB * 1e9)},
		CVVDP: &score.CVVDP{Bin: *cvvdp, Exec: ex, Version: v},
		Host:  host,
		Tmp:   filepath.Join(*dir, "tmp"),
		Batch: *batch,
		Watch: *watch,
		Poll:  *poll,
	}
	if *httpAddr != "" {
		ln, err := net.Listen("tcp", *httpAddr)
		if err != nil {
			return fmt.Errorf("-http: %w", err)
		}
		hs := &http.Server{Handler: score.Handler(r), ReadHeaderTimeout: 10 * time.Second}
		go hs.Serve(ln)
		defer hs.Close()
	}
	slog.Info("Scoring", "cvvdp", v, "jobs", paths, "log", filepath.Join(*dir, "scores.csv"), "scored", len(log.Rows()))
	err = r.Run(ctx)
	st := r.Snapshot()
	slog.Info("Stopped", "scored", st.Scored, "failed", st.Failed)
	return err
}

// loadJobs reads the work files paths name, a directory standing for
// the .yaml files in it.
func loadJobs(paths []string) ([]*score.Job, error) {
	var files []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			files = append(files, p)
			continue
		}
		m, err := filepath.Glob(filepath.Join(p, "*.yaml"))
		if err != nil {
			return nil, err
		}
		slices.Sort(m)
		files = append(files, m...)
	}
	var jobs []*score.Job
	for _, f := range files {
		j, err := score.LoadJob(f)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}
