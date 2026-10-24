// Command encbench finds, for each candidate set of encoder flags, the
// quality setting that just reaches a target VMAF on each sample clip,
// and records size, speed and quality for comparison.
//
//	encbench [-config bench.yaml] run [-n] [-sample glob] [-flagset glob]
//	encbench [-config bench.yaml] report
//	encbench [-config bench.yaml] prune [-n]
//
// Every encode is logged in results.csv and never repeated, so adding a
// flagset or a sample only runs the new combinations.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/scottlaird/videoencoder/internal/bench"
	"github.com/scottlaird/videoencoder/internal/probe"
)

const usage = `usage: encbench [-config bench.yaml] <command>

commands:
  run [-n] [-sample glob] [-flagset glob]   search for unfinished pairs (-n: show what would be encoded)
  report                                    print the answers as Markdown tables
  prune [-n]                                delete kept encodes of removed samples and flagsets
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "encbench:", err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	global := flag.NewFlagSet("encbench", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { fmt.Fprint(stderr, usage) }
	configPath := global.String("config", "bench.yaml", "bench config file")
	if err := global.Parse(args); err != nil {
		return err
	}
	if global.NArg() == 0 {
		global.Usage()
		return flag.ErrHelp
	}
	cfg, err := bench.LoadConfig(*configPath)
	if err != nil {
		return err
	}

	cmd, rest := global.Arg(0), global.Args()[1:]
	switch cmd {
	case "run":
		return runCmd(ctx, cfg, rest, stderr)
	case "report":
		results, err := bench.LoadResults(filepath.Join(cfg.Workdir, "results.csv"))
		if err != nil {
			return err
		}
		bench.Report(stdout, cfg, results)
		return nil
	case "prune":
		fs := flag.NewFlagSet("prune", flag.ContinueOnError)
		fs.SetOutput(stderr)
		dryRun := fs.Bool("n", false, "only list what would be deleted")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return prune(cfg, *dryRun, stdout)
	}
	global.Usage()
	return fmt.Errorf("unknown command %q", cmd)
}

func runCmd(ctx context.Context, cfg *bench.Config, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("n", false, "show what would be encoded, without encoding")
	sample := fs.String("sample", "", "only samples whose file name matches this glob")
	flagset := fs.String("flagset", "", "only flagsets whose name matches this glob")
	if err := fs.Parse(args); err != nil {
		return err
	}

	version, err := ffmpegVersion(ctx)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	b := &bench.Bench{
		Cfg:           cfg,
		Exec:          bench.ExecExecutor{Stderr: stderr},
		Prober:        probe.FFProbe{},
		Host:          host,
		FFmpegVersion: version,
		SampleFilter:  *sample,
		FlagsetFilter: *flagset,
		DryRun:        *dryRun,
	}
	return b.Run(ctx)
}

// ffmpegVersion returns the version from the first line of
// "ffmpeg -version", such as "8.1".
func ffmpegVersion(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-version").Output()
	if err != nil {
		return "", fmt.Errorf("running ffmpeg -version: %w", err)
	}
	line, _, _ := bytes.Cut(out, []byte("\n"))
	if f := strings.Fields(string(line)); len(f) >= 3 && f[0] == "ffmpeg" && f[1] == "version" {
		return f[2], nil
	}
	return strings.TrimSpace(string(line)), nil
}

// prune deletes kept encodes that are no longer an answer for a
// current sample and flagset.
func prune(cfg *bench.Config, dryRun bool, stdout io.Writer) error {
	results, err := bench.LoadResults(filepath.Join(cfg.Workdir, "results.csv"))
	if err != nil {
		return err
	}
	keepDir := filepath.Join(cfg.Workdir, "keep")
	wanted := map[string]bool{}
	for _, b := range bench.Bests(cfg, results) {
		wanted[bench.KeeperPath(keepDir, b.Sample, b.Name, b.Quality)] = true
	}
	kept, err := filepath.Glob(filepath.Join(keepDir, "*", "*.mp4"))
	if err != nil {
		return err
	}
	w := bufio.NewWriter(stdout)
	defer w.Flush()
	for _, k := range kept {
		if wanted[k] {
			continue
		}
		fmt.Fprintln(w, k)
		if !dryRun {
			if err := os.Remove(k); err != nil {
				return err
			}
		}
	}
	return nil
}
