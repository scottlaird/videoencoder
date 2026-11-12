package bench

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// FFmpegVersion returns the version from the first line of
// "ffmpeg -version", such as "8.1".
func FFmpegVersion(ctx context.Context) (string, error) {
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

// CPUModel describes this machine's processor for the results log,
// such as "AMD Ryzen Threadripper PRO 5975WX 32-Cores, 64 threads", or
// just the thread count where the model can't be read.
func CPUModel(ctx context.Context) string {
	model := ""
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.CommandContext(ctx, "sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			model = strings.TrimSpace(string(out))
		}
	case "linux":
		if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			model = cpuinfoModel(string(data))
		}
	}
	threads := fmt.Sprintf("%d threads", runtime.NumCPU())
	if model == "" {
		return threads
	}
	return model + ", " + threads
}

// cpuinfoModel returns the first "model name" in /proc/cpuinfo text.
func cpuinfoModel(cpuinfo string) string {
	for line := range strings.Lines(cpuinfo) {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == "model name" {
			return strings.Join(strings.Fields(v), " ")
		}
	}
	return ""
}

// EncoderVersion identifies the library behind an ffmpeg encoder, such
// as "x265 4.3+1-e9b8812 (NEON Neon_DotProd Neon_I8MM)", by encoding two
// tiny frames with it and reading what the library reports: its version,
// and the SIMD extensions it chose where it says. VideoToolbox encoders
// report the macOS version. It returns "" for encoders it can't
// identify, or that fail to run.
func EncoderVersion(ctx context.Context, codec string) string {
	if strings.HasSuffix(codec, "_videotoolbox") {
		out, err := exec.CommandContext(ctx, "sw_vers", "-productVersion").Output()
		if err != nil {
			return ""
		}
		return "macOS " + strings.TrimSpace(string(out))
	}
	// Hardware encoders: the GPU and its driver, which is what changes
	// their output.
	if strings.HasSuffix(codec, "_nvenc") {
		out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=name,driver_version", "--format=csv,noheader").Output()
		if err != nil {
			return ""
		}
		return nvidiaVersion(string(out))
	}
	if strings.HasSuffix(codec, "_qsv") || strings.HasSuffix(codec, "_vaapi") {
		// vainfo writes its driver line to stdout or stderr, by version.
		out, _ := exec.CommandContext(ctx, "vainfo").CombinedOutput()
		return vaDriverVersion(string(out))
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-nostdin", "-v", "info",
		"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=1", "-frames:v", "2",
		"-c:v", codec, "-f", "nut", "-")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return ""
	}
	return parseEncoderVersion(codec, stdout.String(), stderr.String())
}

var (
	x264Core     = regexp.MustCompile(`x264 - core (\d+ r\d+ [0-9a-f]+)`)
	x265Version  = regexp.MustCompile(`HEVC encoder version (\S+)`)
	svtVersion   = regexp.MustCompile(`SVT-AV1 Encoder Lib (v\S+)`)
	aomVersion   = regexp.MustCompile(`\[libaom-av1 @ 0x[0-9a-f]+\] (v?\d+\.\d+\.\d+\S*)`)
	cpuCaps      = regexp.MustCompile(`using cpu capabilities: (.+)`)
	versionNames = map[string]struct {
		name string
		re   *regexp.Regexp
		bits bool // found in the bitstream, not the log
	}{
		"libx264":    {"x264 core", x264Core, true},
		"libx265":    {"x265", x265Version, false},
		"libsvtav1":  {"SVT-AV1", svtVersion, false},
		"libaom-av1": {"libaom", aomVersion, false},
	}
)

// parseEncoderVersion finds codec's version in an encode's output
// (stdout, where x264 writes its version into the bitstream) and log
// (stderr).
func parseEncoderVersion(codec, stdout, stderr string) string {
	v, ok := versionNames[codec]
	if !ok {
		return ""
	}
	text := stderr
	if v.bits {
		text = stdout
	}
	m := v.re.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	s := v.name + " " + m[1]
	if c := cpuCaps.FindStringSubmatch(stderr); c != nil {
		s += " (" + strings.TrimSpace(c[1]) + ")"
	}
	return s
}

// nvidiaVersion reads nvidia-smi's "name, driver_version" lines (one
// per GPU) as "NVIDIA GeForce RTX 4090, driver 615.71.09", the first
// GPU's.
func nvidiaVersion(out string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	name, driver, ok := strings.Cut(line, ",")
	if !ok {
		return ""
	}
	return strings.TrimSpace(name) + ", driver " + strings.TrimSpace(driver)
}

var vaDriver = regexp.MustCompile(`(?m)Driver version:\s*(.+?)\s*$`)

// vaDriverVersion reads vainfo's driver line, such as "Intel iHD
// driver for Intel(R) Gen Graphics - 26.1.2 ()".
func vaDriverVersion(out string) string {
	m := vaDriver.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSpace(m[1]), " ()")
}
