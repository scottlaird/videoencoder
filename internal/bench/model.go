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
	// id names the model and its contents, for cache keys.
	id string
}

// LoadVMAFModel reads the libvmaf JSON model at path.
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
			} `json:"conversion_target"`
		} `json:"model_dict"`
	}
	if err := json.Unmarshal(data, &m); err != nil || m.ModelDict == nil {
		return VMAFModel{}, fmt.Errorf("VMAF model %s: not a libvmaf JSON model", path)
	}
	hdr := false
	if ct := m.ModelDict.ConversionTarget; ct != nil {
		if ct.Colorspace.TRC != "smpte2084" {
			return VMAFModel{}, fmt.Errorf("VMAF model %s: can't convert to transfer %q", path, ct.Colorspace.TRC)
		}
		hdr = true
	}
	sum := sha256.Sum256(data)
	return VMAFModel{Path: path, HDR: hdr, id: "model=" + hex.EncodeToString(sum[:8])}, nil
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

// hdrConversion converts PQ or HLG pictures (with the given ffprobe
// transfer name) to what libvmaf's HDR models are trained on: PQ,
// BT.2020, ICtCp, 4:2:0 at 16 bits. Newer libvmaf does this itself from
// the model's conversion_target; 3.2 ignores it.
func hdrConversion(transfer string) (string, error) {
	switch transfer {
	case "smpte2084", "arib-std-b67":
	default:
		return "", fmt.Errorf("an HDR VMAF model needs an HDR reference, not transfer %q", transfer)
	}
	return "zscale=tin=" + transfer + ":pin=bt2020:min=bt2020nc:rin=limited" +
		":t=smpte2084:p=bt2020:m=ictcp:r=limited:npl=1000,format=yuv420p16le", nil
}
