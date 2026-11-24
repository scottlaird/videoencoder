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
	"slices"
	"strconv"
	"strings"
	"time"
)

// CPUStats is what turbostat measured of the whole machine over an
// encode: its samples' averages (see turbostatInterval). A field
// turbostat didn't report (temperatures on some AMD chips, say, or LLC
// counts on older turbostats) is NaN.
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

// turbostatTime is turbostat's column for when each sample was taken.
const turbostatTime = "Time_Of_Day_Seconds"

// turbostatInterval is how often turbostat samples the machine during an
// encode. Measuring a whole encode with one reading at each end isn't
// enough: Linux keeps AMD's package energy count from a 32-bit register,
// read every 164 seconds or when asked, and takes more than half the
// register's range between readings (32.8 kJ, under two minutes at
// 280 W) for a wrap. Sampling often keeps each reading's share small.
const turbostatInterval = 2 * time.Second

func (s *CPUStats) fields() []*float64 {
	return []*float64{&s.BusyPct, &s.BusyMHz, &s.IPC, &s.PkgWatts, &s.CoreWatts, &s.PkgTempC, &s.LLCHitPct}
}

// turbostatArgv runs turbostat sampling the whole machine every
// interval, writing a line of each sample's summary to out, until it's
// interrupted, when it writes the last, partial interval's.
func turbostatArgv(turbostat, out string, interval time.Duration) []string {
	cols := append(slices.Clone(turbostatColumns), turbostatTime)
	return []string{turbostat, "--quiet", "--Summary", "--show", strings.Join(cols, ","),
		"--interval", strconv.FormatFloat(interval.Seconds(), 'f', -1, 64), "--out", out}
}

// FindTurbostat returns the turbostat to measure encodes with, or ""
// if there's none or this user can't run it (it needs capabilities;
// see turbostat-setup.sh). It tries one short sample, with the columns
// encodes are measured with.
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
	argv := append(turbostatArgv(path, out.Name(), 100*time.Millisecond), "--num_iterations", "1")
	msg, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err == nil {
		_, err = readTurbostat(out.Name(), 0.1)
	}
	if err != nil {
		slog.Info("Not measuring encodes with turbostat: it doesn't run without sudo here (see turbostat-setup.sh)",
			"err", err, "output", strings.TrimSpace(string(msg)))
		return ""
	}
	return path
}

// turbostatRun is turbostat sampling the machine while a command runs.
type turbostatRun struct {
	cmd *exec.Cmd
	out string
}

// startTurbostat starts turbostat sampling, until stop.
func startTurbostat(ctx context.Context, turbostat string) (*turbostatRun, error) {
	f, err := os.CreateTemp("", "turbostat-*.txt")
	if err != nil {
		return nil, err
	}
	f.Close()
	argv := turbostatArgv(turbostat, f.Name(), turbostatInterval)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		os.Remove(f.Name())
		return nil, err
	}
	return &turbostatRun{cmd: cmd, out: f.Name()}, nil
}

// stop interrupts turbostat, which writes its last sample and exits,
// and returns the samples' averages.
func (t *turbostatRun) stop() (*CPUStats, error) {
	defer os.Remove(t.out)
	if err := t.cmd.Process.Signal(os.Interrupt); err != nil {
		t.cmd.Process.Kill()
	}
	t.cmd.Wait()
	return readTurbostat(t.out, turbostatInterval.Seconds())
}

func readTurbostat(path string, interval float64) (*CPUStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseTurbostat(f, interval)
}

// turbostatSample is one line of turbostat's --Summary output.
type turbostatSample struct {
	secs   float64            // how long it covers
	values map[string]float64 // by column; a column it lacks is absent
}

// parseTurbostat reads turbostat's --Summary output, a line of
// tab-separated column names and then a line for each sample, and
// averages the samples. Other lines (the run's duration, say) are
// ignored. A sample covers the time since the one before (by
// turbostatTime, if it's there), and the first covers interval seconds.
// It's an error if there are no samples.
func parseTurbostat(r io.Reader, interval float64) (*CPUStats, error) {
	var header []string
	var samples []turbostatSample
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "\t") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if _, err := strconv.ParseFloat(fields[0], 64); err != nil {
			header = fields
			continue
		}
		if header == nil {
			continue
		}
		smp := turbostatSample{secs: interval, values: map[string]float64{}}
		for j, h := range header {
			if j < len(fields) {
				if v, err := strconv.ParseFloat(fields[j], 64); err == nil {
					smp.values[h] = v
				}
			}
		}
		if t, ok := smp.values[turbostatTime]; ok && len(samples) > 0 {
			if prev, ok := samples[len(samples)-1].values[turbostatTime]; ok && t > prev {
				smp.secs = t - prev
			}
		}
		samples = append(samples, smp)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(samples) == 0 {
		return nil, errNoTurbostatSummary
	}
	return averageTurbostat(samples)
}

// averageTurbostat is the stats over samples: each averaged over the
// time that it's a rate of, so busy clock over busy time and IPC over
// busy cycles, and the temperature the last.
func averageTurbostat(samples []turbostatSample) (*CPUStats, error) {
	weight := func(s turbostatSample, by ...string) float64 {
		w := s.secs
		for _, col := range by {
			if v, ok := s.values[col]; ok {
				w *= v
			}
		}
		return w
	}
	avg := func(col string, by ...string) float64 {
		var sum, total float64
		for _, s := range samples {
			if v, ok := s.values[col]; ok {
				w := weight(s, by...)
				sum += v * w
				total += w
			}
		}
		if total == 0 {
			return math.NaN()
		}
		return sum / total
	}
	last := func(col string) float64 {
		for i := len(samples) - 1; i >= 0; i-- {
			if v, ok := samples[i].values[col]; ok {
				return v
			}
		}
		return math.NaN()
	}
	s := &CPUStats{
		BusyPct:   avg("Busy%"),
		BusyMHz:   avg("Bzy_MHz", "Busy%"),
		IPC:       avg("IPC", "Busy%", "Bzy_MHz"),
		PkgWatts:  avg("PkgWatt"),
		CoreWatts: avg("CorWatt"),
		PkgTempC:  last("PkgTmp"),
		LLCHitPct: avg("LLC%hit", "Busy%"),
	}
	for _, f := range s.fields() {
		if !math.IsNaN(*f) {
			return s, nil
		}
	}
	return nil, errNoTurbostatSummary
}

var errNoTurbostatSummary = errors.New("turbostat wrote no summary")
