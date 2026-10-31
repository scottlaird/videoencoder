// Package config loads videoencoder's layered YAML configuration.
//
// A video's settings are merged from, lowest priority first: the
// built-in defaults (default.yaml), the user's global config, every
// videoencoder.yaml from the filesystem root down to the video's
// directory, the video's entry under "videos:" in those files, its
// "<name>.video.yaml" sidecar, and finally command-line overrides.
//
// Every layer has the same schema. Maps merge key by key, scalars and
// lists replace, and an explicit null deletes an inherited key; that is
// why renditions and encoders are maps keyed by name, so one layer can
// adjust or drop a single rendition.
package config

import (
	_ "embed"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:embed default.yaml
var defaultYAML []byte

// Config is one video's fully merged settings.
type Config struct {
	// Sources maps a label to a source file. Sources are encoded in
	// label order, and the first supplies the audio and poster, so
	// "hdr" before "sdr" works as expected.
	Sources    map[string]string     `yaml:"sources"`
	Outdir     string                `yaml:"outdir"`
	Streamdir  string                `yaml:"streamdir"`
	Thumbnails Thumbnails            `yaml:"thumbnails"`
	Poster     Poster                `yaml:"poster"`
	Publish    Publish               `yaml:"publish"`
	Tune       Tune                  `yaml:"tune"`
	Encoders   map[string]*Encoder   `yaml:"encoders"`
	Renditions map[string]*Rendition `yaml:"renditions"`
}

// layer is the schema of a config file: a Config that may also define
// per-video layers. The video entries are checked when they're used.
type layer struct {
	Config `yaml:",inline"`
	Videos map[string]any `yaml:"videos"`
}

// Tune says how "videoencoder tune" picks rendition qualities.
type Tune struct {
	TargetVMAF float64 `yaml:"target_vmaf"`
	Start      string  `yaml:"start"`
	Duration   string  `yaml:"duration"`
	Anchors    int     `yaml:"anchors"`
}

// Thumbnails says how to make seek thumbnails.
type Thumbnails struct {
	Enabled  bool    `yaml:"enabled"`
	Interval float64 `yaml:"interval"` // seconds between thumbnails
	Height   uint64  `yaml:"height"`
	Columns  uint64  `yaml:"columns"`
	Rows     uint64  `yaml:"rows"`
}

// Publish says where finished streams go.
type Publish struct {
	// To names the targets published to after each render.
	To      []string                  `yaml:"to"`
	Targets map[string]*PublishTarget `yaml:"targets"`
}

// PublishTarget is a local directory or an S3 bucket; exactly one is set.
type PublishTarget struct {
	Dir string    `yaml:"dir"`
	S3  *S3Target `yaml:"s3"`
	// PerVideo is set when the dir or prefix contains {name}, so the
	// location holds only this video and pruning it is safe.
	PerVideo bool `yaml:"-"`
}

// S3Target is an S3-compatible bucket.
type S3Target struct {
	Endpoint string `yaml:"endpoint"`
	Bucket   string `yaml:"bucket"`
	Prefix   string `yaml:"prefix"`
	Region   string `yaml:"region"`
	Profile  string `yaml:"profile"`
}

// Poster says how to make the video's still images.
type Poster struct {
	Offset         string   `yaml:"offset"`
	Widths         []uint64 `yaml:"widths"`
	Formats        []string `yaml:"formats"`
	GainMapFormats []string `yaml:"gainmap_formats"`
	// HDR adds plain PQ AVIF posters for HDR sources.
	HDR bool `yaml:"hdr"`
}

// PosterFormats are the image formats posters can be written in, and
// GainMapFormats those that can carry a gain map.
var (
	PosterFormats  = []string{"avif", "jpg", "webp"}
	GainMapFormats = []string{"avif", "jpg"}
)

// Encoder is an ffmpeg video encoder and its fixed options.
type Encoder struct {
	Codec       string   `yaml:"codec"`
	QualityFlag string   `yaml:"quality_flag"`
	Args        []string `yaml:"args"`
	// CapBitrate passes each rendition's bitrate as -b:v, with
	// -maxrate and -bufsize at 1.5x. Without it the bitrate is only a
	// label.
	CapBitrate bool `yaml:"cap_bitrate"`
	// SDROnly means the encoder only makes SDR renditions: from SDR
	// sources, and from an HDR source (tone-mapped) only if the video
	// has no SDR source.
	SDROnly bool `yaml:"sdr_only"`
}

// Rendition is one output of the ladder.
type Rendition struct {
	Encoder string  `yaml:"encoder"`
	Height  uint64  `yaml:"height"`
	Quality float64 `yaml:"quality"`
	// Bitrate is in bits per second, with an optional k or M suffix.
	Bitrate string `yaml:"bitrate"`
	// Args are extra options for this rendition only.
	Args []string `yaml:"args"`
	// TargetVMAF overrides tune.target_vmaf for this rendition.
	TargetVMAF float64 `yaml:"target_vmaf"`
	// Tune false leaves the rendition out of "videoencoder tune": it
	// isn't searched or interpolated, and its quality and bitrate stay
	// as configured. For fixed rungs, like a bitrate-capped one for
	// slow networks.
	Tune *bool `yaml:"tune"`
}

// Tuned reports whether "videoencoder tune" sets r's quality: unless
// it says tune: false. A rendition with no config of its own is tuned.
func (r *Rendition) Tuned() bool {
	return r == nil || r.Tune == nil || *r.Tune
}

// SourceLabels returns the source labels in encoding order.
func (c *Config) SourceLabels() []string {
	labels := make([]string, 0, len(c.Sources))
	for l := range c.Sources {
		labels = append(labels, l)
	}
	slices.Sort(labels)
	return labels
}

// RenditionNames returns rendition names ordered by encoder, then
// height, then name.
func (c *Config) RenditionNames() []string {
	names := make([]string, 0, len(c.Renditions))
	for n := range c.Renditions {
		names = append(names, n)
	}
	slices.SortFunc(names, func(a, b string) int {
		ra, rb := c.Renditions[a], c.Renditions[b]
		if r := strings.Compare(ra.Encoder, rb.Encoder); r != 0 {
			return r
		}
		if ra.Height != rb.Height {
			if ra.Height < rb.Height {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	return names
}

// Validate checks that the merged config is complete and consistent.
func (c *Config) Validate() error {
	if len(c.Sources) == 0 {
		return fmt.Errorf("no sources")
	}
	for label, src := range c.Sources {
		if src == "" {
			return fmt.Errorf("source %q: empty path", label)
		}
	}
	if c.Outdir == "" || c.Streamdir == "" {
		return fmt.Errorf("outdir and streamdir are required")
	}
	for name, e := range c.Encoders {
		if e == nil || e.Codec == "" || e.QualityFlag == "" {
			return fmt.Errorf("encoder %q: codec and quality_flag are required", name)
		}
	}
	if t := c.Thumbnails; t.Enabled && (t.Interval <= 0 || t.Height == 0 || t.Columns == 0 || t.Rows == 0) {
		return fmt.Errorf("thumbnails: interval, height, columns and rows must be positive")
	}
	if _, err := ParseOffset(c.Poster.Offset); err != nil {
		return fmt.Errorf("poster.offset: %w", err)
	}
	if len(c.Poster.Widths) == 0 || len(c.Poster.Formats) == 0 {
		return fmt.Errorf("poster: widths and formats are required")
	}
	for _, f := range c.Poster.Formats {
		if !slices.Contains(PosterFormats, f) {
			return fmt.Errorf("poster: unknown format %q (want one of %v)", f, PosterFormats)
		}
	}
	for _, f := range c.Poster.GainMapFormats {
		if !slices.Contains(GainMapFormats, f) {
			return fmt.Errorf("poster: gainmap_formats can't include %q (want some of %v)", f, GainMapFormats)
		}
	}
	if t := c.Tune; t.TargetVMAF <= 0 || t.TargetVMAF > 100 || t.Anchors < 2 {
		return fmt.Errorf("tune: target_vmaf must be in (0, 100] and anchors at least 2")
	}
	for _, f := range []struct{ name, v string }{{"tune.start", c.Tune.Start}, {"tune.duration", c.Tune.Duration}} {
		if _, err := ParseOffset(f.v); err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
	}
	for name, t := range c.Publish.Targets {
		if t == nil || (t.Dir == "") == (t.S3 == nil) {
			return fmt.Errorf("publish target %q: set exactly one of dir and s3", name)
		}
		if t.S3 != nil && t.S3.Bucket == "" {
			return fmt.Errorf("publish target %q: s3.bucket is required", name)
		}
	}
	for _, name := range c.Publish.To {
		if _, ok := c.Publish.Targets[name]; !ok {
			return fmt.Errorf("publish.to: no target named %q", name)
		}
	}
	if len(c.Renditions) == 0 {
		return fmt.Errorf("no renditions")
	}
	for name, r := range c.Renditions {
		if r == nil {
			return fmt.Errorf("rendition %q is empty", name)
		}
		if _, ok := c.Encoders[r.Encoder]; !ok {
			return fmt.Errorf("rendition %q: unknown encoder %q", name, r.Encoder)
		}
		if r.Height == 0 {
			return fmt.Errorf("rendition %q: height is required", name)
		}
		if _, err := ParseBitrate(r.Bitrate); err != nil {
			return fmt.Errorf("rendition %q: %w", name, err)
		}
	}
	return nil
}

// ParseOffset parses a time into the video: seconds ("72.5"), seconds
// with a unit ("1.5s"), or "[HH:]MM:SS[.ms]".
func ParseOffset(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return d, nil
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("bad time %q", s)
	}
	var total float64
	for i, p := range parts {
		f, err := strconv.ParseFloat(p, 64)
		if err != nil || f < 0 || (i > 0 && f >= 60) {
			return 0, fmt.Errorf("bad time %q", s)
		}
		total = total*60 + f
	}
	return time.Duration(total * float64(time.Second)), nil
}

// ParseBitrate parses "4M", "800k" or "1500000" as bits per second. An
// empty string is 0.
func ParseBitrate(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "M"):
		mult, s = 1e6, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "k"):
		mult, s = 1e3, strings.TrimSuffix(s, "k")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("bad bitrate %q", s)
	}
	return uint64(f*mult + 0.5), nil
}
