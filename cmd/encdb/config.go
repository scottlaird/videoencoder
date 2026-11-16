package main

import (
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v4"
)

// Config says where encdb build finds encbench's results and how to
// label them.
type Config struct {
	// Bench is the directory that relative Logs and Probe paths are in.
	// A relative Bench is relative to the config file.
	Bench string `yaml:"bench"`
	// Logs are results.csv files (or snapshots of them), read in order.
	// The same encode often turns up in several; the first log listed
	// that has it is the one the row's log column names.
	Logs []string `yaml:"logs"`
	// Hosts are encbench servers (such as "http://render1:8090") whose
	// /api/results are read after Logs, labeled "api:" plus the host.
	Hosts []string `yaml:"hosts"`
	// Machines describes each value of the host column.
	Machines map[string]Machine `yaml:"machines"`
	// Probe are directories of samples to ffprobe for their bit depth:
	// the sources, and encbench's scaled copies of them.
	Probe []string `yaml:"probe"`
	// Standard is, per resolution ("1080p", "4k"), the clips and scorer
	// that charts average over: std_clip and std_scorer.
	Standard map[string]Standard `yaml:"standard"`
	// Retime replaces some answers' times with those of a later timing
	// run; see retime.
	Retime []Retime `yaml:"retime"`
}

// Machine is what a host is, for labels and grouping.
type Machine struct {
	Machine  string `yaml:"machine"`  // a short name, such as "mac"
	Hardware string `yaml:"hardware"` // the whole machine, such as "TR 5975WX + RTX 4090"
	CPU      string `yaml:"cpu"`      // what runs software encoders
	GPU      string `yaml:"gpu"`      // what runs hardware encoders; empty if none
}

// Standard is the set of clips a resolution's charts use.
type Standard struct {
	Scorer  string   `yaml:"scorer"`
	Samples []string `yaml:"samples"`
}

// Retime names a log whose encodes re-timed a host's earlier ones.
type Retime struct {
	Host string `yaml:"host"`
	Log  string `yaml:"log"` // one of Config.Logs
	// SameTime are variant tokens that don't change how long an encode
	// takes, so a flagset with them takes the plain one's times when
	// their files are identical (such as Quick Sync's B-pyramid, bpyr).
	SameTime []string `yaml:"same_time"`
	// Families limits it to these encoder families (see Attrs); empty
	// is every family.
	Families []string `yaml:"families"`
}

// loadConfig reads the config at path, with Bench made absolute.
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Load(data, &c, yaml.WithKnownFields()); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !filepath.IsAbs(c.Bench) {
		c.Bench = filepath.Join(filepath.Dir(path), c.Bench)
	}
	for _, r := range c.Retime {
		found := false
		for _, l := range c.Logs {
			found = found || l == r.Log
		}
		if !found {
			return nil, fmt.Errorf("%s: retime log %q isn't one of logs", path, r.Log)
		}
	}
	return &c, nil
}

func (c *Config) path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Bench, p)
}
