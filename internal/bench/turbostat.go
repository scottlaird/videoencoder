package bench

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// CPUStats is what turbostat measured of the whole machine over an
// encode. A field turbostat didn't report (temperatures on some AMD
// chips, say, or LLC counts on older turbostats) is NaN.
type CPUStats struct {
	// BusyPct is the share of time the CPUs were busy, and BusyMHz
	// their average clock while busy.
	BusyPct, BusyMHz float64
	// IPC is instructions per cycle, per logical CPU.
	IPC float64
	// PkgWatts is the processor package's average power, and CoreWatts
	// the cores' part of it.
	PkgWatts, CoreWatts float64
	// PkgTempC is the package temperature at the end, in °C.
	PkgTempC float64
	// LLCHitPct is the last-level cache's hit rate.
	LLCHitPct float64
}

// turbostatColumns are turbostat's names for CPUStats's fields.
var turbostatColumns = []string{"Busy%", "Bzy_MHz", "IPC", "PkgWatt", "CorWatt", "PkgTmp", "LLC%hit"}

func (s *CPUStats) fields() []*float64 {
	return []*float64{&s.BusyPct, &s.BusyMHz, &s.IPC, &s.PkgWatts, &s.CoreWatts, &s.PkgTempC, &s.LLCHitPct}
}

// turbostatArgv runs argv under turbostat, which writes one summary of
// the whole run to out.
func turbostatArgv(turbostat, out string, argv []string) []string {
	return append([]string{turbostat, "--quiet", "--Summary",
		"--show", strings.Join(turbostatColumns, ","), "--out", out, "--"}, argv...)
}

// FindTurbostat returns the turbostat to measure encodes with, or ""
// if there's none or this user can't run it (it needs capabilities;
// see turbostat-setup.sh). It tries it on a command that does
// nothing, with the columns encodes are measured with.
func FindTurbostat(ctx context.Context) string {
	path, err := exec.LookPath("turbostat")
	if err != nil {
		return ""
	}
	out, err := os.CreateTemp("", "turbostat-*.txt")
	if err != nil {
		return ""
	}
	out.Close()
	defer os.Remove(out.Name())
	argv := turbostatArgv(path, out.Name(), []string{"true"})
	msg, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err == nil {
		_, err = readTurbostat(out.Name())
	}
	if err != nil {
		slog.Info("Not measuring encodes with turbostat: it doesn't run without sudo here (see turbostat-setup.sh)",
			"err", err, "output", strings.TrimSpace(string(msg)))
		return ""
	}
	return path
}

func readTurbostat(path string) (*CPUStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseTurbostat(f)
}

// parseTurbostat reads turbostat's --Summary output: a line of
// tab-separated column names, then one of values. Other lines (the
// run's duration, say) are ignored. It's an error if there's no
// summary.
func parseTurbostat(r io.Reader) (*CPUStats, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		header := strings.Fields(sc.Text())
		if !strings.Contains(sc.Text(), "\t") || len(header) == 0 || !sc.Scan() {
			continue
		}
		values := strings.Fields(sc.Text())
		s := &CPUStats{}
		fields := s.fields()
		found := false
		for i, name := range turbostatColumns {
			*fields[i] = math.NaN()
			for j, h := range header {
				if h != name || j >= len(values) {
					continue
				}
				if v, err := strconv.ParseFloat(values[j], 64); err == nil {
					*fields[i], found = v, true
				}
			}
		}
		if found {
			return s, nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errNoTurbostatSummary
}

var errNoTurbostatSummary = errors.New("turbostat wrote no summary")
