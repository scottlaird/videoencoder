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

// quietEnv are the variables QuietEnv adds, each only if it isn't
// already set:
//
//   - SVT_LOG=1 limits SVT-AV1 to errors.
//   - LIBVA_MESSAGING_LEVEL=1 limits libva to errors. Otherwise, each
//     time a VA-API or Quick Sync encoder opens on Linux, it prints
//     "libva info:" lines past ffmpeg's -loglevel.
var quietEnv = [][2]string{
	{"SVT_LOG", "1"},
	{"LIBVA_MESSAGING_LEVEL", "1"},
}

// QuietEnv is the environment for ffmpeg: this process's, with the
// libraries that log to stderr on their own limited to errors, unless
// their variables are already set.
func QuietEnv() []string {
	env := os.Environ()
	for _, kv := range quietEnv {
		if _, ok := os.LookupEnv(kv[0]); !ok {
			env = append(env, kv[0]+"="+kv[1])
		}
	}
	return env
}
