package bench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// VMAFModel is what VMAF is scored with. The zero value is the
// built-in v0.6.1 model, its 4K variant for 2160p and up.
type VMAFModel struct {
	// Path is a libvmaf JSON model file.
	Path string
	// HDR is set for models that score PQ pictures in ICtCp, like
	// Netflix's hdr_vmaf_v1 models. They're given HDR on both sides,
	// converted to what the model's conversion_target asks for, instead
	// of SDR tone maps.
	HDR bool
	// Display is the display height the model's conversion_target
	// scales both pictures to (1080 or 2160), or 0 to score them at the
	// reference's size.
	Display uint64
	// id names the model and its contents, for cache keys.
	id string
	// stripped is the model without its conversion_target, which is
	// what libvmaf is given; see LoadVMAFModel. Nil for models without
	// one.
	stripped []byte
}

// LoadVMAFModel reads the libvmaf JSON model at path.
//
// A model's conversion_target is carried out by the scoring filtergraph
// (see hdrConversion), and libvmaf is given a copy without it. libvmaf
// 3.2 ignores it, while newer versions convert and scale the pictures
// themselves, but only when built with zimg. Doing it in ffmpeg scores
// the same with either.
func LoadVMAFModel(path string) (VMAFModel, error) {
	// libvmaf's model option separates fields with ':' and models with
	// '|', and the filtergraph quotes it with '.
	if strings.ContainsAny(path, "|'") {
		return VMAFModel{}, fmt.Errorf("VMAF model %s: path can't contain | or '", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return VMAFModel{}, fmt.Errorf("VMAF model: %w", err)
	}
	var m struct {
		ModelDict *struct {
			ConversionTarget *struct {
				Colorspace struct {
					TRC string `json:"trc"`
				} `json:"colorspace"`
				Resolution struct {
					Display uint64 `json:"display"`
				} `json:"resolution"`
			} `json:"conversion_target"`
		} `json:"model_dict"`
	}
	if err := json.Unmarshal(data, &m); err != nil || m.ModelDict == nil {
		return VMAFModel{}, fmt.Errorf("VMAF model %s: not a libvmaf JSON model", path)
	}
	sum := sha256.Sum256(data)
	model := VMAFModel{Path: path, id: "model=" + hex.EncodeToString(sum[:8])}
	ct := m.ModelDict.ConversionTarget
	if ct == nil {
		return model, nil
	}
	if ct.Colorspace.TRC != "smpte2084" {
		return VMAFModel{}, fmt.Errorf("VMAF model %s: can't convert to transfer %q", path, ct.Colorspace.TRC)
	}
	switch d := ct.Resolution.Display; d {
	case 0, 1080, 2160:
		model.Display = d
		// Scores at the display size differ from earlier ones taken at
		// the reference's size, so they mustn't share cached trials.
		model.id += fmt.Sprintf("+display=%d", d)
	default:
		return VMAFModel{}, fmt.Errorf("VMAF model %s: unknown display resolution %d", path, d)
	}
	model.HDR = true
	if model.stripped, err = stripConversionTarget(data); err != nil {
		return VMAFModel{}, fmt.Errorf("VMAF model %s: %w", path, err)
	}
	return model, nil
}

// stripConversionTarget returns the model JSON data without
// model_dict.conversion_target.
func stripConversionTarget(data []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, err
	}
	var dict map[string]json.RawMessage
	if err := json.Unmarshal(top["model_dict"], &dict); err != nil {
		return nil, err
	}
	delete(dict, "conversion_target")
	d, err := json.Marshal(dict)
	if err != nil {
		return nil, err
	}
	top["model_dict"] = d
	return json.Marshal(top)
}

// Name is the model's file name without .json, or "" for the built-in
// model.
func (m VMAFModel) Name() string {
	if m.Path == "" {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(m.Path), ".json")
}

// ID identifies the model's contents, or is "" for the built-in model.
func (m VMAFModel) ID() string { return m.id }

// ScorerAt identifies the model that scores a rendition height pixels
// tall: the built-in model switches to its 4K variant at 2160p.
func (m VMAFModel) ScorerAt(height uint64) string {
	if m.Path == "" {
		return vmafModel(height)
	}
	return m.id
}

// option returns libvmaf's model option for scoring at height: the
// model as "vmaf", and for the built-in model its no-enhancement-gain
// variant as "vmaf_neg".
func (m VMAFModel) option(height uint64) string {
	if m.Path == "" {
		v := vmafModel(height)
		return fmt.Sprintf("version=%s\\:name=vmaf|version=%sneg\\:name=vmaf_neg", v, v)
	}
	return "path=" + strings.ReplaceAll(m.Path, ":", `\:`) + `\:name=vmaf`
}

// hdrConversion converts width x height PQ or HLG pictures (with the
// given ffprobe transfer name) to what libvmaf's HDR models are trained
// on: PQ, BT.2020, ICtCp, 4:2:0 at 16 bits, scaled to the model's
// display height as newer libvmaf does (see displaySize). One zscale
// does both, as libvmaf's single zimg graph does. zscale resamples
// chroma with the same filter as luma, where libvmaf uses bilinear.
func hdrConversion(transfer string, width, height, display uint64) (string, error) {
	switch transfer {
	case "smpte2084", "arib-std-b67":
	default:
		return "", fmt.Errorf("an HDR VMAF model needs an HDR reference, not transfer %q", transfer)
	}
	resize := ""
	if w, h := displaySize(width, height, display); w != width || h != height {
		// libvmaf's choice: bicubic to enlarge, lanczos to shrink.
		f := "lanczos"
		if w*h > width*height {
			f = "bicubic"
		}
		resize = fmt.Sprintf("w=%d:h=%d:f=%s:", w, h, f)
	}
	return "zscale=" + resize + "tin=" + transfer + ":pin=bt2020:min=bt2020nc:rin=limited" +
		":t=smpte2084:p=bt2020:m=ictcp:r=limited:npl=1000,format=yuv420p16le", nil
}

// displaySize is the size libvmaf scales a width x height picture to
// for a model's display height: the short side at display, the long
// side at the same aspect ratio rounded to the nearest even number. A
// display of 0, or a picture already that size, keeps its size.
func displaySize(width, height, display uint64) (uint64, uint64) {
	short, long := height, width
	portrait := height > width
	if portrait {
		short, long = width, height
	}
	if display == 0 || short == 0 || short == display {
		return width, height
	}
	scaled := (long*display + short) / (2 * short) * 2
	if portrait {
		return display, scaled
	}
	return scaled, display
}
