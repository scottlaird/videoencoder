package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/scottlaird/videoencoder/internal/bench"
	"github.com/scottlaird/videoencoder/internal/benchweb"
)

// row is one logged encode and the log it was read from.
type row struct {
	benchweb.ResultRow
	From string // the log column: a Config.Logs path or "api:<host>"
}

// dedupKey identifies a logged encode across logs: a row copied into
// another log, or served by the API from the log it's in, is the same
// encode.
type dedupKey struct {
	host, sample, reference, flagset, scorer, vmafModel string
	time                                                int64
	quality                                             float64
	bytes                                               int64
}

func keyOf(r *benchweb.ResultRow) dedupKey {
	return dedupKey{r.Host, r.Sample, r.Reference, r.Flagset, r.Scorer, r.VMAFModel, r.Time.UnixMicro(), r.Quality, r.Bytes}
}

// gather reads every log and host in c's order, keeping each encode
// once, from the first source that has it. A missing log or an
// unreachable host is reported to log and skipped.
func gather(ctx context.Context, c *Config, log *slog.Logger) []row {
	seen := map[dedupKey]bool{}
	var rows []row
	add := func(from string, rs []benchweb.ResultRow) {
		n := 0
		for _, r := range rs {
			k := keyOf(&r)
			if seen[k] {
				continue
			}
			seen[k] = true
			rows = append(rows, row{r, from})
			n++
		}
		log.Info("read", "source", from, "rows", len(rs), "new", n)
	}
	for _, l := range c.Logs {
		path := c.path(l)
		if _, err := os.Stat(path); err != nil {
			log.Warn("skipping log", "err", err)
			continue
		}
		res, err := readLog(path, log)
		if err != nil {
			log.Warn("skipping log", "err", err)
			continue
		}
		rs := make([]benchweb.ResultRow, len(res.Rows))
		for i := range res.Rows {
			rs[i] = benchweb.ResultRowOf(0, &res.Rows[i])
		}
		add(l, rs)
	}
	for _, h := range c.Hosts {
		rs, err := fetchResults(ctx, h)
		if err != nil {
			log.Warn("skipping host", "host", h, "err", err)
			continue
		}
		add(hostLabel(h), rs)
	}
	return rows
}

// requiredFloats are the columns encbench's reader needs a number in.
// encbench writes "NaN" for a metric it lacks; logs rewritten by other
// tools sometimes leave them empty.
var requiredFloats = []string{
	"quality", "kbps", "wall_s", "user_s", "sys_s",
	"vmaf", "vmaf_hmean", "vmaf_p1", "vmaf_neg", "psnr_y", "psnr_cb", "psnr_cr", "ciede2000",
}

