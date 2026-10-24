package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Metrics are the quality scores of one encode against its reference.
//
// VMAF only looks at luma, so it can't see colour casts at all; the
// chroma PSNRs and CIEDE2000 are there to catch those. For all of them,
// higher is better, and a perfect match is +Inf. CIEDE2000 is measured
// on sampled frames at half resolution (see colourFrameStep), the rest
// on every frame.
type Metrics struct {
	VMAF      float64 // mean
	VMAFHMean float64 // harmonic mean
	VMAFP1    float64 // 1st-percentile frame
	VMAFNeg   float64 // mean of the "no enhancement gain" model
	PSNRY     float64
	PSNRCb    float64
	PSNRCr    float64
	CIEDE2000 float64
}

// vmafModel picks the model trained for the reference's viewing
// conditions: the 4K model for 2160p and up, the 1080p one otherwise.
func vmafModel(height uint64) string {
	if height >= 2160 {
		return "vmaf_4k_v0.6.1"
	}
	return "vmaf_v0.6.1"
}

// frameIndexPTS renumbers frames 0, 1, 2… so libvmaf pairs them by
// position rather than by timestamp. Containers round timestamps
// differently (Matroska to the millisecond, so frame 2 of 30 fps video
// is at 67 ms there and 66.67 ms in MP4), and pairing by timestamp then
// compares every third frame against the wrong reference frame.
const frameIndexPTS = "settb=AVTB,setpts=N"

// colourFrameStep and the half-resolution scale make CIEDE2000, which
// costs several times as much as VMAF per pixel, affordable. Colour
// casts are broad and persistent, so every sixth frame at chroma
// resolution (half size, for 4:2:0) is enough to find them.
const colourFrameStep = 6

// scoreArgv returns the ffmpeg command that scores distorted against
// reference. It writes two libvmaf JSON logs: logPath with VMAF and
// PSNR on every frame, and colourLogPath with CIEDE2000 on sampled
// frames. The distorted video is scaled to the reference size first,
// and both are converted to one pixel format, since libvmaf needs
// matching inputs.
//
// prefilter, if not "", is applied to both inputs first; for HDR, a
// tone map to SDR, since VMAF and CIEDE2000 are only meaningful there.
func scoreArgv(distorted, reference string, width, height uint64, threads int, logPath, colourLogPath, prefilter string) []string {
	model := vmafModel(height)
	pre := ""
	if prefilter != "" {
		pre = prefilter + ","
	}
	sample := fmt.Sprintf("select='not(mod(n,%d))',scale=%d:%d:flags=area", colourFrameStep, (width/2)&^1, (height/2)&^1)
	graph := fmt.Sprintf(
		"[0:v]%sscale=%d:%d:flags=bicubic,format=yuv444p10le,"+frameIndexPTS+",split[d][dc];"+
			"[1:v]%sformat=yuv444p10le,"+frameIndexPTS+",split[r][rc];"+
			"[d][r]libvmaf=log_fmt=json:log_path=%s:n_threads=%d"+
			":model='version=%s\\:name=vmaf|version=%sneg\\:name=vmaf_neg'"+
			":feature='name=psnr'[quality];"+
			"[dc]%s[ds];[rc]%s[rs];"+
			"[ds][rs]libvmaf=log_fmt=json:log_path=%s:n_threads=%d:feature='name=ciede'[colour]",
		pre, width, height, pre, escapeFilterValue(logPath), threads, model, model,
		sample, sample, escapeFilterValue(colourLogPath), threads)
	return []string{
		"ffmpeg", "-hide_banner", "-loglevel", "error", "-nostats",
		"-i", distorted, "-i", reference,
		"-filter_complex", graph,
		"-map", "[quality]", "-f", "null", "-",
		"-map", "[colour]", "-f", "null", "-",
	}
}

