// Package benchqueue runs encbench searches from a directory of work
// files, one (sample, flagset) unit at a time, in an order the files'
// names set, picking up added, edited and deleted files between units.
package benchqueue

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/scottlaird/videoencoder/internal/bench"
)

// A work file is either a bench config, whose every flagset runs on
// every sample, or a job naming a bench config and which of its
// flagsets and samples to run:
//
//	config: ../x264-tunes-1080p.yaml
//	flagsets: [x264-ultrafast-kf10-film, x264-*-kf10-film]
//	samples: [20180529-band.mp4, "2026*-sdr.mov"]
//	report: ../report-x264-film.md
//
// Flagsets run in the order the job lists them, or else the order the
// config does; samples in the order the job lists them, or else by
// name. Entries may be path.Match patterns, which expand in that
// default order, and each must match something. A unit is one flagset
// on one sample; all of a file's samples run with a flagset before the
// next flagset starts.
type job struct {
	Config   string   `yaml:"config"`
	Flagsets []string `yaml:"flagsets"`
	Samples  []string `yaml:"samples"`
	Report   string   `yaml:"report"`
}

// workFile is a parsed work file.
type workFile struct {
	name string // base name, which sets the file's priority
	// hash changes whenever the file or the config it names does.
	hash string
	cfg  *bench.Config
	// configPath is the bench config, the work file itself unless it's
	// a job.
	configPath string
	flagsets   []string // in run order
	samples    []string // patterns in run order; nil is all, by name
	report     string   // where to write the report; "" for the default
}

// isWorkFile reports whether a directory entry named name is a work
// file, rather than, say, an editor's backup or a report.
func isWorkFile(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "#") {
		return false
	}
	ext := filepath.Ext(name)
	return ext == ".yaml" || ext == ".yml"
}

// loadWorkFile parses the work file at path.
func loadWorkFile(path string) (*workFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write(data)
	wf := &workFile{name: filepath.Base(path), configPath: path}

	var doc yaml.Node
	if err := yaml.Load(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	cfgData := data
	if mappingValue(&doc, "config") != nil {
		var j job
		if err := yaml.Load(data, &j, yaml.WithKnownFields()); err != nil {
			return nil, fmt.Errorf("parsing: %w", err)
		}
		if j.Config == "" {
			return nil, fmt.Errorf("config: required")
		}
		wf.configPath = resolve(filepath.Dir(path), j.Config)
		if cfgData, err = os.ReadFile(wf.configPath); err != nil {
			return nil, err
		}
		h.Write([]byte{0})
		h.Write(cfgData)
		wf.samples = j.Samples
		if j.Report != "" {
			wf.report = resolve(filepath.Dir(path), j.Report)
		}
		wf.flagsets = j.Flagsets
	}
	wf.hash = hex.EncodeToString(h.Sum(nil))[:16]

	if wf.cfg, err = bench.LoadConfig(wf.configPath); err != nil {
		return nil, err
	}
	wf.cfg.Job = wf.name
	order, err := flagsetOrder(cfgData)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", wf.configPath, err)
	}
	if wf.flagsets == nil {
		wf.flagsets = order
	} else if wf.flagsets, err = expand(wf.flagsets, order, "flagset"); err != nil {
		return nil, err
	}
	return wf, nil
}

// flagsetOrder lists a bench config's flagsets in the order it gives
// them.
func flagsetOrder(data []byte) ([]string, error) {
	var doc yaml.Node
	if err := yaml.Load(data, &doc); err != nil {
		return nil, err
	}
	fs := mappingValue(&doc, "flagsets")
	if fs == nil || fs.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("flagsets: not a mapping")
	}
	var names []string
	for i := 0; i+1 < len(fs.Content); i += 2 {
		names = append(names, fs.Content[i].Value)
	}
	return names, nil
}

// mappingValue returns the value of key in the document's top-level
// mapping, or nil.
func mappingValue(doc *yaml.Node, key string) *yaml.Node {
	n := doc
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// expand replaces each of patterns with the names it matches, in the
// order names has them, dropping repeats. A pattern matching nothing
// is an error, as a likely typo.
func expand(patterns, names []string, what string) ([]string, error) {
	var out []string
	for _, p := range patterns {
		n := 0
		for _, name := range names {
			if ok, err := path.Match(p, name); err != nil {
				return nil, fmt.Errorf("%ss: %q: %w", what, p, err)
			} else if ok {
				n++
				if !slices.Contains(out, name) {
					out = append(out, name)
				}
			}
		}
		if n == 0 {
			return nil, fmt.Errorf("%ss: %q matches no %s", what, p, what)
		}
	}
	return out, nil
}

func resolve(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// reportPath is where the file's report goes: as its job says, or
// else report-<name>.md beside it.
func (wf *workFile) reportPath(dir string) string {
	if wf.report != "" {
		return wf.report
	}
	return filepath.Join(dir, "report-"+strings.TrimSuffix(wf.name, filepath.Ext(wf.name))+".md")
}
