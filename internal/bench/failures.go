package bench

import (
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

// Failure is an encode, scoring or whole run that failed: a line of
// failures.csv, next to results.csv. Failed encodes have no row in
// results.csv, since a row there is a measurement the search reuses.
type Failure struct {
	Time    time.Time
	Host    string
	Job     string
	Config  string
	Sample  string
	Flagset string
	// Quality is the setting that failed, or NaN for a failure that
	// isn't one encode's.
	Quality float64
	// Stage is "encode", "score" or "run" (anything else that stopped
	// the search, such as making a scaled sample).
	Stage           string
	Err             string
	FFmpegVersion   string
	EncbenchVersion string
}

var failureColumns = []string{"time", "host", "job", "config", "sample", "flagset", "quality",
	"stage", "error", "ffmpeg_version", "encbench_version"}

// AppendFailure adds f to workdir's failures.csv.
func AppendFailure(workdir string, f Failure) error {
	path := filepath.Join(workdir, "failures.csv")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	w := csv.NewWriter(file)
	if st.Size() == 0 {
		w.Write(failureColumns)
	}
	q := ""
	if !math.IsNaN(f.Quality) {
		q = ftoa(f.Quality)
	}
	// One line per failure: errors from ffmpeg can span several.
	msg := strings.Join(strings.Fields(f.Err), " ")
	w.Write([]string{f.Time.UTC().Format(time.RFC3339), f.Host, f.Job, f.Config, f.Sample, f.Flagset, q,
		f.Stage, msg, f.FFmpegVersion, f.EncbenchVersion})
	w.Flush()
	if err := errors.Join(w.Error(), file.Close()); err != nil {
		return fmt.Errorf("appending to %s: %w", path, err)
	}
	return nil
}

// BuildVersion identifies this encbench build: its module version
// (a pseudo-version such as v0.0.0-20261111172332-2ffc873f679e for go
// install @main), else its VCS revision, "-dirty" if it had uncommitted
// changes, else "".
func BuildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev string
	dirty := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev != "" && dirty {
		rev += "-dirty"
	}
	return rev
}
