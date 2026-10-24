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
	VMAFThreads int                 `yaml:"vmaf_threads"`
	VMAFJobs    int                 `yaml:"vmaf_jobs"`
	Flagsets    map[string]*Flagset `yaml:"flagsets"`
}

// Flagset is one candidate set of encoder options, minus the quality
// setting, which the search supplies.
type Flagset struct {
	Args    []string `yaml:"args"`
	Quality Quality  `yaml:"quality"`
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

// margin is how far m clears the pass criteria: >= 0 passes, and the
// value is the smallest of the per-criterion margins.
func (c *Config) margin(m Metrics) float64 {
	margin := m.VMAF - c.TargetVMAF
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