// readLog reads a results log with bench.ReadResults, which never
// rewrites it. If that fails, it tries again on a copy put back into
// encbench's form: columns in encbench's order (logs rewritten by other
// tools may have their own), and empty requiredFloats as NaN.
func readLog(path string, log *slog.Logger) (*bench.Results, error) {
	res, err := bench.ReadResults(path)
	if err == nil {
		return res, nil
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		return nil, err
	}
	fixed, n, ferr := normalizeLog(data)
	if ferr != nil {
		return nil, fmt.Errorf("%w (and as rewritten: %v)", err, ferr)
	}
	tmp, terr := os.CreateTemp("", "encdb-*.csv")
	if terr != nil {
		return nil, terr
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(fixed)
	if werr = cmp.Or(werr, tmp.Close()); werr != nil {
		return nil, werr
	}
	res, err2 := bench.ReadResults(tmp.Name())
	if err2 != nil {
		return nil, fmt.Errorf("%w (and as rewritten: %v)", err, err2)
	}
	log.Info("read in encbench's form", "log", path, "empty numbers", n)
	return res, nil
}

// normalizeLog rewrites a results log's CSV with encbench's columns, in
// its order, up to the last one data has, with empty requiredFloats as
// NaN, and returns it with how many it filled. Columns encbench doesn't
// know are an error.
func normalizeLog(data []byte) ([]byte, int, error) {
	cr := csv.NewReader(bytes.NewReader(data))
	cr.FieldsPerRecord = -1
	recs, err := cr.ReadAll()
	if err != nil {
		return nil, 0, err
	}
	if len(recs) == 0 {
		return data, 0, nil
	}
	cols := bench.ResultColumns()
	from := map[string]int{}
	last := -1
	for i, h := range recs[0] {
		j := slices.Index(cols, h)
		if j < 0 {
			return nil, 0, fmt.Errorf("unknown column %q", h)
		}
		from[h] = i
		last = max(last, j)
	}
	cols = cols[:last+1]
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.Write(cols)
	filled := 0
	out := make([]string, len(cols))
	for _, rec := range recs[1:] {
		for j, c := range cols {
			v := ""
			if i, ok := from[c]; ok && i < len(rec) {
				v = rec[i]
			}
			if v == "" && slices.Contains(requiredFloats, c) {
				v = "NaN"
				filled++
			}
			out[j] = v
		}
		w.Write(out)
	}
	w.Flush()
	return buf.Bytes(), filled, w.Error()
}

// hostLabel is the log column for a host's API: "api:render1".
func hostLabel(h string) string {
	if u, err := url.Parse(h); err == nil && u.Hostname() != "" {
		return "api:" + u.Hostname()
	}
	return "api:" + h
}

// fetchResults reads an encbench server's /api/results.
func fetchResults(ctx context.Context, base string) ([]benchweb.ResultRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/api/results", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var doc benchweb.Results
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	if doc.Format != benchweb.ResultsFormat {
		return nil, fmt.Errorf("results format %d, want %d", doc.Format, benchweb.ResultsFormat)
	}
	return doc.Rows, nil
}

// derive makes the table: each row with its derived columns, sorted by
// time. depths is the bit depth of each probed file stem.
func derive(c *Config, rows []row, depths map[string]int32) []Encode {
	// The tallest encode of each sample, for samples whose names don't
	// give their resolution.
	height := map[string]int64{}
	for _, r := range rows {
		if r.Height != nil {
			height[r.Sample] = max(height[r.Sample], *r.Height)
		}
	}
	type std struct{ res, scorer string }
	stds := map[string]std{}
	for res, s := range c.Standard {
		for _, sample := range s.Samples {
			stds[sample] = std{res, s.Scorer}
		}
	}
	attrs := map[[2]string]Attrs{}
	out := make([]Encode, len(rows))
	for i := range rows {
		r := &rows[i]
		// A flagset's attributes come from the first row of it, so that
		// a change to its args over time can't split it.
		ak := [2]string{r.Encoder, r.Flagset}
		a, ok := attrs[ak]
		if !ok {
			a = attributes(r.Encoder, r.Flagset, r.Args)
			attrs[ak] = a
		}
		m, known := c.Machines[r.Host]
		e := Encode{
			Machine:   opt(cmp.Or(m.Machine, r.Host)),
			Hardware:  opt(m.Hardware),
			Family:    a.Family,
			Software:  a.Software,
			Preset:    a.Preset,
			BitDepth:  a.BitDepth,
			Keyframes: a.Keyframes,
			Variant:   a.Variant,
			Clip:      clipOf(r.Sample),
			Codec:     codecOf(r.Encoder, r.OutCodec),
		}
		if known {
			e.Device = opt(m.GPU)
			if a.Software {
				e.Device = opt(m.CPU)
			}
		}
		src, inp := stems(r.Sample)
		e.SourceBitDepth = lookup(depths, src)
		e.InputBitDepth = lookup(depths, inp)
		switch {
		case a.BitDepth == "8":
			e.OutBitDepth = ptr(int32(8))
		case a.BitDepth == "10":
			e.OutBitDepth = ptr(int32(10))
		case r.OutPixFmt != "" && isTenBit(r.OutPixFmt):
			e.OutBitDepth = ptr(int32(10))
		case r.OutPixFmt != "":
			e.OutBitDepth = ptr(int32(8))
		default:
			e.OutBitDepth = e.InputBitDepth
		}
		s, isStd := stds[r.Sample]
		e.StdClip = isStd
		e.StdScorer = isStd && r.Scorer == s.scorer
		e.Res = opt(cmp.Or(s.res, resOf(r.Sample, height[r.Sample])))
		e.setRaw(&r.ResultRow)
		e.Log = r.From
		out[i] = e
	}
	slices.SortStableFunc(out, func(a, b Encode) int { return a.Time.Compare(b.Time) })
	return out
}

func lookup(m map[string]int32, k string) *int32 {
	if v, ok := m[k]; ok {
		return &v
	}
	return nil
}

// probeDepths is the bit depth of every video file in c.Probe's
// directories, by name without extension: 10 for a 10-bit pixel format,
// else 8. Files ffprobe can't read are left out.
func probeDepths(ctx context.Context, c *Config, probe func(context.Context, string) (string, error), log *slog.Logger) map[string]int32 {
	out := map[string]int32{}
	for _, d := range c.Probe {
		dir := c.path(d)
		ents, err := os.ReadDir(dir)
		if err != nil {
			log.Warn("skipping probe directory", "err", err)
			continue
		}
		for _, ent := range ents {
			if ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
				continue
			}
			pf, err := probe(ctx, filepath.Join(dir, ent.Name()))
			if err != nil {
				continue
			}
			depth := int32(8)
			if isTenBit(pf) {
				depth = 10
			}
			out[strings.TrimSuffix(ent.Name(), filepath.Ext(ent.Name()))] = depth
		}
	}
	return out
}
