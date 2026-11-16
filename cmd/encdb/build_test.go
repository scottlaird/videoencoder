package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"

	"github.com/scottlaird/videoencoder/internal/bench"
	"github.com/scottlaird/videoencoder/internal/benchweb"
)

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func answer(host, sample, flagset, sha string, quality, vmaf float64, wall float64, from string) row {
	return row{benchweb.ResultRow{
		Host: host, Sample: sample, Reference: sample, Encoder: "hevc_qsv", Flagset: flagset,
		Scorer: "v6", Quality: quality, VMAF: &vmaf, OutSHA256: sha, Bytes: 1000, WallS: wall, UserS: wall / 2,
		Time: t0,
	}, from}
}

func TestRetime(t *testing.T) {
	const old, fresh = "old.csv", "2dimm.csv"
	rows := []row{
		// medium's answer is re-timed directly; fast's file matches medium's.
		answer("render1", "a", "hevc-qsv-medium", "m", 25, 95.5, 30, old),
		answer("render1", "a", "hevc-qsv-fast", "m", 25, 95.5, 31, old),
		// A failing setting isn't an answer, and isn't re-timed.
		answer("render1", "a", "hevc-qsv-slow", "m", 26, 94, 33, old),
		// slower's file differs from every re-timed one.
		answer("render1", "a", "hevc-qsv-slower", "s", 25, 96, 40, old),
		// Another host's identical file is left alone.
		answer("render2", "a", "hevc-qsv-medium", "m", 25, 95.5, 50, old),
		// Hardware decoding makes the same file with different work, but
		// B-pyramid is a SameTime token.
		answer("render1", "a", "hevc-qsv-medium-hwdec", "m", 25, 95.5, 15, old),
		answer("render1", "a", "hevc-qsv-medium-bpyr", "m", 25, 95.5, 32, old),
		answer("render1", "a", "hevc-qsv-medium", "m", 25, 95.5, 20, fresh),
	}
	if got, want := retime(rows, Retime{Host: "render1", Log: fresh, SameTime: []string{"bpyr"}, Families: []string{"Quick Sync"}}), 3; got != want {
		t.Errorf("retime changed %d rows, want %d", got, want)
	}
	for i, want := range []float64{20, 20, 33, 40, 50, 15, 20, 20} {
		if got := rows[i].WallS; got != want {
			t.Errorf("rows[%d] (%s %s) wall_s = %v, want %v", i, rows[i].Host, rows[i].Flagset, got, want)
		}
	}
	if got, want := rows[1].From, "old.csv (times: 2dimm.csv)"; got != want {
		t.Errorf("rows[1].From = %q, want %q", got, want)
	}
}

// TestBuild runs build against a fake encbench server and reads the
// table back.
func TestBuild(t *testing.T) {
	vmaf, height := 95.2, int64(2160)
	r := benchweb.ResultRow{
		Host: "render2", Sample: "20180529-band.mp4", Reference: "20180529-band.mp4",
		Encoder: "libx265", Flagset: "x265-slow-10bit-kf10", Args: "-preset slow -pix_fmt yuv420p10le",
		Scorer: "v6", Quality: 24, VMAF: &vmaf, Bytes: 12345, Height: &height, Time: t0,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/results" {
			http.NotFound(w, req)
			return
		}
		// The same row twice is one encode.
		json.NewEncoder(w).Encode(benchweb.Results{Format: benchweb.ResultsFormat, Rows: []benchweb.ResultRow{r, r}})
	}))
	defer srv.Close()

	dir := t.TempDir()
	config := filepath.Join(dir, "encdb.yml")
	os.WriteFile(config, []byte(`bench: .
hosts: [`+srv.URL+`]
machines:
  render2: {machine: render2, hardware: Ryzen 9 9955HX, cpu: Ryzen 9 9955HX}
standard:
  4k: {scorer: v6, samples: [20180529-band.mp4]}
`), 0o644)
	out := filepath.Join(dir, "encodes.parquet")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := build(context.Background(), config, out, log); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parquet.Read[Encode](bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d rows, want 1", len(got))
	}
	e := got[0]
	for _, c := range []struct{ name, got, want string }{
		{"machine", deref(e.Machine), "render2"},
		{"device", deref(e.Device), "Ryzen 9 9955HX"},
		{"family", e.Family, "x265"},
		{"preset", e.Preset, "slow"},
		{"clip", e.Clip, "band"},
		{"res", deref(e.Res), "4k"},
		{"codec", e.Codec, "hevc"},
		{"log", e.Log, "api:127.0.0.1"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if !e.StdClip || !e.StdScorer {
		t.Errorf("std_clip, std_scorer = %v, %v, want true, true", e.StdClip, e.StdScorer)
	}
	if e.OutBitDepth == nil || *e.OutBitDepth != 10 {
		t.Errorf("out_bit_depth = %v, want 10", e.OutBitDepth)
	}
	if e.OutSHA256 != nil {
		t.Errorf("out_sha256 = %q, want null", *e.OutSHA256)
	}
	if !e.Time.Equal(t0) {
		t.Errorf("time = %v, want %v", e.Time, t0)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestNormalizeLog(t *testing.T) {
	// Another tool's column order, and an empty vmaf_neg.
	in := "reference,sample,quality,vmaf_neg\nb.mov,a.mov,24,\n"
	got, n, err := normalizeLog([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	cols := strings.Split(lines[0], ",")
	vals := strings.Split(lines[1], ",")
	if want := bench.ResultColumns()[:len(cols)]; !slices.Equal(cols, want) {
		t.Errorf("header = %q, want %q", cols, want)
	}
	field := func(name string) string { return vals[slices.Index(cols, name)] }
	for name, want := range map[string]string{"sample": "a.mov", "reference": "b.mov", "quality": "24", "vmaf_neg": "NaN", "kbps": "NaN", "args": ""} {
		if got := field(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// Every required number but quality: the copy has every column up to
	// reference, and they're all before it.
	if want := len(requiredFloats) - 1; n != want {
		t.Errorf("filled %d numbers, want %d", n, want)
	}
	if _, _, err := normalizeLog([]byte("sample,bogus\na,b\n")); err == nil {
		t.Error("normalizeLog accepted an unknown column")
	}
}
