package tune

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"time"
)

// ResultsFile is the name of the results file kept in a video's
// outdir.
const ResultsFile = "tune-results.csv"

// A trial is one encode and scoring of the sample.
type trial struct {
	vmaf, bitrate float64
}

// trialKey identifies everything that decides a trial's score except
// its quality setting: the sample (the source's content, where it was
// cut and for how long), the encode's arguments, the reference it is
// judged against, the ffmpeg build, and how it is scored. Trials with
// equal keys and qualities are interchangeable.
type trialKey struct {
	source          string // fileid.Content of the source
	start, duration string
	args            []string // the encode's arguments, quality left out
	reference       string   // reference size, tone map and transfer
	prefilter       string
	ffmpeg, scorer  string
}

func (k trialKey) hash() string {
	h := sha256.New()
	fmt.Fprintf(h, "tune/v1\x00%s\x00%s\x00%s", k.source, k.start, k.duration)
	for _, a := range k.args {
		fmt.Fprintf(h, "\x00%s", a)
	}
	fmt.Fprintf(h, "\x00\x00%s\x00%s\x00%s\x00%s", k.reference, k.prefilter, k.ffmpeg, k.scorer)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// resultsHeader names the columns of a tune results file. Only key,
// quality, vmaf and bitrate are read back; the rest are for people.
var resultsHeader = []string{"time", "key", "rendition", "quality", "vmaf", "bitrate", "start", "duration", "ffmpeg", "scorer", "args"}

// results are the trials recorded in a results file, by key and then
// by quality as written by strconv.FormatFloat(q, 'f', -1, 64).
type results struct {
	path   string // "" keeps trials in memory only
	trials map[string]map[string]trial
}

// loadResults reads the trials in path. A missing file has none.
func loadResults(path string) (*results, error) {
	r := &results{path: path, trials: map[string]map[string]trial{}}
	if path == "" {
		return r, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cr := csv.NewReader(f)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err == io.EOF {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[h] = i
	}
	for _, c := range []string{"key", "quality", "vmaf", "bitrate"} {
		if _, ok := col[c]; !ok {
			return nil, fmt.Errorf("%s: no %q column", path, c)
		}
	}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			return r, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		get := func(c string) string {
			if i := col[c]; i < len(rec) {
				return rec[i]
			}
			return ""
		}
		vmaf, err1 := strconv.ParseFloat(get("vmaf"), 64)
		bitrate, err2 := strconv.ParseFloat(get("bitrate"), 64)
		if err1 != nil || err2 != nil {
			continue // a torn last line from an interrupted run
		}
		r.add(get("key"), get("quality"), trial{vmaf, bitrate})
	}
}

func (r *results) add(key, quality string, t trial) {
	if r.trials[key] == nil {
		r.trials[key] = map[string]trial{}
	}
	r.trials[key][quality] = t
}

// record adds a trial and appends it to the file, so an interrupted
// run keeps what it measured.
func (r *results) record(k trialKey, rendition string, quality float64, t trial) error {
	key, q := k.hash(), strconv.FormatFloat(quality, 'f', -1, 64)
	r.add(key, q, t)
	if r.path == "" {
		return nil
	}
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w := csv.NewWriter(f)
	if st.Size() == 0 {
		w.Write(resultsHeader)
	}
	w.Write([]string{
		time.Now().UTC().Format(time.RFC3339), key, rendition, q,
		strconv.FormatFloat(t.vmaf, 'f', 4, 64), strconv.FormatFloat(t.bitrate, 'f', 0, 64),
		k.start, k.duration, k.ffmpeg, k.scorer, fmt.Sprint(k.args),
	})
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// lookup returns the trial for k at quality, if one is recorded.
func (r *results) lookup(k trialKey, quality float64) (trial, bool) {
	t, ok := r.trials[k.hash()][strconv.FormatFloat(quality, 'f', -1, 64)]
	return t, ok
}
