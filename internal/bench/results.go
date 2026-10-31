package bench

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Row is one encode and its scores: a line of results.csv.
type Row struct {
	Sample string
	// Reference is the file Sample's encodes were scored against: Sample
	// itself unless the config names another (Config.References).
	Reference   string
	Encoder     string
	Flagset     string
	Args        string
	QualityFlag string
	Quality     float64
	Key         string // identifies this encode: sample, flagset, quality
	FlagsetKey  string // identifies the flagset, independent of its name
	Bytes       int64
	Kbps        float64
	WallS       float64
	UserS       float64
	SysS        float64
	MaxRSSKB    int64
	Metrics     Metrics
	// Scorer is the scoring method's version. Rows from other versions
	// stay in the log but don't count.
	Scorer        string
	FFmpegVersion string
	Host          string
	Time          time.Time
}

var resultColumns = []string{
	"sample", "encoder", "flagset", "args", "quality_flag", "quality",
	"key", "flagset_key", "bytes", "kbps", "wall_s", "user_s", "sys_s", "max_rss_kb",
	"vmaf", "vmaf_hmean", "vmaf_p1", "vmaf_neg", "psnr_y", "psnr_cb", "psnr_cr", "ciede2000",
	"ffmpeg_version", "host", "time", "color_shift_p99", "color_shift_p999", "scorer",
	"color_flicker_p99", "color_flicker_p999", "reference",
}

// scorer is the current scoring method: v2 added CIEDE2000 on sampled
// frames; v3 the block color shift; v4 measured it as ΔE_ITP before any
// tone map, on every frame (v3's block images held only about three),
// and added color flicker, but scored VMAF about 0.8 low; v5 is v4 with
// v3's VMAF restored; v6 converts block images precisely, which v5 got
// wrong for 8-bit encodes (10-bit ones moved by 0.04 at most).
const scorer = "v6"

// ScorerVersion is scorer, for callers that keep scores of their own.
const ScorerVersion = scorer

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func (r *Row) record() []string {
	m := r.Metrics
	return []string{
		r.Sample, r.Encoder, r.Flagset, r.Args, r.QualityFlag, ftoa(r.Quality),
		r.Key, r.FlagsetKey, strconv.FormatInt(r.Bytes, 10), ftoa(r.Kbps),
		ftoa(r.WallS), ftoa(r.UserS), ftoa(r.SysS), strconv.FormatInt(r.MaxRSSKB, 10),
		ftoa(m.VMAF), ftoa(m.VMAFHMean), ftoa(m.VMAFP1), ftoa(m.VMAFNeg),
		ftoa(m.PSNRY), ftoa(m.PSNRCb), ftoa(m.PSNRCr), ftoa(m.CIEDE2000),
		r.FFmpegVersion, r.Host, r.Time.UTC().Format(time.RFC3339),
		ftoa(m.ColorShiftP99), ftoa(m.ColorShiftP999), r.Scorer,
		ftoa(m.ColorFlickerP99), ftoa(m.ColorFlickerP999), r.Reference,
	}
}

