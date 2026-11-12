package benchqueue

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/scottlaird/videoencoder/internal/bench"
	"github.com/scottlaird/videoencoder/internal/probe"
)

// Local is the Backend for this machine: it runs searches in this
// process, and waits while DaVinci Resolve renders or another
// videoencoder or encbench runs.
type Local struct {
	// Host, FFmpegVersion and CPU are recorded with each result.
	Host, FFmpegVersion, CPU string
	// Stderr gets the encoders' error output.
	Stderr io.Writer

	prober   cachingProber
	mu       sync.Mutex
	encoders map[string]string
}

// NewLocal returns a Local for this host and the ffmpeg on PATH.
func NewLocal(ctx context.Context, stderr io.Writer) (*Local, error) {
	version, err := bench.FFmpegVersion(ctx)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	return &Local{
		Host:          host,
		FFmpegVersion: version,
		CPU:           bench.CPUModel(ctx),
		Stderr:        stderr,
		prober:        cachingProber{p: probe.FFProbe{}, cache: map[string]probe.VideoData{}},
		encoders:      map[string]string{},
	}, nil
}

func (l *Local) bench(cfg *bench.Config) *bench.Bench {
	return &bench.Bench{
		Cfg:           cfg,
		Exec:          bench.ExecExecutor{Stderr: l.Stderr},
		Prober:        &l.prober,
		OutputProber:  bench.FFProbeOutput{},
		Host:          l.Host,
		FFmpegVersion: l.FFmpegVersion,
		CPU:           l.CPU,
	}
}

func (l *Local) Status(ctx context.Context, cfg *bench.Config) ([]bench.PairStatus, error) {
	return l.bench(cfg).Status(ctx)
}

func (l *Local) Run(ctx context.Context, cfg *bench.Config, sample, flagset string, observe func(bench.Event)) error {
	b := l.bench(cfg)
	b.SampleFilter, b.FlagsetFilter = globEscape(sample), globEscape(flagset)
	b.Observe = observe
	if fs := cfg.Flagsets[flagset]; fs != nil && fs.Encoder() != "" {
		e := fs.Encoder()
		l.mu.Lock()
		v, ok := l.encoders[e]
		l.mu.Unlock()
		if !ok {
			v = bench.EncoderVersion(ctx, e)
			l.mu.Lock()
			l.encoders[e] = v
			l.mu.Unlock()
		}
		b.EncoderVersions = map[string]string{e: v}
	}
	return b.Run(ctx)
}

func (l *Local) Report(ctx context.Context, cfg *bench.Config, path string) error {
	results, err := bench.LoadResults(filepath.Join(cfg.Workdir, "results.csv"))
	if err != nil {
		return err
	}
	sc, err := l.bench(cfg).Scorings(ctx)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	bench.Report(&buf, cfg, results, sc)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Busy waits, as the bench's shell scripts did, while Resolve has a
// .mov open for writing (a render) or a videoencoder runs, and also
// while another encbench does, since two at once spoil both timings.
func (l *Local) Busy(ctx context.Context) string {
	out, _ := exec.CommandContext(ctx, "lsof", "-c", "Resolve").Output() // exits 1 when Resolve isn't running
	if resolveRendering(string(out)) {
		return "Resolve is rendering"
	}
	if out, err := exec.CommandContext(ctx, "pgrep", "-x", "videoencoder").Output(); err == nil && len(bytes.TrimSpace(out)) > 0 {
		return "videoencoder is running"
	}
	out, _ = exec.CommandContext(ctx, "pgrep", "encbench").Output()
	if pid := otherPID(string(out), os.Getpid()); pid != 0 {
		return fmt.Sprintf("another encbench (pid %d) is running", pid)
	}
	return ""
}

// resolveRendering reports whether lsof output lists a .mov open for
// writing: an FD column ending in w or u.
func resolveRendering(lsof string) bool {
	for _, line := range strings.Split(lsof, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.Contains(line, ".mov") {
			continue
		}
		if fd := f[3]; strings.HasSuffix(fd, "w") || strings.HasSuffix(fd, "u") {
			return true
		}
	}
	return false
}

// otherPID returns the first of pgrep's PIDs that isn't self, or 0.
func otherPID(pgrep string, self int) int {
	for _, f := range strings.Fields(pgrep) {
		if pid, err := strconv.Atoi(f); err == nil && pid != self {
			return pid
		}
	}
	return 0
}

// globEscape quotes path.Match's special characters in s.
func globEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\*?[`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// cachingProber remembers probes of files whose size and time haven't
// changed, since Status probes every sample on every rescan.
type cachingProber struct {
	p     probe.Prober
	mu    sync.Mutex
	cache map[string]probe.VideoData
}

func (c *cachingProber) Probe(ctx context.Context, path string) (probe.VideoData, error) {
	st, err := os.Stat(path)
	if err != nil {
		return probe.VideoData{}, err
	}
	key := fmt.Sprintf("%s\x00%d\x00%d", path, st.Size(), st.ModTime().UnixNano())
	c.mu.Lock()
	vd, ok := c.cache[key]
	c.mu.Unlock()
	if ok {
		return vd, nil
	}
	vd, err = c.p.Probe(ctx, path)
	if err != nil {
		return vd, err
	}
	c.mu.Lock()
	c.cache[key] = vd
	c.mu.Unlock()
	return vd, nil
}
