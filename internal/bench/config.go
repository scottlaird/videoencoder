// Package bench finds, for each candidate set of encoder flags, the
// lowest-quality setting (and so the smallest file) that still reaches
// a target VMAF on each sample clip.
package bench

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"

	"go.yaml.in/yaml/v4"
)

// Config is the contents of bench.yaml. Relative paths are relative to
// the config file.
type Config struct {
	// Samples is a directory of sample clips.
	Samples string `yaml:"samples"`
	// Workdir holds results.csv, best.csv, kept encodes and scratch
	// files. Defaults to "bench".
	Workdir string `yaml:"workdir"`
	// Scales lists heights to test samples at. Taller samples are
	// downscaled once (cached in workdir/samples); a sample no taller
	// than a scale is used as it is. Empty means native size only.
	Scales []uint64 `yaml:"scales"`
	// References maps a sample's file name to another file in Samples
	// to score it against instead of itself: say, an H.265 export
	// scored against the ProRes master it was made from, to see what
	// encoding from the export costs. The two must have the same
	// frames. Scaled copies are scored against the reference's copies.
	References map[string]string `yaml:"references"`
	// An encode passes when its mean VMAF reaches TargetVMAF (default
	// 95) and it meets the optional color limits. VMAF ignores chroma,
	// so a color cast only fails an encode through MinCIEDE2000 (a
	// higher-is-better score; ~50 is a barely visible shift) or
	// MinChromaPSNR (the worse of the Cb and Cr PSNRs, in dB).
	TargetVMAF    float64 `yaml:"target_vmaf"`
	MinCIEDE2000  float64 `yaml:"min_ciede2000"`
	MinChromaPSNR float64 `yaml:"min_chroma_psnr"`
	// MaxColorShift fails encodes whose block color shift at the
	// 99.9th percentile (Metrics.ColorShiftP999) exceeds it: a local
	// color cast that the frame averages above miss. 0 means no
	// limit. On the Pyramid Creek HDR sample, encodes that looked fine
	// measured at most 2.54 and those with a visible cast at least
	// 2.92, so 2.7 is a reasonable starting point.
	MaxColorShift float64 `yaml:"max_color_shift"`
	// OldMaxColorShift is max_color_shift's first spelling, still read.
	OldMaxColorShift float64 `yaml:"max_colour_shift"`
	// MaxColorFlicker fails encodes whose block color flicker at the
	// 99.9th percentile (Metrics.ColorFlickerP999) exceeds it: a color
	// cast that comes and goes. 0 means no limit. On Pyramid Creek,
	// fine encodes measured at most 1.12 and those with a visible
	// flickering cast at least 1.32; 1.25 is a starting point.
	MaxColorFlicker float64 `yaml:"max_color_flicker"`
	// VMAFThreads is passed to libvmaf's n_threads; VMAFJobs is how many
	// scorings run at once.
	VMAFThreads int `yaml:"vmaf_threads"`
	VMAFJobs    int `yaml:"vmaf_jobs"`
	// VMAFModel is a libvmaf JSON model to score SDR samples with,
	// VMAFModel2160 one for samples 2160p and up (default VMAFModel).
	// Unset, or BuiltinVMAFModel, is libvmaf's built-in v0.6.1 model,
	// which switches to its 4K variant at 2160p.
	VMAFModel     string `yaml:"vmaf_model"`
	VMAFModel2160 string `yaml:"vmaf_model_2160"`
	// HDR overrides the target and models for HDR samples.
	HDR      HDRScoring          `yaml:"hdr"`
	Flagsets map[string]*Flagset `yaml:"flagsets"`

	sdrModels, hdrModels modelPair
}

// HDRScoring is how HDR samples are judged. Unset fields fall back to
// the SDR ones. An HDR model (such as Netflix's hdr_vmaf_v1) scores
// both sides as HDR; any other model scores them tone-mapped to SDR.
type HDRScoring struct {
	TargetVMAF    float64 `yaml:"target_vmaf"`
	VMAFModel     string  `yaml:"vmaf_model"`
	VMAFModel2160 string  `yaml:"vmaf_model_2160"`
}

// BuiltinVMAFModel is the vmaf_model value for libvmaf's own model.
const BuiltinVMAFModel = "v0"

// modelPair is base, and uhd for samples 2160p and up.
type modelPair struct{ base, uhd VMAFModel }

func (m modelPair) forHeight(h uint64) VMAFModel {
	if h >= 2160 {
		return m.uhd
	}
	return m.base
}

func loadModelPair(base, uhd string) (modelPair, error) {
	load := func(path string) (VMAFModel, error) {
		if path == "" || path == BuiltinVMAFModel {
			return VMAFModel{}, nil
		}
		return LoadVMAFModel(path)
	}
	var p modelPair
	var err error
	if p.base, err = load(base); err != nil {
		return modelPair{}, err
	}
	p.uhd = p.base
	if uhd != "" {
		if p.uhd, err = load(uhd); err != nil {
			return modelPair{}, err
		}
	}
	return p, nil
}

