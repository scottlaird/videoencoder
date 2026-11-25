// Package score scores encodes that encbench queues serve over HTTP
// (see benchweb's file routes) with metrics encbench doesn't run
// inline, cvvdp first, and logs the scores apart from encbench's own
// results, keyed so they join back to every row with the same output.
package score

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/scottlaird/videoencoder/internal/benchweb"
)

// Job is a work file: which queues to read, which of their encodes to
// score, and how.
type Job struct {
	// Path is the file the job was loaded from.
	Path string `yaml:"-"`
	// Hosts are encbench queue web views, such as "http://render3:8090".
	Hosts []string `yaml:"hosts"`
	// TokenFile, if set, holds the queues' bearer token
	// (encbench queue -http-token-file).
	TokenFile string `yaml:"token_file"`
	Token     string `yaml:"-"`
	Select    Select `yaml:"select"`
	// Scorers says how to score what Select picks. Each row is scored
	// by every scorer that applies to it.
	Scorers []ScorerConfig `yaml:"scorers"`
}

// Select picks rows from the queues' results. Every field that is set
// must match; an empty list matches anything.
type Select struct {
	Samples  []string `yaml:"samples"` // sample names, as in results.csv; globs allowed
	Encoders []string `yaml:"encoders"`
	Flagsets []string `yaml:"flagsets"` // globs allowed
	Heights  []int64  `yaml:"heights"`
	// SHA256s picks encodes by out_sha256.
	SHA256s []string `yaml:"sha256s"`
	// Ladders includes ladder rungs, which are left out by default.
	Ladders bool `yaml:"ladders"`
}

// ScorerConfig is one way of scoring an encode.
type ScorerConfig struct {
	// Metric is "cvvdp", the only one so far.
	Metric string `yaml:"metric"`
	// Display is the cvvdp display model, such as "standard_fhd".
	Display string `yaml:"display"`
	// DisplayModels, if set, is a display_models.json (relative to the
	// job file) defining Display. Its contents are part of the scorer's
	// identity, so editing it starts a new set of scores.
	DisplayModels string `yaml:"display_models"`
	// Heights limits the scorer to samples of these heights.
	Heights []int64 `yaml:"heights"`
	// Transfer limits it to "sdr" or "hdr" samples (by color_transfer:
	// PQ and HLG are HDR); empty means either.
	Transfer string `yaml:"transfer"`
	// Device is cvvdp's --device, such as "cuda:0" or "mps"; empty lets
	// cvvdp choose.
	Device string `yaml:"device"`
	// GPUMem is cvvdp's --gpu-mem, in GB; 0 leaves it to cvvdp.
	GPUMem float64 `yaml:"gpu_mem"`
}

// LoadJob reads a work file.
func LoadJob(path string) (*Job, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var j Job
	if err := yaml.Load(data, &j, yaml.WithKnownFields()); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	j.Path = path
	if err := j.finish(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &j, nil
}

func (j *Job) finish(base string) error {
	if len(j.Hosts) == 0 {
		return fmt.Errorf("hosts: required")
	}
	for i, h := range j.Hosts {
		if !strings.HasPrefix(h, "http://") && !strings.HasPrefix(h, "https://") {
			return fmt.Errorf("hosts: %q is not an http:// or https:// URL", h)
		}
		j.Hosts[i] = strings.TrimSuffix(h, "/")
	}
	if j.TokenFile != "" {
		tok, err := os.ReadFile(resolve(base, j.TokenFile))
		if err != nil {
			return fmt.Errorf("token_file: %w", err)
		}
		if j.Token = strings.TrimSpace(string(tok)); j.Token == "" {
			return fmt.Errorf("token_file: empty")
		}
	}
	for _, p := range append(slices.Clone(j.Select.Samples), j.Select.Flagsets...) {
		if _, err := filepath.Match(p, ""); err != nil {
			return fmt.Errorf("select: bad pattern %q", p)
		}
	}
	if len(j.Scorers) == 0 {
		return fmt.Errorf("scorers: required")
	}
	for i := range j.Scorers {
		s := &j.Scorers[i]
		if s.Metric != "cvvdp" {
			return fmt.Errorf("scorers[%d]: metric %q is not cvvdp", i, s.Metric)
		}
		if s.Display == "" {
			return fmt.Errorf("scorers[%d]: display: required", i)
		}
		if s.Transfer != "" && s.Transfer != "sdr" && s.Transfer != "hdr" {
			return fmt.Errorf("scorers[%d]: transfer %q is not sdr or hdr", i, s.Transfer)
		}
		if s.DisplayModels != "" {
			s.DisplayModels = resolve(base, s.DisplayModels)
			if _, err := os.Stat(s.DisplayModels); err != nil {
				return fmt.Errorf("scorers[%d]: display_models: %w", i, err)
			}
		}
	}
	return nil
}

func resolve(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// picks says whether the job's selection includes row.
func (s *Select) picks(row *benchweb.ResultRow) bool {
	if row.Ladder != "" && !s.Ladders {
		return false
	}
	if !matchAny(s.Samples, row.Sample) || !matchAny(s.Flagsets, row.Flagset) {
		return false
	}
	if len(s.Encoders) > 0 && !slices.Contains(s.Encoders, row.Encoder) {
		return false
	}
	if len(s.Heights) > 0 && (row.Height == nil || !slices.Contains(s.Heights, *row.Height)) {
		return false
	}
	if len(s.SHA256s) > 0 && !slices.Contains(s.SHA256s, row.OutSHA256) {
		return false
	}
	return true
}

// applies says whether the scorer is for row's sample.
func (c *ScorerConfig) applies(row *benchweb.ResultRow) bool {
	if len(c.Heights) > 0 && (row.Height == nil || !slices.Contains(c.Heights, *row.Height)) {
		return false
	}
	switch c.Transfer {
	case "sdr":
		return !isHDR(row.ColorTransfer)
	case "hdr":
		return isHDR(row.ColorTransfer)
	}
	return true
}

func isHDR(transfer string) bool { return transfer == "smpte2084" || transfer == "arib-std-b67" }

func matchAny(patterns []string, s string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, s); ok {
			return true
		}
	}
	return false
}
