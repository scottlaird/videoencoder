package score

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Executor runs a command, returning its standard output.
type Executor interface {
	Run(ctx context.Context, argv []string) ([]byte, error)
}

// Command is the Executor that runs real commands.
type Command struct{}

func (Command) Run(ctx context.Context, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
			msg = msg[i+1:]
		}
		return out, fmt.Errorf("%s: %w: %s", filepath.Base(argv[0]), err, msg)
	}
	// cvvdp can log an error, give up on a test and still exit 0.
	for line := range strings.Lines(stderr.String()) {
		if msg, ok := strings.CutPrefix(strings.TrimSpace(line), "[ERROR] "); ok {
			return out, fmt.Errorf("%s: %s", filepath.Base(argv[0]), msg)
		}
	}
	return out, nil
}

// CVVDP runs ColorVideoVDP's command-line tool.
type CVVDP struct {
	Bin  string // the cvvdp command; "cvvdp" if empty
	Exec Executor
	// Version is cvvdp's version, part of every scorer's identity.
	Version string
}

func (c *CVVDP) bin() string {
	if c.Bin == "" {
		return "cvvdp"
	}
	return c.Bin
}

// CVVDPVersion asks the Python that runs bin (found on PATH) which
// version of the cvvdp package it has.
func CVVDPVersion(ctx context.Context, ex Executor, bin string) (string, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	line, _ := bufio.NewReader(f).ReadString('\n')
	f.Close()
	interp, ok := strings.CutPrefix(strings.TrimSpace(line), "#!")
	if !ok {
		return "", fmt.Errorf("%s isn't a Python script; set its version with -cvvdp-version", path)
	}
	argv := append(strings.Fields(interp), "-c", "import importlib.metadata as m; print(m.version('cvvdp'))")
	out, err := ex.Run(ctx, argv)
	if err != nil {
		return "", fmt.Errorf("cvvdp's version: %w", err)
	}
	v := strings.TrimSpace(string(out))
	if v == "" || strings.ContainsAny(v, " ,\n") {
		return "", fmt.Errorf("cvvdp's version: got %q", v)
	}
	return v, nil
}

// ScorerName is the identity of scoring with s: the metric, its
// version, the display model, and a hash of the display models file if
// one is given.
func (c *CVVDP) ScorerName(s *ScorerConfig) (string, error) {
	name := fmt.Sprintf("cvvdp %s %s", c.Version, s.Display)
	if s.DisplayModels != "" {
		data, err := os.ReadFile(s.DisplayModels)
		if err != nil {
			return "", err
		}
		h := sha256.Sum256(data)
		name += " models=" + hex.EncodeToString(h[:])[:12]
	}
	return name, nil
}

// Score scores each of tests against ref, in one run of cvvdp, and
// returns their JODs in order. tmp is a directory for its result file.
func (c *CVVDP) Score(ctx context.Context, s *ScorerConfig, ref string, tests []string, tmp string) ([]float64, error) {
	res, err := os.CreateTemp(tmp, "cvvdp-*.csv")
	if err != nil {
		return nil, err
	}
	res.Close()
	defer os.Remove(res.Name())
	argv := []string{c.bin(), "--quiet", "--result", res.Name(), "--display", s.Display}
	if s.DisplayModels != "" {
		argv = append(argv, "--config-paths", s.DisplayModels)
	}
	if s.Device != "" {
		argv = append(argv, "--device", s.Device)
	}
	if s.GPUMem > 0 {
		argv = append(argv, "--gpu-mem", strconv.FormatFloat(s.GPUMem, 'f', -1, 64))
	}
	argv = append(argv, "--ref", ref, "--test")
	argv = append(argv, tests...)
	_, err = c.Exec.Run(ctx, argv)
	if err != nil && strings.Contains(err.Error(), "--ffmpeg-cc") {
		// Its GPU color conversion can't do some pixel formats, such as
		// full-range yuvj420p, and it asks for ffmpeg's instead, which
		// is slower.
		slog.Info("Retrying with --ffmpeg-cc", "ref", ref, "err", err)
		argv = slices.Insert(argv, 1, "--ffmpeg-cc")
		_, err = c.Exec.Run(ctx, argv)
	}
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(res.Name())
	if err != nil {
		return nil, err
	}
	return parseCVVDPResult(data, tests)
}

// parseCVVDPResult reads cvvdp's --result file: a header, then
// "test, reference, value" for each test, in order.
func parseCVVDPResult(data []byte, tests []string) ([]float64, error) {
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != len(tests)+1 {
		return nil, fmt.Errorf("cvvdp gave %d results for %d encodes", len(lines)-1, len(tests))
	}
	out := make([]float64, len(tests))
	for i, l := range lines[1:] {
		if !strings.HasPrefix(l, tests[i]+", ") {
			return nil, fmt.Errorf("cvvdp result %d is for %q, want %s", i, l, tests[i])
		}
		f := l[strings.LastIndex(l, ", ")+2:]
		v, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
		if err != nil {
			return nil, fmt.Errorf("cvvdp result for %s: %w", tests[i], err)
		}
		out[i] = v
	}
	return out, nil
}
