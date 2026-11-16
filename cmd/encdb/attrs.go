package main

import (
	"regexp"
	"slices"
	"strings"
)

// Software encoders, by ffmpeg encoder name: the family they're shown
// as. Anything else is a hardware encoder.
var softwareFamilies = map[string]string{
	"libx264": "x264", "libx265": "x265", "libsvtav1": "SVT-AV1", "libaom-av1": "libaom",
	"mpeg2video": "mpeg2video", "mpeg4": "mpeg4", "h263p": "h263p",
}

// Hardware encoder families, by the ffmpeg encoder name's suffix.
var hardwareFamilies = map[string]string{"nvenc": "NVENC", "qsv": "Quick Sync", "videotoolbox": "VideoToolbox"}

// H.264 encoders, which encbench always runs 8-bit.
var h264Encoders = []string{"libx264", "h264_nvenc", "h264_qsv", "h264_videotoolbox"}

var (
	presetRE    = regexp.MustCompile(`^(ultrafast|superfast|veryfast|faster|fast|medium|slow|slower|veryslow|p[0-9]+|cpu[0-9]+)$`)
	keyframesRE = regexp.MustCompile(`^(kf[0-9]+|g[0-9]+)$`)
	pixFmtRE    = regexp.MustCompile(`-pix_fmt (\S+)|format=(\w+)`)
	tenBitRE    = regexp.MustCompile(`p10|p010`)
	clipRE      = regexp.MustCompile(`^(?:[0-9]{8}-)?([a-z0-9]+)`)
	av1RE       = regexp.MustCompile(`^(av1_.*|libsvtav1|libaom-av1|librav1e)$`)
	hevcRE      = regexp.MustCompile(`^(hevc_.*|libx265)$`)
	h264RE      = regexp.MustCompile(`^(h264_.*|libx264)$`)
	res1080RE   = regexp.MustCompile(`[-@]1080p`)
	extRE       = regexp.MustCompile(`\.[^.]*$`)
	scaledExtRE = regexp.MustCompile(`(@[^.]*)?\.[^.]*$`)
)

// Attrs are the filterable facts about a flagset.
type Attrs struct {
	Family   string
	Software bool
	Preset   string // empty if the flagset names none
	// BitDepth is the depth asked for, "8" or "10", or "as source".
	BitDepth  string
	Keyframes string // such as "kf10", or "default"
	// Variant is whatever else the flagset's name says, or "default".
	// NVENC runs uncapped (-maxrate 250M) as standard, so "nocap" is
	// its default and "capped" its absence.
	Variant string
}

// attributes reads Attrs from a flagset's encoder, name and ffmpeg
// args: h264-qsv-medium-kf10-bpyr is Quick Sync, medium, 8-bit, kf10,
// variant bpyr.
func attributes(encoder, flagset, args string) Attrs {
	var a Attrs
	prefix := 1 // x265-medium-...
	if f, ok := softwareFamilies[encoder]; ok {
		a.Family, a.Software = f, true
	} else {
		suffix := encoder[strings.LastIndexByte(encoder, '_')+1:]
		a.Family = encoder
		if f, ok := hardwareFamilies[suffix]; ok {
			a.Family = f
		}
		prefix = 2 // hevc-qsv-medium-...
	}
	tokens := strings.Split(flagset, "-")
	tokens = tokens[min(prefix, len(tokens)):]
	if len(tokens) > 0 && presetRE.MatchString(tokens[0]) {
		a.Preset, tokens = tokens[0], tokens[1:]
	}
	a.Keyframes = "default"
	if i := slices.IndexFunc(tokens, keyframesRE.MatchString); i >= 0 {
		a.Keyframes = tokens[i]
	}
	// The pixel format asked for: -pix_fmt, or a hardware scaler's
	// format=. Without one the encode follows the source, except H.264.
	switch m := pixFmtRE.FindStringSubmatch(args); {
	case m != nil && tenBitRE.MatchString(m[1]+m[2]):
		a.BitDepth = "10"
	case m != nil, slices.Contains(h264Encoders, encoder):
		a.BitDepth = "8"
	default:
		a.BitDepth = "as source"
	}
	tokens = slices.DeleteFunc(tokens, func(t string) bool { return t == a.Keyframes || t == "10bit" })
	if a.Family == "NVENC" {
		if slices.Contains(tokens, "nocap") {
			tokens = slices.DeleteFunc(tokens, func(t string) bool { return t == "nocap" })
		} else {
			tokens = append(tokens, "capped")
		}
	}
	a.Variant = strings.Join(tokens, "-")
	if a.Variant == "" {
		a.Variant = "default"
	}
	return a
}

// codecOf is the codec encoded: ffprobe's out_codec, which only newer
// rows have, else what the encoder makes.
func codecOf(encoder, outCodec string) string {
	switch {
	case outCodec != "":
		return outCodec
	case av1RE.MatchString(encoder):
		return "av1"
	case hevcRE.MatchString(encoder):
		return "hevc"
	case h264RE.MatchString(encoder):
		return "h264"
	}
	return encoder
}

// clipOf is the clip a sample is of: "20180529-band@1080p.mov" is band.
func clipOf(sample string) string {
	if m := clipRE.FindStringSubmatch(sample); m != nil {
		return m[1]
	}
	return ""
}

// resOf is a sample's resolution from its name, or from height (the
// tallest encode of it logged) if the name doesn't say; empty if
// neither does.
func resOf(sample string, height int64) string {
	switch {
	case res1080RE.MatchString(sample):
		return "1080p"
	case strings.Contains(sample, "@2160p"):
		return "4k"
	case strings.Contains(sample, "@4320p"):
		return "8k"
	}
	return map[int64]string{1080: "1080p", 2160: "4k", 4320: "8k"}[height]
}

// stems are the probe keys for a sample: the camera file it came from
// and the file encoded ("20180529-band" and "20180529-band@1080p").
func stems(sample string) (source, input string) {
	return scaledExtRE.ReplaceAllString(sample, ""), extRE.ReplaceAllString(sample, "")
}

// isTenBit says if an ffmpeg pixel format is 10-bit.
func isTenBit(pixFmt string) bool { return tenBitRE.MatchString(pixFmt) }
