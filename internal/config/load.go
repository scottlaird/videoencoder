package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"
)

const (
	// ProjectFile is the per-directory config file name.
	ProjectFile = "videoencoder.yaml"
	// SidecarSuffix ends a per-video config file name: "foo.video.yaml"
	// configures the video named "foo".
	SidecarSuffix = ".video.yaml"
)

// Video identifies one video to encode: a name in a directory.
type Video struct {
	Name string
	// Dir is the absolute directory the video lives in. Relative paths
	// in its config are relative to it.
	Dir string
}

// Loader merges the config layers for videos.
type Loader struct {
	// GlobalPath is the user's config file; a missing file is fine.
	GlobalPath string
	// Overrides is the highest-priority layer, from the command line.
	Overrides map[string]any
}

// DefaultGlobalPath is $XDG_CONFIG_HOME/videoencoder/config.yaml, or
// ~/.config/videoencoder/config.yaml.
func DefaultGlobalPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "videoencoder", "config.yaml")
}

// Resolved is a video's merged config, with paths made absolute and
// {name} expanded.
type Resolved struct {
	Video
	Config
	// Merged is the merged YAML with each value annotated with the file
	// it came from, for display.
	Merged *yaml.Node
}

// source is one config layer.
type source struct {
	label string // shown in provenance comments
	node  *yaml.Node
}

// Load merges every layer that applies to v.
func (l *Loader) Load(v Video) (*Resolved, error) {
	layers, err := l.layers(v)
	if err != nil {
		return nil, err
	}

	merged := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, s := range layers {
		mergeInto(merged, s.node, s.label, "")
	}

	r := &Resolved{Video: v, Merged: merged}
	if err := merged.Load(&r.Config, yaml.WithKnownFields()); err != nil {
		return nil, fmt.Errorf("video %s: %w", v.Name, err)
	}
	r.resolvePaths()
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("video %s in %s: %w", v.Name, v.Dir, err)
	}
	if err := r.resolveQualities(); err != nil {
		return nil, fmt.Errorf("video %s in %s: %w", v.Name, v.Dir, err)
	}
	return r, nil
}

// resolveQualities sets the quality of each rendition with
// quality_from, following chains, and shows the result in Merged.
func (r *Resolved) resolveQualities() error {
	done := map[string]bool{}
	var resolve func(name string, path []string) error
	resolve = func(name string, path []string) error {
		rd := r.Renditions[name]
		if done[name] || rd.QualityFrom == "" {
			return nil
		}
		path = append(path, name)
		if slices.Contains(path[:len(path)-1], name) {
			return fmt.Errorf("rendition %q: quality_from loops: %s", name, strings.Join(path, " -> "))
		}
		from, ok := r.Renditions[rd.QualityFrom]
		if !ok {
			return fmt.Errorf("rendition %q: quality_from: no rendition %q", name, rd.QualityFrom)
		}
		if from.Encoder != rd.Encoder {
			return fmt.Errorf("rendition %q: quality_from %q uses encoder %q, not %q", name, rd.QualityFrom, from.Encoder, rd.Encoder)
		}
		if err := resolve(rd.QualityFrom, path); err != nil {
			return err
		}
		rd.Quality = from.Quality + rd.QualityOffset
		done[name] = true
		m := child(child(r.Merged, "renditions"), name)
		setScalar(m, "quality", formatQuality(rd.Quality), "!!float").LineComment = rd.Follows()
		return nil
	}
	for _, name := range r.RenditionNames() {
		if err := resolve(name, nil); err != nil {
			return err
		}
	}
	return nil
}

func (l *Loader) layers(v Video) ([]source, error) {
	def, err := parseLayer(defaultYAML, "default")
	if err != nil {
		return nil, fmt.Errorf("built-in defaults: %w", err)
	}
	layers := []source{{"default", def}}

	if l.GlobalPath != "" {
		n, err := loadLayerFile(l.GlobalPath)
		if err != nil {
			return nil, err
		}
		if n != nil {
			layers = append(layers, source{l.GlobalPath, withoutKey(n, "videos")})
		}
	}

	var entry *yaml.Node
	for _, dir := range ancestors(v.Dir) {
		path := filepath.Join(dir, ProjectFile)
		n, err := loadLayerFile(path)
		if err != nil {
			return nil, err
		}
		if n == nil {
			continue
		}
		layers = append(layers, source{path, withoutKey(n, "videos")})
		if dir == v.Dir {
			if entry, err = videoEntry(n, v.Name, path); err != nil {
				return nil, err
			}
		}
	}
	if entry != nil {
		layers = append(layers, source{filepath.Join(v.Dir, ProjectFile) + " videos." + v.Name, entry})
	}

	sidecar := filepath.Join(v.Dir, v.Name+SidecarSuffix)
	n, err := loadLayerFile(sidecar)
	if err != nil {
		return nil, err
	}
	if n != nil {
		if mappingValue(n, "videos") != nil {
			return nil, fmt.Errorf("%s: a sidecar can't define videos", sidecar)
		}
		layers = append(layers, source{sidecar, n})
	}

	if len(l.Overrides) > 0 {
		var o yaml.Node
		if err := o.Encode(l.Overrides); err != nil {
			return nil, fmt.Errorf("command-line overrides: %w", err)
		}
		layers = append(layers, source{"command line", &o})
	}
	return layers, nil
}

// videoEntry returns the "videos.<name>" layer of a project file, or nil.
func videoEntry(project *yaml.Node, name, path string) (*yaml.Node, error) {
	videos := mappingValue(project, "videos")
	if videos == nil {
		return nil, nil
	}
	e := mappingValue(videos, name)
	if e == nil || isNull(e) {
		return nil, nil
	}
	if e.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: videos.%s must be a mapping", path, name)
	}
	var c Config
	if err := e.Load(&c, yaml.WithKnownFields()); err != nil {
		return nil, fmt.Errorf("%s: videos.%s: %w", path, name, err)
	}
	return e, nil
}

// loadLayerFile parses and checks one config file. A missing or empty
// file is (nil, nil).
func loadLayerFile(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseLayer(data, path)
}

// parseLayer parses data as a config layer, rejecting unknown keys.
func parseLayer(data []byte, label string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Load(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 || isNull(doc.Content[0]) {
		return nil, nil
	}
	n := doc.Content[0]
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: top level must be a mapping", label)
	}
	var l layer
	if err := n.Load(&l, yaml.WithKnownFields()); err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	return n, nil
}

// ancestors returns dir and its parents, root first.
func ancestors(dir string) []string {
	var dirs []string
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	slices.Reverse(dirs)
	return dirs
}

func (r *Resolved) resolvePaths() {
	abs := func(p string) string {
		p = strings.ReplaceAll(p, "{name}", r.Name)
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(r.Dir, p)
	}
	r.Outdir = abs(r.Outdir)
	r.Streamdir = abs(r.Streamdir)
	for label, src := range r.Sources {
		r.Sources[label] = abs(src)
	}
	for _, t := range r.Publish.Targets {
		if t == nil {
			continue
		}
		if t.Dir != "" {
			t.PerVideo = strings.Contains(t.Dir, "{name}")
			t.Dir = abs(t.Dir)
		}
		if t.S3 != nil {
			t.PerVideo = strings.Contains(t.S3.Prefix, "{name}")
			t.S3.Prefix = strings.ReplaceAll(t.S3.Prefix, "{name}", r.Name)
			if t.S3.Prefix != "" && !strings.HasSuffix(t.S3.Prefix, "/") {
				t.S3.Prefix += "/"
			}
		}
	}
}
