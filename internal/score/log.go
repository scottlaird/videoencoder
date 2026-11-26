package score

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Score is one metric's value for one encode: a line of scores.csv.
type Score struct {
	// Key identifies the scoring: Scorer, SampleID and OutSHA256 (see
	// Key). Every encbench row with that sample and output shares it,
	// on whichever machine it was encoded.
	Key    string
	Scorer string // the method and its version, such as "cvvdp 0.5.7 standard_fhd"
	Metric string // what Value is, such as "jod"
	Value  float64
	// SampleID and OutSHA256 are the encbench row's sample_id (which
	// covers its reference) and out_sha256; SourceID its source_id.
	SampleID, SourceID, OutSHA256 string
	// ScoreS is how long scoring took, in seconds. Encodes scored in
	// one run of the scorer share its time evenly.
	ScoreS float64
	// Host and Device are where it was scored.
	Host, Device string
	Time         time.Time
	// From is the queue the encode was fetched from, and Sample,
	// Flagset and Quality the row it was found by, for reading the log
	// by eye; other rows with the same Key are scored too.
	From, Sample, Flagset string
	Quality               float64
}

var scoreColumns = []string{"key", "scorer", "metric", "value", "sample_id", "source_id", "out_sha256",
	"score_s", "host", "device", "time", "from", "sample", "flagset", "quality"}

// Key is the key of a scoring of the encode out (a SHA-256) of sample
// by scorer.
func Key(scorer, sampleID, out string) string {
	h := sha256.New()
	for _, p := range []string{"score/v1", scorer, sampleID, out} {
		fmt.Fprintf(h, "%d:%s\n", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Log is scores.csv, appended to as scores come in.
type Log struct {
	path string
	mu   sync.Mutex
	rows []Score
	keys map[string]bool
}

// OpenLog reads the log at path, creating it if need be.
func OpenLog(path string) (*Log, error) {
	l := &Log{path: path, keys: map[string]bool{}}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err == io.EOF {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[h] = i
	}
	for _, c := range []string{"key", "scorer", "metric", "value"} {
		if _, ok := col[c]; !ok {
			return nil, fmt.Errorf("%s: no %s column", path, c)
		}
	}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		var pe *csv.ParseError
		if errors.As(err, &pe) {
			break // a line still being written
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		get := func(c string) string {
			if i, ok := col[c]; ok && i < len(rec) {
				return rec[i]
			}
			return ""
		}
		s := Score{Key: get("key"), Scorer: get("scorer"), Metric: get("metric"),
			SampleID: get("sample_id"), SourceID: get("source_id"), OutSHA256: get("out_sha256"),
			Host: get("host"), Device: get("device"), From: get("from"), Sample: get("sample"), Flagset: get("flagset")}
		if s.Value, err = strconv.ParseFloat(get("value"), 64); err != nil {
			continue // partial
		}
		s.ScoreS, _ = strconv.ParseFloat(get("score_s"), 64)
		s.Quality, _ = strconv.ParseFloat(get("quality"), 64)
		s.Time, _ = time.Parse(time.RFC3339, get("time"))
		l.rows = append(l.rows, s)
		l.keys[s.Key] = true
	}
	return l, nil
}

// Has says whether the log has a score with key.
func (l *Log) Has(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.keys[key]
}

// Rows returns the log's scores, in order.
func (l *Log) Rows() []Score {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.rows)
}

// Append adds scores to the log, writing the header if it's new.
func (l *Log) Append(scores ...Score) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
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
		w.Write(scoreColumns)
	}
	for _, s := range scores {
		w.Write([]string{s.Key, s.Scorer, s.Metric, strconv.FormatFloat(s.Value, 'f', -1, 64),
			s.SampleID, s.SourceID, s.OutSHA256, strconv.FormatFloat(s.ScoreS, 'f', 2, 64),
			s.Host, s.Device, s.Time.UTC().Format(time.RFC3339), s.From, s.Sample, s.Flagset,
			strconv.FormatFloat(s.Quality, 'f', -1, 64)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	for _, s := range scores {
		l.rows = append(l.rows, s)
		l.keys[s.Key] = true
	}
	return nil
}