// Scoring is how one sample's encodes are judged.
type Scoring struct {
	// Scorer is logged with each encode (Row.Scorer): the scoring
	// method's version, and the VMAF model's ID if it isn't built in.
	Scorer string
	Target float64
	Model  VMAFModel
}

// scoring returns how a sample of the given tone and height is judged.
func (c *Config) scoring(hdr bool, height uint64) Scoring {
	s := Scoring{Scorer: scorer, Target: c.TargetVMAF, Model: c.sdrModels.forHeight(height)}
	if hdr {
		s.Model = c.hdrModels.forHeight(height)
		if c.HDR.TargetVMAF != 0 {
			s.Target = c.HDR.TargetVMAF
		}
	}
	if id := s.Model.ID(); id != "" {
		s.Scorer += " " + id
	}
	return s
}

// Scorings maps sample names to how they're judged. Samples it doesn't
// list are judged as SDR with the built-in model.
type Scorings map[string]Scoring

func (sc Scorings) of(c *Config, sample string) Scoring {
	if s, ok := sc[sample]; ok {
		return s
	}
	return Scoring{Scorer: scorer, Target: c.TargetVMAF}
}

// Flagset is one candidate set of encoder options, minus the quality
// setting, which the search supplies.
type Flagset struct {
	Args    []string `yaml:"args"`
	Quality Quality  `yaml:"quality"`
	// SDROnly skips HDR samples, for encoders that shouldn't be used
	// for HDR: 8-bit H.264 in PQ, say.
	SDROnly bool `yaml:"sdr_only"`
}

// Quality describes the flag the search varies. Zero fields are filled
// from the encoder's defaults (see qualityDefaults).
type Quality struct {
	Flag string  `yaml:"flag"`
	Min  float64 `yaml:"min"`
	Max  float64 `yaml:"max"`
	Step float64 `yaml:"step"`
	// Seed is where the first search starts when no earlier results
	// suggest a better place.
	Seed float64 `yaml:"seed"`
	// HigherIsBetter is true for encoders where a larger number means
	// better quality, such as VideoToolbox's -q:v.
	HigherIsBetter *bool `yaml:"higher_is_better"`
}

func ptr[T any](v T) *T { return &v }

// qualityDefaults maps an ffmpeg encoder name to its quality flag.
var qualityDefaults = map[string]Quality{
	"libx264":           {Flag: "-crf", Min: 10, Max: 40, Step: 0.1, Seed: 20, HigherIsBetter: ptr(false)},
	"libx265":           {Flag: "-crf", Min: 10, Max: 40, Step: 0.1, Seed: 20, HigherIsBetter: ptr(false)},
	"libsvtav1":         {Flag: "-crf", Min: 10, Max: 63, Step: 1, Seed: 32, HigherIsBetter: ptr(false)},
	"libaom-av1":        {Flag: "-crf", Min: 10, Max: 63, Step: 1, Seed: 32, HigherIsBetter: ptr(false)},
	"librav1e":          {Flag: "-qp", Min: 20, Max: 255, Step: 1, Seed: 80, HigherIsBetter: ptr(false)},
	"h264_nvenc":        {Flag: "-cq:v", Min: 15, Max: 51, Step: 0.1, Seed: 28, HigherIsBetter: ptr(false)},
	"hevc_nvenc":        {Flag: "-cq:v", Min: 15, Max: 51, Step: 0.1, Seed: 28, HigherIsBetter: ptr(false)},
	"av1_nvenc":         {Flag: "-cq:v", Min: 15, Max: 51, Step: 0.1, Seed: 30, HigherIsBetter: ptr(false)},
	"h264_videotoolbox": {Flag: "-q:v", Min: 1, Max: 100, Step: 1, Seed: 60, HigherIsBetter: ptr(true)},
	"hevc_videotoolbox": {Flag: "-q:v", Min: 1, Max: 100, Step: 1, Seed: 60, HigherIsBetter: ptr(true)},
}

// DefaultQuality returns the quality flag, range, step, typical value
// and direction for an ffmpeg encoder such as "libx265".
func DefaultQuality(codec string) (Quality, bool) {
	q, ok := qualityDefaults[codec]
	return q, ok
}

// Encoder returns the ffmpeg video encoder named in the flagset's
// arguments, or "".
func (f *Flagset) Encoder() string {
	for i := 0; i+1 < len(f.Args); i++ {
		switch f.Args[i] {
		case "-c:v", "-codec:v", "-vcodec":
			return f.Args[i+1]
		}
	}
	return ""
}

func (q *Quality) fillFrom(d Quality) {
	if q.Flag == "" {
		q.Flag = d.Flag
	}
	if q.Min == 0 && q.Max == 0 {
		q.Min, q.Max = d.Min, d.Max
	}
	if q.Step == 0 {
		q.Step = d.Step
	}
	if q.Seed == 0 {
		q.Seed = d.Seed
	}
	if q.HigherIsBetter == nil {
		q.HigherIsBetter = d.HigherIsBetter
	}
}

