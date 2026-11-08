package config

import (
	"fmt"
	"strconv"

	"go.yaml.in/yaml/v4"
)

// expandEncoders finishes the encoders and renditions of merged, the
// merged layers, before it's decoded. It works on the YAML so that
// every value it adds is shown with where it came from:
//
//   - An encoder naming a preset starts from a copy of that preset,
//     with the encoder's own fields replacing the preset's.
//   - An encoder of a known codec (see DefaultQualityScale) gets that
//     codec's quality_flag and quality if it has none.
//   - Renditions of an encoder some layer deleted with null are
//     dropped too, unless a later layer defined the encoder again.
//   - Renditions without a quality take their encoder's.
//
// deleted holds the named-map entries the layers deleted, as recorded
// by mergeInto.
func expandEncoders(merged *yaml.Node, deleted map[string]bool) error {
	presets := mappingValue(merged, "presets")
	encoders := mappingValue(merged, "encoders")
	if encoders != nil && encoders.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(encoders.Content); i += 2 {
			name, enc := encoders.Content[i].Value, encoders.Content[i+1]
			if enc.Kind != yaml.MappingNode {
				continue
			}
			if p := mappingValue(enc, "preset"); p != nil && !isNull(p) && p.Value != "" {
				base := mappingValue(presets, p.Value)
				if base == nil || base.Kind != yaml.MappingNode {
					return fmt.Errorf("encoder %q: no preset %q", name, p.Value)
				}
				if mappingValue(base, "preset") != nil {
					return fmt.Errorf("preset %q: presets can't name a preset", p.Value)
				}
				fromPreset := copyNode(base)
				relabel(fromPreset, "preset "+p.Value)
				enc = overlay(fromPreset, enc)
				encoders.Content[i+1] = enc
			}
			codec := mappingValue(enc, "codec")
			if codec == nil {
				continue
			}
			s, ok := DefaultQualityScale(codec.Value)
			if !ok {
				continue
			}
			from := "default for " + codec.Value
			if mappingValue(enc, "quality_flag") == nil {
				setScalar(enc, "quality_flag", s.Flag, "!!str").LineComment = from
			}
			if mappingValue(enc, "quality") == nil {
				setScalar(enc, "quality", formatQuality(s.Default), "!!float").LineComment = from
			}
		}
	}

	renditions := mappingValue(merged, "renditions")
	if renditions == nil || renditions.Kind != yaml.MappingNode {
		return nil
	}
	kept := renditions.Content[:0]
	for i := 0; i+1 < len(renditions.Content); i += 2 {
		name, r := renditions.Content[i].Value, renditions.Content[i+1]
		e := mappingValue(r, "encoder")
		if e == nil {
			kept = append(kept, renditions.Content[i], r)
			continue
		}
		enc := mappingValue(encoders, e.Value)
		if enc == nil && deleted["encoders."+e.Value] {
			continue
		}
		kept = append(kept, renditions.Content[i], r)
		if enc == nil || mappingValue(r, "quality") != nil {
			continue
		}
		q := mappingValue(enc, "quality")
		if q == nil || isNull(q) {
			return fmt.Errorf("rendition %q: no quality, and encoder %q has none", name, e.Value)
		}
		if _, err := strconv.ParseFloat(q.Value, 64); err != nil {
			return fmt.Errorf("encoder %q: bad quality %q", e.Value, q.Value)
		}
		setScalar(r, "quality", q.Value, "!!float").LineComment = "encoder " + e.Value + "'s"
	}
	renditions.Content = kept
	return nil
}

// overlay returns base with every key of over set to over's value:
// the preset key first, then base's keys in order, then over's new
// ones.
func overlay(base, over *yaml.Node) *yaml.Node {
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if p := mappingValue(over, "preset"); p != nil {
		out.Content = append(out.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "preset"}, p)
	}
	for i := 0; i+1 < len(base.Content); i += 2 {
		k := base.Content[i]
		v := base.Content[i+1]
		if o := mappingValue(over, k.Value); o != nil {
			v = o
		}
		out.Content = append(out.Content, k, v)
	}
	for i := 0; i+1 < len(over.Content); i += 2 {
		k := over.Content[i]
		if k.Value != "preset" && mappingValue(base, k.Value) == nil {
			out.Content = append(out.Content, k, over.Content[i+1])
		}
	}
	return out
}

// copyNode deep-copies n, keeping its comments.
func copyNode(n *yaml.Node) *yaml.Node {
	c := *n
	c.Content = nil
	for _, child := range n.Content {
		c.Content = append(c.Content, copyNode(child))
	}
	return &c
}

// relabel adds what to the provenance comment of each value in n, as
// annotate set them: "default" becomes "default, preset x265-faster".
func relabel(n *yaml.Node, what string) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			relabel(n.Content[i], what)
		}
	default:
		if n.LineComment != "" {
			n.LineComment += ", " + what
		} else {
			n.LineComment = what
		}
	}
}