// Score measures distorted against reference, which must be width x
// height (distorted is scaled to match). Scratch logs go in tmpDir,
// named after name. See scoreArgv for prefilter.
func Score(ctx context.Context, e Executor, distorted, reference string, width, height uint64, threads int, tmpDir, name, prefilter string) (Metrics, error) {
	logPath := filepath.Join(tmpDir, name+".vmaf.json")
	colourLogPath := filepath.Join(tmpDir, name+".ciede.json")
	defer os.Remove(logPath)
	defer os.Remove(colourLogPath)
	argv := scoreArgv(distorted, reference, width, height, threads, logPath, colourLogPath, prefilter)
	if _, err := e.Exec(ctx, argv); err != nil {
		return Metrics{}, fmt.Errorf("scoring: %w", err)
	}
	return readLogs(logPath, colourLogPath)
}

// escapeFilterValue escapes s for use as an option value inside an
// ffmpeg filtergraph.
func escapeFilterValue(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, `:`, `\:`, `,`, `\,`, `;`, `\;`, `[`, `\[`, `]`, `\]`)
	return r.Replace(s)
}

type vmafLog struct {
	Frames []struct {
		Metrics map[string]*float64 `json:"metrics"`
	} `json:"frames"`
	Pooled map[string]struct {
		Mean         *float64 `json:"mean"`
		HarmonicMean *float64 `json:"harmonic_mean"`
	} `json:"pooled_metrics"`
}

// parseVMAFLog reads the metrics out of a libvmaf JSON log.
func parseVMAFLog(data []byte) (Metrics, error) {
	var l vmafLog
	if err := json.Unmarshal(data, &l); err != nil {
		return Metrics{}, fmt.Errorf("parsing libvmaf log: %w", err)
	}
	mean := func(name string) (float64, error) {
		p, ok := l.Pooled[name]
		if !ok {
			return 0, fmt.Errorf("libvmaf log has no %q", name)
		}
		return orInf(p.Mean), nil
	}

	var m Metrics
	var err error
	for _, f := range []struct {
		name string
		dst  *float64
	}{
		{"vmaf", &m.VMAF},
		{"vmaf_neg", &m.VMAFNeg},
		{"psnr_y", &m.PSNRY},
		{"psnr_cb", &m.PSNRCb},
		{"psnr_cr", &m.PSNRCr},
	} {
		if *f.dst, err = mean(f.name); err != nil {
			return Metrics{}, err
		}
	}
	m.VMAFHMean = orInf(l.Pooled["vmaf"].HarmonicMean)

	var frames []float64
	for _, f := range l.Frames {
		frames = append(frames, orInf(f.Metrics["vmaf"]))
	}
	if len(frames) == 0 {
		return Metrics{}, fmt.Errorf("libvmaf log has no frames")
	}
	m.VMAFP1 = percentile(frames, 1)
	return m, nil
}

// orInf maps libvmaf's null (an infinite score: identical frames) to +Inf.
func orInf(p *float64) float64 {
	if p == nil {
		return math.Inf(1)
	}
	return *p
}

// percentile returns the p-th percentile of xs by nearest rank.
func percentile(xs []float64, p float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	i := int(math.Ceil(p/100*float64(len(s)))) - 1
	return s[max(i, 0)]
}

// parseColourLog reads the mean CIEDE2000 out of a libvmaf JSON log.
func parseColourLog(data []byte) (float64, error) {
	var l vmafLog
	if err := json.Unmarshal(data, &l); err != nil {
		return 0, fmt.Errorf("parsing libvmaf colour log: %w", err)
	}
	p, ok := l.Pooled["ciede2000"]
	if !ok {
		return 0, fmt.Errorf("libvmaf colour log has no ciede2000")
	}
	return orInf(p.Mean), nil
}

// readLogs reads the two logs written by the scoreArgv command.
func readLogs(logPath, colourLogPath string) (Metrics, error) {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return Metrics{}, fmt.Errorf("reading libvmaf log: %w", err)
	}
	m, err := parseVMAFLog(data)
	if err != nil {
		return Metrics{}, err
	}
	if data, err = os.ReadFile(colourLogPath); err != nil {
		return Metrics{}, fmt.Errorf("reading libvmaf colour log: %w", err)
	}
	m.CIEDE2000, err = parseColourLog(data)
	return m, err
}
