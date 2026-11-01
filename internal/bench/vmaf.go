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
// VMAF only looks at luma, so it can't see color casts at all; the
// chroma PSNRs and CIEDE2000 are there to catch those. For all of them,
// higher is better, and a perfect match is +Inf. CIEDE2000 is measured
// on sampled frames at half resolution (see colorFrameStep), the rest
// on every frame.
type Metrics struct {
	VMAF      float64 // mean
	VMAFHMean float64 // harmonic mean
	VMAFP1    float64 // 1st-percentile frame
	VMAFNeg   float64 // mean of the "no enhancement gain" model; NaN for models without one
	PSNRY     float64
	PSNRCb    float64
	PSNRCr    float64
	CIEDE2000 float64
	// ColorShiftP99 is the 99th percentile, over 64x64 blocks of every
	// frame, of the error in each block's average color, as
	// ΔE_ITP chroma (ITU-R BT.2124: hue and saturation, ignoring
	// lightness, with about 1 the smallest visible difference). Lower is
	// better. Unlike the frame averages above, it catches a cast
	// confined to part of the picture, and it is measured in HDR for
	// HDR. ColorShiftP999 is the same at the 99.9th percentile.
	ColorShiftP99  float64
	ColorShiftP999 float64
	// ColorFlickerP99 and P999 are percentiles of how far a block's
	// color error strays from its own average over colorWindow frames:
	// a cast that comes and goes, which the eye notices far more than a
	// steady one of the same size. Lower is better.
	ColorFlickerP99  float64
	ColorFlickerP999 float64
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

// colorFrameStep and the half-resolution scale make CIEDE2000, which
// costs several times as much as VMAF per pixel, affordable. Color
// casts are broad and persistent, so every sixth frame at chroma
// resolution (half size, for 4:2:0) is enough to find them.
const colorFrameStep = 6

// colorBlock is the side, in pixels at scoring size, of the blocks
// whose average colors are compared: AV1's superblock size. A cast that
// is faint per pixel but covers a large patch is what the eye notices,
// and smaller blocks drown it in the ordinary noise of textured areas
// such as moving water.
const colorBlock = 64

// blockSize is the size of the block-average images for a scoring size.
func blockSize(width, height uint64) (uint64, uint64) {
	return max(1, width/colorBlock), max(1, height/colorBlock)
}

// scoreArgv returns the ffmpeg command that scores distorted against
// reference. It writes two libvmaf JSON logs: logPath with VMAF and
// PSNR on every frame, and colorLogPath with CIEDE2000 on sampled
// frames. It also writes, for every frame, block-average images of both
// sides (16-bit RGB in
// the reference's own transfer and primaries, one pixel per colorBlock
// square) to distBlocks and refBlocks, for ColorShift. The distorted
// video is scaled to the reference size first, and both are converted
// to one pixel format, since libvmaf needs matching inputs.
//
// prefilter, if not "", is applied to both inputs for VMAF and
// CIEDE2000: for HDR, a tone map to SDR, since those are SDR metrics.
// The block images skip it: color is compared in HDR as HDR, where a
// tone map would squeeze the error in highlights. blockFilter, if not
// "", converts them first (HLG to PQ, say).
//
// model is what VMAF is scored with. vmafConv, if not "", converts both
// inputs once more for VMAF alone, after the 4:4:4 conversion: what an
// HDR model expects (see hdrConversion).
func scoreArgv(distorted, reference string, width, height uint64, threads int, logPath, colorLogPath, distBlocks, refBlocks, prefilter, blockFilter string, model VMAFModel, vmafConv string) []string {
	pre := ""
	if prefilter != "" {
		pre = prefilter + ","
	}
	bpre := ""
	if blockFilter != "" {
		bpre = blockFilter + ","
	}
	sample := fmt.Sprintf("select='not(mod(n,%d))',scale=%d:%d:flags=area", colorFrameStep, (width/2)&^1, (height/2)&^1)
	bw, bh := blockSize(width, height)
	vmafIn := "[d][r]"
	if vmafConv != "" {
		vmafIn = fmt.Sprintf("[d]%s[dq];[r]%s[rq];[dq][rq]", vmafConv, vmafConv)
	}
	// The 16-bit 4:4:4 step matters: swscale's direct 8-bit YUV to RGB
	// path is imprecise enough to read a lossless 8-bit copy as a color
	// shift of 2.7.
	blocks := fmt.Sprintf("%sformat=yuv444p16le,format=rgb48le,scale=%d:%d:flags=area", bpre, bw, bh)
	// The VMAF and CIEDE2000 inputs are prepared exactly as scorer v3
	// did: prefilter on the decoded frames, then scale and convert. A
	// prefilter that runs after the 4:4:4 conversion instead scores
	// every encode about 0.8 VMAF lower.
	graph := fmt.Sprintf(
		"[0:v]"+frameIndexPTS+",split[dv][dbs];"+
			"[dv]%sscale=%d:%d:flags=bicubic,format=yuv444p10le,split[d][dc];"+
			"[dbs]scale=%d:%d:flags=bicubic,%s[dbo];"+
			"[1:v]"+frameIndexPTS+",split[rv][rbs];"+
			"[rv]%sformat=yuv444p10le,split[r][rc];"+
			"[rbs]%s[rbo];"+
			"%slibvmaf=log_fmt=json:log_path=%s:n_threads=%d"+
			":model='%s'"+
			":feature='name=psnr'[quality];"+
			"[dc]%s[ds];[rc]%s[rs];"+
			"[ds][rs]libvmaf=log_fmt=json:log_path=%s:n_threads=%d:feature='name=ciede'[color]",
		pre, width, height, width, height, blocks, pre, blocks,
		vmafIn, escapeFilterValue(logPath), threads, model.option(height),
		sample, sample, escapeFilterValue(colorLogPath), threads)
	return []string{
		"ffmpeg", "-hide_banner", "-loglevel", "error", "-nostats", "-y",
		"-i", distorted, "-i", reference,
		"-filter_complex", graph,
		"-map", "[quality]", "-f", "null", "-",
		"-map", "[color]", "-f", "null", "-",
		// Passthrough, or the muxer pads or drops frames to fit a
		// constant rate (frameIndexPTS's microsecond ticks drop nearly
		// all of them), and frames stop lining up with the reference.
		"-map", "[dbo]", "-fps_mode", "passthrough", "-f", "rawvideo", distBlocks,
		"-map", "[rbo]", "-fps_mode", "passthrough", "-f", "rawvideo", refBlocks,
	}
}

// Score measures distorted against reference, which must be width x
// height (distorted is scaled to match), and whose color transfer
// (ffprobe's name: "smpte2084", "bt709", ...) is transfer. Scratch logs
// go in tmpDir, named after name. See scoreArgv for prefilter. VMAF is
// scored with model; an HDR model needs an HDR reference, and no tone
// map in prefilter.
func Score(ctx context.Context, e Executor, distorted, reference string, width, height uint64, threads int, tmpDir, name, prefilter, transfer string, model VMAFModel) (Metrics, error) {
	vmafConv := ""
	if model.HDR {
		var err error
		if vmafConv, err = hdrConversion(transfer); err != nil {
			return Metrics{}, err
		}
	}
	logPath := filepath.Join(tmpDir, name+".vmaf.json")
	colorLogPath := filepath.Join(tmpDir, name+".ciede.json")
	distBlocks := filepath.Join(tmpDir, name+".dist.blocks")
	refBlocks := filepath.Join(tmpDir, name+".ref.blocks")
	for _, p := range []string{logPath, colorLogPath, distBlocks, refBlocks} {
		defer os.Remove(p)
	}
	space, blockFilter := colorSpaceFor(transfer)
	argv := scoreArgv(distorted, reference, width, height, threads, logPath, colorLogPath, distBlocks, refBlocks, prefilter, blockFilter, model, vmafConv)
	if _, err := e.Exec(ctx, argv); err != nil {
		return Metrics{}, fmt.Errorf("scoring: %w", err)
	}
	m, err := readLogs(logPath, colorLogPath)
	if err != nil {
		return Metrics{}, err
	}
	bw, bh := blockSize(width, height)
	st, err := ColorShift(distBlocks, refBlocks, bw, bh, space)
	m.ColorShiftP99, m.ColorShiftP999 = st.ShiftP99, st.ShiftP999
	m.ColorFlickerP99, m.ColorFlickerP999 = st.FlickerP99, st.FlickerP999
	return m, err
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
	// Only the built-in model has a no-enhancement-gain variant.
	m.VMAFNeg = math.NaN()
	if _, ok := l.Pooled["vmaf_neg"]; ok {
		if m.VMAFNeg, err = mean("vmaf_neg"); err != nil {
			return Metrics{}, err
		}
	}
	for _, f := range []struct {
		name string
		dst  *float64
	}{
		{"vmaf", &m.VMAF},
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

// parseColorLog reads the mean CIEDE2000 out of a libvmaf JSON log.
func parseColorLog(data []byte) (float64, error) {
	var l vmafLog
	if err := json.Unmarshal(data, &l); err != nil {
		return 0, fmt.Errorf("parsing libvmaf color log: %w", err)
	}
	p, ok := l.Pooled["ciede2000"]
	if !ok {
		return 0, fmt.Errorf("libvmaf color log has no ciede2000")
	}
	return orInf(p.Mean), nil
}

// readLogs reads the two logs written by the scoreArgv command.
func readLogs(logPath, colorLogPath string) (Metrics, error) {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return Metrics{}, fmt.Errorf("reading libvmaf log: %w", err)
	}
	m, err := parseVMAFLog(data)
	if err != nil {
		return Metrics{}, err
	}
	if data, err = os.ReadFile(colorLogPath); err != nil {
		return Metrics{}, fmt.Errorf("reading libvmaf color log: %w", err)
	}
	m.CIEDE2000, err = parseColorLog(data)
	return m, err
}
