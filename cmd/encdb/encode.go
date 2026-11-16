package main

import (
	"fmt"
	"io"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/scottlaird/videoencoder/internal/benchweb"
)

// Encode is a row of encodes.parquet: the derived columns the pages
// filter and group by, then the log's own columns (results.csv's, with
// /api/results's nulls, and with empty strings null too), then which
// log it came from.
type Encode struct {
	Machine        *string `parquet:"machine,optional,dict"`
	Hardware       *string `parquet:"hardware,optional,dict"`
	Family         string  `parquet:"family,dict"`
	Software       bool    `parquet:"software"`
	Preset         string  `parquet:"preset,dict"`
	BitDepth       string  `parquet:"bit_depth,dict"` // the setting: "8", "10" or "as source"
	Keyframes      string  `parquet:"keyframes,dict"`
	Variant        string  `parquet:"variant,dict"`
	SourceBitDepth *int32  `parquet:"source_bit_depth,optional"` // the camera file's
	InputBitDepth  *int32  `parquet:"input_bit_depth,optional"`  // what the encoder read
	OutBitDepth    *int32  `parquet:"out_bit_depth,optional"`    // what it wrote
	Device         *string `parquet:"device,optional,dict"`      // CPU for software, else GPU or SoC
	Clip           string  `parquet:"clip,dict"`
	Res            *string `parquet:"res,optional,dict"`
	Codec          string  `parquet:"codec,dict"`
	StdClip        bool    `parquet:"std_clip"`
	StdScorer      bool    `parquet:"std_scorer"`

	Sample           *string   `parquet:"sample,optional,dict"`
	Encoder          *string   `parquet:"encoder,optional,dict"`
	Flagset          *string   `parquet:"flagset,optional,dict"`
	Args             *string   `parquet:"args,optional,dict"`
	QualityFlag      *string   `parquet:"quality_flag,optional,dict"`
	Quality          float64   `parquet:"quality"`
	Key              *string   `parquet:"key,optional"`
	FlagsetKey       *string   `parquet:"flagset_key,optional,dict"`
	Bytes            int64     `parquet:"bytes"`
	Kbps             float64   `parquet:"kbps"`
	WallS            float64   `parquet:"wall_s"`
	UserS            float64   `parquet:"user_s"`
	SysS             float64   `parquet:"sys_s"`
	MaxRSSKB         int64     `parquet:"max_rss_kb"`
	VMAF             *float64  `parquet:"vmaf,optional"`
	VMAFHMean        *float64  `parquet:"vmaf_hmean,optional"`
	VMAFP1           *float64  `parquet:"vmaf_p1,optional"`
	VMAFNeg          *float64  `parquet:"vmaf_neg,optional"`
	PSNRY            *float64  `parquet:"psnr_y,optional"`
	PSNRCb           *float64  `parquet:"psnr_cb,optional"`
	PSNRCr           *float64  `parquet:"psnr_cr,optional"`
	CIEDE2000        *float64  `parquet:"ciede2000,optional"`
	FFmpegVersion    *string   `parquet:"ffmpeg_version,optional,dict"`
	Host             *string   `parquet:"host,optional,dict"`
	Time             time.Time `parquet:"time,timestamp(microsecond:local)"` // UTC
	ColorShiftP99    *float64  `parquet:"color_shift_p99,optional"`
	ColorShiftP999   *float64  `parquet:"color_shift_p999,optional"`
	Scorer           *string   `parquet:"scorer,optional,dict"`
	ColorFlickerP99  *float64  `parquet:"color_flicker_p99,optional"`
	ColorFlickerP999 *float64  `parquet:"color_flicker_p999,optional"`
	Reference        *string   `parquet:"reference,optional,dict"`
	CPU              *string   `parquet:"cpu,optional,dict"`
	EncoderVersion   *string   `parquet:"encoder_version,optional,dict"`
	CAMBI            *float64  `parquet:"cambi,optional"`
	CAMBIFR          *float64  `parquet:"cambi_fr,optional"`
	CAMBIFRMax       *float64  `parquet:"cambi_fr_max,optional"`
	Timed            bool      `parquet:"timed"`
	RowID            *string   `parquet:"row_id,optional"`
	Source           *string   `parquet:"source,optional,dict"`
	SampleID         *string   `parquet:"sample_id,optional,dict"`
	Width            *int64    `parquet:"width,optional"`
	Height           *int64    `parquet:"height,optional"`
	FPS              *float64  `parquet:"fps,optional"`
	DurationS        *float64  `parquet:"duration_s,optional"`
	SourceWidth      *int64    `parquet:"source_width,optional"`
	SourceHeight     *int64    `parquet:"source_height,optional"`
	ColorTransfer    *string   `parquet:"color_transfer,optional,dict"`
	ColorPrimaries   *string   `parquet:"color_primaries,optional,dict"`
	OutCodec         *string   `parquet:"out_codec,optional,dict"`
	OutProfile       *string   `parquet:"out_profile,optional,dict"`
	OutLevel         *string   `parquet:"out_level,optional,dict"`
	OutPixFmt        *string   `parquet:"out_pix_fmt,optional,dict"`
	OutKeyframes     *int64    `parquet:"out_keyframes,optional"` // results.csv's keyframes
	PeakKbps1s       *float64  `parquet:"peak_kbps_1s,optional"`
	VMAFModel        *string   `parquet:"vmaf_model,optional,dict"`
	TargetVMAF       *float64  `parquet:"target_vmaf,optional"`
	Pass             *bool     `parquet:"pass,optional"`
	ScoreS           *float64  `parquet:"score_s,optional"`
	BatchSize        *int64    `parquet:"batch_size,optional"`
	BatchID          *string   `parquet:"batch_id,optional"`
	EncbenchVersion  *string   `parquet:"encbench_version,optional,dict"`
	Config           *string   `parquet:"config,optional,dict"`
	Job              *string   `parquet:"job,optional,dict"`
	OutSHA256        *string   `parquet:"out_sha256,optional"`
	CPUBusyPct       *float64  `parquet:"cpu_busy_pct,optional"`
	CPUMHz           *float64  `parquet:"cpu_mhz,optional"`
	CPUIPC           *float64  `parquet:"cpu_ipc,optional"`
	CPUPkgW          *float64  `parquet:"cpu_pkg_w,optional"`
	CPUCoreW         *float64  `parquet:"cpu_core_w,optional"`
	CPUPkgTempC      *float64  `parquet:"cpu_pkg_temp_c,optional"`
	CPULLCHitPct     *float64  `parquet:"cpu_llc_hit_pct,optional"`

	Log string `parquet:"log,dict"`
}

