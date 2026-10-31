package ffmpeg

import (
	"os"
	"slices"
	"strings"
)

// x265Quiet is the x265 option that keeps its banner and per-encode
// summary, logged at info, off stderr.
const x265Quiet = "log-level=error"

// Quiet returns argv with the encoder libraries that log to stderr
// themselves, ignoring ffmpeg's -loglevel, told to log only errors:
// every libx265 output gets log-level=error, merged into its
// -x265-params if it has one. It is applied when a command runs, not
// to the arguments jobs and trials are keyed by, so it changes no
// output's name.
func Quiet(argv []string) []string {
	out := slices.Clone(argv)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "-x265-params" && !strings.Contains(out[i+1], "log-level=") {
			out[i+1] += ":" + x265Quiet
		}
	}
	for i := len(out) - 2; i >= 0; i-- {
		if !isCodecFlag(out[i]) || out[i+1] != "libx265" {
			continue
		}
		// An output's own -x265-params, already merged above, comes
		// after its codec and before the next output's.
		end := len(out)
		for j := i + 2; j < len(out); j++ {
			if isCodecFlag(out[j]) {
				end = j
				break
			}
		}
		if !slices.Contains(out[i+2:end], "-x265-params") {
			out = slices.Insert(out, i+2, "-x265-params", x265Quiet)
		}
	}
	return out
}

func isCodecFlag(s string) bool {
	return s == "-c:v" || s == "-codec:v" || s == "-vcodec"
}

// QuietEnv is the environment for ffmpeg: this process's, with
// SVT-AV1 limited to errors (SVT_LOG=1) unless SVT_LOG is already set.
func QuietEnv() []string {
	env := os.Environ()
	if _, ok := os.LookupEnv("SVT_LOG"); !ok {
		env = append(env, "SVT_LOG=1")
	}
	return env
}