func parseRow(col map[string]int, rec []string) (Row, error) {
	var errs []error
	get := func(name string) string {
		i, ok := col[name]
		if !ok || i >= len(rec) {
			errs = append(errs, fmt.Errorf("missing column %q", name))
			return ""
		}
		return rec[i]
	}
	float := func(name string) float64 {
		f, err := strconv.ParseFloat(get(name), 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
		return f
	}
	integer := func(name string) int64 {
		n, err := strconv.ParseInt(get(name), 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
		return n
	}

	r := Row{
		Sample:      get("sample"),
		Encoder:     get("encoder"),
		Flagset:     get("flagset"),
		Args:        get("args"),
		QualityFlag: get("quality_flag"),
		Quality:     float("quality"),
		Key:         get("key"),
		FlagsetKey:  get("flagset_key"),
		Bytes:       integer("bytes"),
		Kbps:        float("kbps"),
		WallS:       float("wall_s"),
		UserS:       float("user_s"),
		SysS:        float("sys_s"),
		MaxRSSKB:    integer("max_rss_kb"),
		Metrics: Metrics{
			VMAF:      float("vmaf"),
			VMAFHMean: float("vmaf_hmean"),
			VMAFP1:    float("vmaf_p1"),
			VMAFNeg:   float("vmaf_neg"),
			PSNRY:     float("psnr_y"),
			PSNRCb:    float("psnr_cb"),
			PSNRCr:    float("psnr_cr"),
			CIEDE2000: float("ciede2000"),
		},
		FFmpegVersion: get("ffmpeg_version"),
		Host:          get("host"),
	}
	t, err := time.Parse(time.RFC3339, get("time"))
	if err != nil {
		errs = append(errs, fmt.Errorf("time: %w", err))
	}
	r.Time = t
	// Columns added later are absent from older rows.
	for name, dst := range map[string]*float64{
		"color_shift_p99":    &r.Metrics.ColorShiftP99,
		"color_shift_p999":   &r.Metrics.ColorShiftP999,
		"color_flicker_p99":  &r.Metrics.ColorFlickerP99,
		"color_flicker_p999": &r.Metrics.ColorFlickerP999,
	} {
		*dst = math.NaN()
		if i, ok := col[name]; ok && i < len(rec) {
			*dst = float(name)
		}
	}
	r.Scorer = "v2"
	if i, ok := col["scorer"]; ok && i < len(rec) {
		r.Scorer = get("scorer")
	}
	r.Reference = r.Sample
	if i, ok := col["reference"]; ok && i < len(rec) && rec[i] != "" {
		r.Reference = get("reference")
	}
	return r, errors.Join(errs...)
}

// Results is the append-only log of every encode, which doubles as the
// cache: an encode whose Key is already logged is never repeated.
type Results struct {
	path  string
	Rows  []Row
	byKey map[string]int
}

// LoadResults reads the log at path; a missing file is an empty log.
//
// A log written before columns were added is upgraded: its header is a
// prefix of the current columns, older rows lack the new fields (they
// read as NaN), and rows appended since carry them past the old header.
// The file is then rewritten with the current header, so later appends
// line up.
func LoadResults(path string) (*Results, error) {
	r := &Results{path: path, byKey: map[string]int{}}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cr := csv.NewReader(f)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err == io.EOF {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	asWritten := slices.Clone(header)
	for i, h := range header {
		if n, ok := renamedColumns[h]; ok {
			header[i] = n
		}
	}
	col := columnIndex(header)
	current := columnIndex(resultColumns)
	upgradable := len(header) <= len(resultColumns) && slices.Equal(header, resultColumns[:len(header)])
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		cols := col
		if len(rec) > len(header) && upgradable {
			cols = current
		}
		row, err := parseRow(cols, rec)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		r.add(row)
	}
	if !slices.Equal(asWritten, resultColumns) {
		if !upgradable {
			return nil, fmt.Errorf("%s: unrecognized columns %q", path, header)
		}
		if err := r.rewrite(); err != nil {
			return nil, fmt.Errorf("upgrading %s: %w", path, err)
		}
	}
	return r, nil
}

// renamedColumns maps old column names to current ones.
var renamedColumns = map[string]string{
	"colour_shift_p99":  "color_shift_p99",
	"colour_shift_p999": "color_shift_p999",
}

func columnIndex(names []string) map[string]int {
	m := map[string]int{}
	for i, n := range names {
		m[n] = i
	}
	return m
}

// rewrite writes the whole log with the current columns, atomically.
func (r *Results) rewrite() error {
	tmp := r.path + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	w.Write(resultColumns)
	for i := range r.Rows {
		w.Write(r.Rows[i].record())
	}
	w.Flush()
	if err := errors.Join(w.Error(), f.Close()); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, r.path)
}

func (r *Results) add(row Row) {
	r.byKey[row.Key] = len(r.Rows)
	r.Rows = append(r.Rows, row)
}

// Lookup returns the logged encode with key, if any.
func (r *Results) Lookup(key string) (Row, bool) {
	i, ok := r.byKey[key]
	if !ok {
		return Row{}, false
	}
	return r.Rows[i], true
}

// Append adds row to the log, on disk first so a crash never loses an
// encode that the in-memory log claims to have.
func (r *Results) Append(row Row) error {
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w := csv.NewWriter(f)
	if st.Size() == 0 {
		w.Write(resultColumns)
	}
	w.Write(row.record())
	w.Flush()
	if err := errors.Join(w.Error(), f.Close()); err != nil {
		return fmt.Errorf("appending to %s: %w", r.path, err)
	}
	r.add(row)
	return nil
}

func hashKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s\n", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// flagsetKey identifies a flagset by what it does (its arguments and
// quality flag), so renaming it doesn't invalidate its results.
func flagsetKey(f *Flagset) string {
	return hashKey(append([]string{"flagset/v1", f.Quality.Flag}, f.Args...)...)
}

// encodeKey identifies one encode. The scoring method is versioned so
// that changing how scores are computed reruns everything.
func encodeKey(sampleID, fkey string, quality float64) string {
	return hashKey("encode/v1", "score/"+scorer, sampleID, fkey, ftoa(quality))
}

// joinArgs renders arguments for the log; it is for reading, not for
// pasting into a shell.
func joinArgs(args []string) string { return strings.Join(args, " ") }
