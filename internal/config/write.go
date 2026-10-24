package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"go.yaml.in/yaml/v4"
)

// RenditionUpdate sets values of one rendition in a sidecar.
type RenditionUpdate struct {
	Name    string
	Quality float64
	// Bitrate, if not "", is written too, in config syntax ("2.4M").
	Bitrate string
	// Comment is attached to the quality line.
	Comment string
}

// UpdateSidecar writes updates into the sidecar at path, keeping its
// other contents and comments. A missing sidecar is created; if sources
// is not nil and the sidecar has none, they are written too, so a video
// first given on the command line gets a complete sidecar.
func UpdateSidecar(path string, sources map[string]string, updates []RenditionUpdate) error {
	var doc yaml.Node
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := yaml.Load(data, &doc); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	if doc.Kind == 0 || len(doc.Content) == 0 || isNull(doc.Content[0]) {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: top level must be a mapping", path)
	}

	if sources != nil && mappingValue(root, "sources") == nil {
		m := child(root, "sources")
		labels := make([]string, 0, len(sources))
		for l := range sources {
			labels = append(labels, l)
		}
		slices.Sort(labels)
		for _, l := range labels {
			setScalar(m, l, sources[l], "!!str")
		}
	}
	renditions := child(root, "renditions")
	for _, u := range updates {
		r := child(renditions, u.Name)
		q := setScalar(r, "quality", formatQuality(u.Quality), "!!float")
		q.LineComment = u.Comment
		if u.Bitrate != "" {
			setScalar(r, "bitrate", u.Bitrate, "!!str")
		}
	}

	out, err := yaml.Dump(&doc, yaml.WithIndent(2))
	if err != nil {
		return err
	}
	tmp := path + ".partial"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func formatQuality(q float64) string { return fmt.Sprintf("%g", q) }

// child returns the mapping under key in m, creating it (or replacing a
// null or scalar there) if needed.
func child(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			if v := m.Content[i+1]; v.Kind == yaml.MappingNode {
				v.Style &^= yaml.FlowStyle
				return v
			}
			m.Content[i+1] = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			return m.Content[i+1]
		}
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

// setScalar sets key in mapping m to a scalar, returning the value node.
func setScalar(m *yaml.Node, key, value, tag string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			*v = yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value, HeadComment: v.HeadComment}
			return v
		}
	}
	v := &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}
