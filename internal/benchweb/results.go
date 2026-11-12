package benchweb

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/scottlaird/videoencoder/internal/bench"
)

// ResultsFormat is the version of the /api/results document. It
// changes only if a field is renamed, removed or changes meaning;
// fields may be added without a change.
const ResultsFormat = 1

// Results is the /api/results document: every row of the results logs
// the queue's work files write to, in log order (logs sorted by path).
// A row still being appended is left out.
type Results struct {
	Format int         `json:"format"`
	Logs   []string    `json:"logs"`
	Rows   []ResultRow `json:"rows"`
}

// ResultRow is one encode and its scores: a line of results.csv, with
// the same names. Metrics a row lacks (columns added after it was
// logged, or VMAFNeg for models without it) are null.
type ResultRow struct {
	Log              int       `json:"log"` // index into Results.Logs
	Sample           string    `json:"sample"`
	Reference        string    `json:"reference"`
	Encoder          string    `json:"encoder"`
	Flagset          string    `json:"flagset"`
	Args             string    `json:"args"`
	QualityFlag      string    `json:"quality_flag"`
	Quality          float64   `json:"quality"`
	Key              string    `json:"key"`
	FlagsetKey       string    `json:"flagset_key"`
	Bytes            int64     `json:"bytes"`
	Kbps             float64   `json:"kbps"`
	Timed            bool      `json:"timed"`
	WallS            float64   `json:"wall_s"`
	UserS            float64   `json:"user_s"`
	SysS             float64   `json:"sys_s"`
	MaxRSSKB         int64     `json:"max_rss_kb"`
	VMAF             *float64  `json:"vmaf"`
	VMAFHMean        *float64  `json:"vmaf_hmean"`
	VMAFP1           *float64  `json:"vmaf_p1"`
	VMAFNeg          *float64  `json:"vmaf_neg"`
	PSNRY            *float64  `json:"psnr_y"`
	PSNRCb           *float64  `json:"psnr_cb"`
	PSNRCr           *float64  `json:"psnr_cr"`
	CIEDE2000        *float64  `json:"ciede2000"`
	ColorShiftP99    *float64  `json:"color_shift_p99"`
	ColorShiftP999   *float64  `json:"color_shift_p999"`
	ColorFlickerP99  *float64  `json:"color_flicker_p99"`
	ColorFlickerP999 *float64  `json:"color_flicker_p999"`
	CAMBI            *float64  `json:"cambi"`
	CAMBIFR          *float64  `json:"cambi_fr"`
	CAMBIFRMax       *float64  `json:"cambi_fr_max"`
	Scorer           string    `json:"scorer"`
	FFmpegVersion    string    `json:"ffmpeg_version"`
	EncoderVersion   string    `json:"encoder_version"`
	Host             string    `json:"host"`
	CPU              string    `json:"cpu"`
	Time             time.Time `json:"time"`
}

// metric is f, or nil (JSON null) if it's NaN or infinite, which JSON
// can't hold.
func metric(f float64) *float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}

func resultRow(log int, r *bench.Row) ResultRow {
	m := r.Metrics
	return ResultRow{
		Log: log, Sample: r.Sample, Reference: r.Reference, Encoder: r.Encoder,
		Flagset: r.Flagset, Args: r.Args, QualityFlag: r.QualityFlag, Quality: r.Quality,
		Key: r.Key, FlagsetKey: r.FlagsetKey, Bytes: r.Bytes, Kbps: r.Kbps,
		Timed: r.Timed, WallS: r.WallS, UserS: r.UserS, SysS: r.SysS, MaxRSSKB: r.MaxRSSKB,
		VMAF: metric(m.VMAF), VMAFHMean: metric(m.VMAFHMean), VMAFP1: metric(m.VMAFP1),
		VMAFNeg: metric(m.VMAFNeg), PSNRY: metric(m.PSNRY), PSNRCb: metric(m.PSNRCb),
		PSNRCr: metric(m.PSNRCr), CIEDE2000: metric(m.CIEDE2000),
		ColorShiftP99: metric(m.ColorShiftP99), ColorShiftP999: metric(m.ColorShiftP999),
		ColorFlickerP99: metric(m.ColorFlickerP99), ColorFlickerP999: metric(m.ColorFlickerP999),
		CAMBI: metric(m.CAMBI), CAMBIFR: metric(m.CAMBIFR), CAMBIFRMax: metric(m.CAMBIFRMax),
		Scorer: r.Scorer, FFmpegVersion: r.FFmpegVersion, EncoderVersion: r.EncoderVersion,
		Host: r.Host, CPU: r.CPU, Time: r.Time,
	}
}

// Results reads the queue's results logs now.
func (s *Server) Results() (Results, error) {
	out := Results{Format: ResultsFormat, Logs: s.Runner.ResultsLogs(), Rows: []ResultRow{}}
	if out.Logs == nil {
		out.Logs = []string{}
	}
	for i, path := range out.Logs {
		res, err := bench.ReadResults(path)
		if err != nil {
			return Results{}, err
		}
		for j := range res.Rows {
			out.Rows = append(out.Rows, resultRow(i, &res.Rows[j]))
		}
	}
	return out, nil
}

func (s *Server) results(w http.ResponseWriter, r *http.Request) {
	res, err := s.Results()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(res)
}
