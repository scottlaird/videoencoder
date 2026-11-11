package bench

import (
	"context"
	"path/filepath"

	"github.com/scottlaird/videoencoder/internal/probe"
)

// EventKind is what an Event says happened.
type EventKind int

const (
	// Encoding: an encode of Sample at Quality began.
	Encoding EventKind = iota
	// Scored: that encode measured VMAF at Kbps, and Pass says whether
	// it met the target.
	Scored
	// Failed: the encode or its scoring failed with Err, so the search
	// for Flagset on Sample gives up.
	Failed
	// Finished: the search for Flagset on Sample ended with Outcome.
	// Quality is its answer, and Kbps and VMAF that encode's, when
	// there is one (Pass is then true).
	Finished
	// Timing: the answer at Quality, first encoded with others (see
	// Config.Batch), is being encoded again by itself for its time.
	Timing
)

// Event is a step of a run, for showing its progress.
type Event struct {
	Kind EventKind
	// Round counts the run's batches of encodes, from 1; it is 0 for
	// Finished.
	Round int
	// Source is the file in the samples directory that Sample is, or
	// is a scaled copy of.
	Source, Sample string
	Flagset        string
	QualityFlag    string
	Quality        float64
	// Qualities are every setting an Encoding event's ffmpeg encodes:
	// Quality alone, or a batch's (see Config.Batch).
	Qualities  []float64
	Kbps, VMAF float64
	Pass       bool
	Outcome    Outcome
	Err        error
}

func (b *Bench) observe(e Event) {
	if b.Observe != nil {
		b.Observe(e)
	}
}

// PairStatus is how far the log has got with a flagset on one sample
// file, counting every scaled version of it.
type PairStatus struct {
	// Sample is the file's name in the samples directory, as the
	// SampleFilter matches it.
	Sample  string
	Flagset string
	// Done means the log answers the search on every version.
	Done bool
	// Skipped means there's nothing to search: the flagset is SDR-only
	// and the sample HDR.
	Skipped bool
}

// Status reports every (sample file, flagset) pair the filters allow,
// samples in name order and flagsets in FlagsetNames order, and
// whether results already answer each. Like Scorings, it makes no
// downscaled copies and encodes nothing.
func (b *Bench) Status(ctx context.Context) ([]PairStatus, error) {
	results, err := LoadResults(filepath.Join(b.Cfg.Workdir, "results.csv"))
	if err != nil {
		return nil, err
	}
	dry := *b
	dry.listing = true
	samples, err := dry.loadSamples(ctx)
	if err != nil {
		return nil, err
	}
	var out []PairStatus
	index := map[[2]string]int{}
	for _, s := range samples {
		for _, name := range b.Cfg.FlagsetNames() {
			if !matches(b.FlagsetFilter, name) {
				continue
			}
			fs := b.Cfg.Flagsets[name]
			k := [2]string{s.source, name}
			i, ok := index[k]
			if !ok {
				i = len(out)
				index[k] = i
				out = append(out, PairStatus{Sample: s.source, Flagset: name, Done: true, Skipped: true})
			}
			if fs.SDROnly && s.vd.Tone() == probe.HDR {
				continue
			}
			out[i].Skipped = false
			if done, _ := settled(b.Cfg, fs, flagsetKey(fs), s, results); !done {
				out[i].Done = false
			}
		}
	}
	for i := range out {
		if out[i].Skipped {
			out[i].Done = false
		}
	}
	return out, nil
}
