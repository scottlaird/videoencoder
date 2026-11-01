package bench

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	sdrModel = `{"param_dict": {}, "model_dict": {"model_type": "LIBSVMNUSVR"}}`
	hdrModel = `{"model_dict": {"conversion_target": {"colorspace": {"trc": "smpte2084", "matrix": "ictcp"}}}}`
)

func writeModel(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadVMAFModel(t *testing.T) {
	sdr, err := LoadVMAFModel(writeModel(t, "vmaf_v1.0.16_3d0h.json", sdrModel))
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := LoadVMAFModel(writeModel(t, "hdr.json", hdrModel))
	if err != nil {
		t.Fatal(err)
	}
	if sdr.HDR || !hdr.HDR {
		t.Errorf("HDR = %v and %v, want false for an SDR model and true for one with a PQ conversion_target", sdr.HDR, hdr.HDR)
	}
	if got, want := sdr.Name(), "vmaf_v1.0.16_3d0h"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if sdr.ID() == "" || sdr.ID() == hdr.ID() {
		t.Errorf("IDs %q and %q: want distinct and set", sdr.ID(), hdr.ID())
	}
	if (VMAFModel{}).Name() != "" || (VMAFModel{}).ID() != "" {
		t.Error("the built-in model should have no name or ID")
	}
	for name, path := range map[string]string{
		"missing":       filepath.Join(t.TempDir(), "none.json"),
		"not a model":   writeModel(t, "x.json", `{"frames": []}`),
		"not json":      writeModel(t, "x.json", `nope`),
		"odd transfer":  writeModel(t, "x.json", `{"model_dict": {"conversion_target": {"colorspace": {"trc": "hlg"}}}}`),
		"pipe in path":  writeModel(t, "a|b.json", sdrModel),
		"quote in path": writeModel(t, "a'b.json", sdrModel),
	} {
		if _, err := LoadVMAFModel(path); err == nil {
			t.Errorf("%s: LoadVMAFModel(%s) = nil error, want one", name, path)
		}
	}
}

func TestScoreArgvModel(t *testing.T) {
	m := VMAFModel{Path: "/models/a:b/hdr.json", HDR: true}
	conv, err := hdrConversion("smpte2084")
	if err != nil {
		t.Fatal(err)
	}
	argv := scoreArgv("d.mp4", "r.mov", 1920, 1080, 4, "/l.json", "/c.json", "/d.blocks", "/r.blocks", "", "", m, conv)
	graph := argv[slices.Index(argv, "-filter_complex")+1]
	for _, want := range []string{
		`model='path=/models/a\:b/hdr.json\:name=vmaf'`,
		"[d]zscale=tin=smpte2084:", "m=ictcp", "format=yuv420p16le[dq]", "[dq][rq]libvmaf=",
	} {
		if !strings.Contains(graph, want) {
			t.Errorf("filtergraph %q lacks %q", graph, want)
		}
	}
	if strings.Contains(graph, "vmaf_neg") {
		t.Errorf("filtergraph %q asks for vmaf_neg, which only the built-in model has", graph)
	}
	// CIEDE2000 still gets the unconverted pictures.
	if strings.Contains(graph, "[dc]zscale") {
		t.Errorf("filtergraph %q converts the CIEDE2000 input", graph)
	}

	if _, err := hdrConversion("bt709"); err == nil {
		t.Error("hdrConversion(bt709) = nil error, want one")
	}
	if c, err := hdrConversion("arib-std-b67"); err != nil || !strings.Contains(c, "tin=arib-std-b67") {
		t.Errorf("hdrConversion(arib-std-b67) = %q, %v", c, err)
	}
}

func TestParseVMAFLogWithoutNeg(t *testing.T) {
	log := `{"frames": [{"metrics": {"vmaf": 93}}], "pooled_metrics": {
	  "vmaf": {"mean": 93, "harmonic_mean": 93},
	  "psnr_y": {"mean": 44}, "psnr_cb": {"mean": 47}, "psnr_cr": {"mean": 46}}}`
	got, err := parseVMAFLog([]byte(log))
	if err != nil {
		t.Fatal(err)
	}
	if got.VMAF != 93 || !math.IsNaN(got.VMAFNeg) {
		t.Errorf("VMAF %v, VMAFNeg %v; want 93 and NaN", got.VMAF, got.VMAFNeg)
	}
}