// LoadConfig reads and validates a bench config, filling in defaults.
// Unknown keys are errors.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var c Config
	if err := yaml.Load(data, &c, yaml.WithKnownFields()); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := c.finish(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) finish(base string) error {
	if c.Samples == "" {
		return fmt.Errorf("samples: required")
	}
	c.Samples = resolve(base, c.Samples)
	if c.Workdir == "" {
		c.Workdir = "bench"
	}
	c.Workdir = resolve(base, c.Workdir)
	if c.TargetVMAF == 0 {
		c.TargetVMAF = 95
	}
	if c.MaxColorShift == 0 {
		c.MaxColorShift = c.OldMaxColorShift
	}
	if c.VMAFThreads == 0 {
		c.VMAFThreads = 4
	}
	if c.VMAFJobs == 0 {
		c.VMAFJobs = 4
	}
	if len(c.Flagsets) == 0 {
		return fmt.Errorf("flagsets: at least one required")
	}
	if c.HDR.TargetVMAF < 0 || c.HDR.TargetVMAF > 100 {
		return fmt.Errorf("hdr.target_vmaf: must be between 0 and 100")
	}
	if c.VMAFModel2160 != "" && c.VMAFModel == "" {
		return fmt.Errorf("vmaf_model_2160: needs vmaf_model (%s for the built-in model)", BuiltinVMAFModel)
	}
	if c.HDR.VMAFModel2160 != "" && c.HDR.VMAFModel == "" {
		return fmt.Errorf("hdr.vmaf_model_2160: needs hdr.vmaf_model (%s for the built-in model)", BuiltinVMAFModel)
	}
	for _, p := range []*string{&c.VMAFModel, &c.VMAFModel2160, &c.HDR.VMAFModel, &c.HDR.VMAFModel2160} {
		if *p != "" && *p != BuiltinVMAFModel {
			*p = resolve(base, *p)
		}
	}
	var err error
	if c.sdrModels, err = loadModelPair(c.VMAFModel, c.VMAFModel2160); err != nil {
		return err
	}
	for _, m := range []VMAFModel{c.sdrModels.base, c.sdrModels.uhd} {
		if m.HDR {
			return fmt.Errorf("vmaf_model: HDR model %s can't judge SDR samples; set hdr.vmaf_model instead", m.Name())
		}
	}
	c.hdrModels = c.sdrModels
	if c.HDR.VMAFModel != "" {
		if c.hdrModels, err = loadModelPair(c.HDR.VMAFModel, c.HDR.VMAFModel2160); err != nil {
			return err
		}
	}

	for _, name := range c.FlagsetNames() {
		f := c.Flagsets[name]
		if f == nil || len(f.Args) == 0 {
			return fmt.Errorf("flagset %q: args required", name)
		}
		if d, ok := qualityDefaults[f.Encoder()]; ok {
			f.Quality.fillFrom(d)
		}
		q := f.Quality
		switch {
		case q.Flag == "":
			return fmt.Errorf("flagset %q: encoder %q has no default quality flag; set quality.flag", name, f.Encoder())
		case q.Step <= 0:
			return fmt.Errorf("flagset %q: quality.step must be positive", name)
		case q.Max <= q.Min:
			return fmt.Errorf("flagset %q: quality.max must exceed quality.min", name)
		case q.HigherIsBetter == nil:
			f.Quality.HigherIsBetter = ptr(false)
		}
		if slices.Contains(f.Args, q.Flag) {
			return fmt.Errorf("flagset %q: args must not contain the quality flag %s", name, q.Flag)
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

// FlagsetNames returns the flagset names in sorted order.
func (c *Config) FlagsetNames() []string {
	names := make([]string, 0, len(c.Flagsets))
	for n := range c.Flagsets {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Steps is the number of steps between Min and Max.
func (q Quality) Steps() int { return q.steps() }

// IndexToQuality and QualityToIndex convert between step indexes (0 is
// the best quality) and the encoder's values; see Search.
func (q Quality) IndexToQuality(i int) float64 { return q.indexToQuality(i) }
func (q Quality) QualityToIndex(v float64) int { return q.qualityToIndex(v) }

// steps is the number of steps between Min and Max.
func (q Quality) steps() int {
	return int(math.Round((q.Max - q.Min) / q.Step))
}

// margin is how far m clears the pass criteria, with target as the
// VMAF target: >= 0 passes, and the value is the smallest of the
// per-criterion margins.
func (c *Config) margin(m Metrics, target float64) float64 {
	margin := m.VMAF - target
	if c.MinCIEDE2000 > 0 {
		margin = min(margin, m.CIEDE2000-c.MinCIEDE2000)
	}
	if c.MinChromaPSNR > 0 {
		margin = min(margin, min(m.PSNRCb, m.PSNRCr)-c.MinChromaPSNR)
	}
	if c.MaxColorShift > 0 {
		if math.IsNaN(m.ColorShiftP999) {
			return math.Inf(-1)
		}
		margin = min(margin, c.MaxColorShift-m.ColorShiftP999)
	}
	if c.MaxColorFlicker > 0 {
		if math.IsNaN(m.ColorFlickerP999) {
			return math.Inf(-1)
		}
		margin = min(margin, c.MaxColorFlicker-m.ColorFlickerP999)
	}
	return margin
}
