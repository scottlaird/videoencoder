// Command encdb builds and serves an in-browser explorer of encbench's
// results: every logged encode, from every machine, in one parquet
// file that DuckDB-WASM queries in the page.
//
//	encdb build [-config encdb.yml] [-o encodes.parquet]
//	encdb serve [-addr 127.0.0.1:8766] [-data encodes.parquet]
//
// build reads the logs and encbench servers the config names (see
// Config) and writes the table. serve serves the pages, with the
// table built into encdb unless -data names another.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"gopkg.in/vansante/go-ffprobe.v2"
)

const usage = `usage: encdb <command>

commands:
  build [-config encdb.yml] [-o encodes.parquet]   build the table from encbench's logs
  serve [-addr 127.0.0.1:8766] [-data file]        serve the explorer
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "encdb:", err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return flag.ErrHelp
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	switch args[0] {
	case "build":
		fs := flag.NewFlagSet("build", flag.ContinueOnError)
		fs.SetOutput(stderr)
		config := fs.String("config", "encdb.yml", "config `file`")
		out := fs.String("o", "encodes.parquet", "output `file`")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return build(ctx, *config, *out, log)
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		fs.SetOutput(stderr)
		addr := fs.String("addr", "127.0.0.1:8766", "listen `address`")
		data := fs.String("data", "", "serve this parquet `file` instead of the built-in snapshot")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return serve(ctx, *addr, *data, log)
	}
	fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

func build(ctx context.Context, configPath, out string, log *slog.Logger) error {
	c, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	rows := gather(ctx, c, log)
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, rt := range c.Retime {
		log.Info("retimed", "host", rt.Host, "log", rt.Log, "answers", retime(rows, rt))
	}
	depths := probeDepths(ctx, c, ffprobePixFmt, log)
	table := derive(c, rows, depths)

	tmp, err := os.CreateTemp(filepath.Dir(out), ".encodes-*.parquet")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := writeParquet(tmp, table); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), out); err != nil {
		return err
	}
	st, err := os.Stat(out)
	if err != nil {
		return err
	}
	log.Info("wrote", "file", out, "rows", len(table), "MB", fmt.Sprintf("%.1f", float64(st.Size())/1e6))
	return nil
}

// ffprobePixFmt is the pixel format of path's first video stream.
func ffprobePixFmt(ctx context.Context, path string) (string, error) {
	data, err := ffprobe.ProbeURL(ctx, path)
	if err != nil {
		return "", err
	}
	if s := data.FirstVideoStream(); s != nil {
		return s.PixFmt, nil
	}
	return "", fmt.Errorf("%s: no video stream", path)
}
