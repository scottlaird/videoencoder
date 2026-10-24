package config

import (
	"go.yaml.in/yaml/v4"
)

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// mappingValue returns the value for key in mapping node m, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// withoutKey returns a shallow copy of mapping m without key.
func withoutKey(m *yaml.Node, key string) *yaml.Node {
	c := *m
	c.Content = nil
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			c.Content = append(c.Content, m.Content[i], m.Content[i+1])
		}
	}
	return &c
}

// mergeInto merges mapping src into mapping dst: mappings merge
// recursively, other values replace, and null deletes. Every value
// taken from src is annotated with label as a line comment.
func mergeInto(dst, src *yaml.Node, label string) {
	for i := 0; i+1 < len(src.Content); i += 2 {
		key, val := src.Content[i], src.Content[i+1]
		at := -1
		for j := 0; j+1 < len(dst.Content); j += 2 {
			if dst.Content[j].Value == key.Value {
				at = j
				break
			}
		}

		switch {
		case isNull(val):
			if at >= 0 {
				dst.Content = append(dst.Content[:at], dst.Content[at+2:]...)
			}
		case at >= 0 && val.Kind == yaml.MappingNode && dst.Content[at+1].Kind == yaml.MappingNode:
			mergeInto(dst.Content[at+1], val, label)
		default:
			k := &yaml.Node{Kind: yaml.ScalarNode, Tag: key.Tag, Value: key.Value}
			v := annotate(clean(val), label)
			if at >= 0 {
				dst.Content[at], dst.Content[at+1] = k, v
			} else {
				dst.Content = append(dst.Content, k, v)
			}
		}
	}
}

// clean deep-copies n without its comments or style, so merged output
// shows only provenance.
func clean(n *yaml.Node) *yaml.Node {
	c := &yaml.Node{Kind: n.Kind, Tag: n.Tag, Value: n.Value}
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		return clean(n.Alias)
	}
	for _, child := range n.Content {
		c.Content = append(c.Content, clean(child))
	}
	return c
}

// annotate labels the leaves of n with where they came from. Scalars
// and sequences carry the label; mappings pass it to their values.
func annotate(n *yaml.Node, label string) *yaml.Node {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			annotate(n.Content[i], label)
		}
	case yaml.SequenceNode:
		n.Style = yaml.FlowStyle
		n.LineComment = label
	default:
		n.LineComment = label
	}
	return n
}