// setRaw copies r's columns into e.
func (e *Encode) setRaw(r *benchweb.ResultRow) {
	e.Sample, e.Encoder, e.Flagset, e.Args = opt(r.Sample), opt(r.Encoder), opt(r.Flagset), opt(r.Args)
	e.QualityFlag, e.Quality, e.Key, e.FlagsetKey = opt(r.QualityFlag), r.Quality, opt(r.Key), opt(r.FlagsetKey)
	e.Bytes, e.Kbps, e.WallS, e.UserS, e.SysS, e.MaxRSSKB = r.Bytes, r.Kbps, r.WallS, r.UserS, r.SysS, r.MaxRSSKB
	e.VMAF, e.VMAFHMean, e.VMAFP1, e.VMAFNeg = r.VMAF, r.VMAFHMean, r.VMAFP1, r.VMAFNeg
	e.PSNRY, e.PSNRCb, e.PSNRCr, e.CIEDE2000 = r.PSNRY, r.PSNRCb, r.PSNRCr, r.CIEDE2000
	e.FFmpegVersion, e.Host, e.Time = opt(r.FFmpegVersion), opt(r.Host), r.Time.UTC()
	e.ColorShiftP99, e.ColorShiftP999, e.Scorer = r.ColorShiftP99, r.ColorShiftP999, opt(r.Scorer)
	e.ColorFlickerP99, e.ColorFlickerP999 = r.ColorFlickerP99, r.ColorFlickerP999
	e.Reference, e.CPU, e.EncoderVersion = opt(r.Reference), opt(r.CPU), opt(r.EncoderVersion)
	e.CAMBI, e.CAMBIFR, e.CAMBIFRMax, e.Timed = r.CAMBI, r.CAMBIFR, r.CAMBIFRMax, r.Timed
	e.RowID, e.Source, e.SampleID = opt(r.RowID), opt(r.Source), opt(r.SampleID)
	e.Width, e.Height, e.FPS, e.DurationS = r.Width, r.Height, r.FPS, r.DurationS
	e.SourceWidth, e.SourceHeight = r.SourceWidth, r.SourceHeight
	e.ColorTransfer, e.ColorPrimaries = opt(r.ColorTransfer), opt(r.ColorPrimaries)
	e.OutCodec, e.OutProfile, e.OutLevel, e.OutPixFmt = opt(r.OutCodec), opt(r.OutProfile), opt(r.OutLevel), opt(r.OutPixFmt)
	e.OutKeyframes, e.PeakKbps1s = r.Keyframes, r.PeakKbps1s
	e.VMAFModel, e.TargetVMAF, e.Pass, e.ScoreS = opt(r.VMAFModel), r.TargetVMAF, r.Pass, r.ScoreS
	e.BatchSize, e.BatchID = r.BatchSize, opt(r.BatchID)
	e.EncbenchVersion, e.Config, e.Job, e.OutSHA256 = opt(r.EncbenchVer), opt(r.Config), opt(r.Job), opt(r.OutSHA256)
	e.CPUBusyPct, e.CPUMHz, e.CPUIPC = r.CPUBusyPct, r.CPUMHz, r.CPUIPC
	e.CPUPkgW, e.CPUCoreW, e.CPUPkgTempC, e.CPULLCHitPct = r.CPUPkgW, r.CPUCoreW, r.CPUPkgTempC, r.CPULLCHit
}

// opt is s, or nil (null) if it's empty.
func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ptr[T any](v T) *T { return &v }

// writeParquet writes rows as a zstd-compressed parquet file.
func writeParquet(w io.Writer, rows []Encode) error {
	pw := parquet.NewGenericWriter[Encode](w, parquet.Compression(&parquet.Zstd))
	if _, err := pw.Write(rows); err != nil {
		return fmt.Errorf("writing parquet: %w", err)
	}
	return pw.Close()
}
