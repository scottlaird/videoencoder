package bench

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const ladderFlagsets = `
  h264: {args: [-c:v, libx264, -preset, slow]}
  h265: {args: [-c:v, libx265, -preset, slow]}
`

const ladderSpec = `ladders:
  full:
    rungs:
      - {flagset: h264, height: 1080}
      - {flagset: h265, height: 1080}
      - {flagset: h265, height: 2160}
`

// ladderBench sets up an 8K sample and a 1080p one (see sizedProber),
// scaled to 1080p and 2160p, with ladderFlagsets and ladderSpec plus
// extra config.
func ladderBench(t *testing.T, extra string) (*Bench, *fakeFFmpeg, string) {
	t.Helper()
	return ladderBenchSpec(t, extra, ladderSpec)
}

// ladderBenchSpec is ladderBench with spec in place of ladderSpec.
func ladderBenchSpec(t *testing.T, extra, spec string) (*Bench, *fakeFFmpeg, string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "samples", "big.mov"), "8k")
	writeFile(t, filepath.Join(dir, "samples", "hd.mov"), "1080p")
	path := filepath.Join(dir, "bench.yaml")
	writeFile(t, path, "samples: samples\nworkdir: work\nscales: [1080, 2160]\n"+extra+"flagsets:\n"+ladderFlagsets+spec)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	fake := newFake(18.7)
	return &Bench{Cfg: cfg, Exec: fake, Prober: sizedProber{}}, fake, dir
}

func TestBenchLadder(t *testing.T) {
	ctx := context.Background()
	b, fake, dir := ladderBench(t, "")
	// The searches run first, so the ladder takes this log's answers.
	if err := b.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// Every search encodes alone; the ladders are one ffmpeg each: three
	// rungs on the 8K sample, and two on the 1080p one, which is too
	// short for the 2160p rung.
	var ladderRuns []int
	for _, n := range fake.runs {
		if n > 1 {
			ladderRuns = append(ladderRuns, n)
		}
	}
	if want := []int{3, 2}; !slices.Equal(ladderRuns, want) {
		t.Errorf("ladder ffmpegs made %v outputs, want %v (all runs %v)", ladderRuns, want, fake.runs)
	}

	results, err := LoadResults(filepath.Join(dir, "work", "results.csv"))
	if err != nil {
		t.Fatal(err)
	}
	answers := bestByPair(b.Cfg, results, b.scorings)
	batches := map[string][]string{}
	for _, r := range results.Rows {
		if r.Ladder == "" {
			continue
		}
		if r.Ladder != "full" || r.LadderKey == "" {
			t.Errorf("ladder row %s %s: ladder %q, key %q", r.Sample, r.Flagset, r.Ladder, r.LadderKey)
		}
		if r.Timed || r.BatchID == "" {
			t.Errorf("ladder row %s %s: timed %v, batch %q; want untimed, batched", r.Sample, r.Flagset, r.Timed, r.BatchID)
		}
		// A rung is encoded at the search's answer for its flagset on
		// the sample's version at its height, and scored against that
		// version.
		answer, ok := answers[pair{r.Sample, flagsetKey(b.Cfg.Flagsets[r.Flagset])}]
		if !ok || r.Quality != answer.Quality {
			t.Errorf("ladder row %s %s at %v; answer %v (%v)", r.Sample, r.Flagset, r.Quality, answer.Quality, ok)
		}
		if r.Reference != r.Sample || r.Source == "" || r.Pass == nil || !*r.Pass {
			t.Errorf("ladder row %s %s: reference %q, source %q, pass %v", r.Sample, r.Flagset, r.Reference, r.Source, r.Pass)
		}
		batches[r.BatchID] = append(batches[r.BatchID], r.Sample+" "+r.Flagset)
	}
	var got []string
	for _, rungs := range batches {
		slices.Sort(rungs)
		got = append(got, strings.Join(rungs, ", "))
	}
	slices.Sort(got)
	want := []string{
		"big@1080p.mov h264, big@1080p.mov h265, big@2160p.mov h265",
		"hd.mov h264, hd.mov h265",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ladder rows by run:\n%q\nwant\n%q", got, want)
	}

	// The rungs don't count as answers or searches.
	if n := len(Bests(b.Cfg, results, b.scorings)); n != 6 {
		t.Errorf("%d answers, want 6: 2 flagsets on big's 2 versions and hd", n)
	}

	// Done: nothing runs again, and Status says so, with each sample's
	// ladder after its flagsets.
	n := len(fake.encodes)
	if err := b.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.encodes) != n {
		t.Errorf("second run encoded %q", fake.encodes[n:])
	}
	status, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pairs []string
	for _, s := range status {
		pairs = append(pairs, s.Sample+" "+s.Flagset)
		if !s.Done {
			t.Errorf("Status: %s %s not done", s.Sample, s.Flagset)
		}
	}
	if want := []string{"big.mov h264", "big.mov h265", "big.mov full", "hd.mov h264", "hd.mov h265", "hd.mov full"}; !slices.Equal(pairs, want) {
		t.Errorf("Status pairs %q, want %q", pairs, want)
	}
}

func TestBenchLadderNeedsAnswers(t *testing.T) {
	ctx := context.Background()
	b, fake, dir := ladderBench(t, "")
	// Only the ladder, before any search has answered.
	b.FlagsetFilter = "full"
	status, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 2 || status[0].Done || status[0].Skipped {
		t.Errorf("Status = %+v, want two pairs, not done", status)
	}
	err = b.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "no answer for") {
		t.Errorf("Run = %v, want a missing answer", err)
	}
	if len(fake.encodes) != 0 {
		t.Errorf("encoded %q without answers", fake.encodes)
	}
	if _, err := os.Stat(filepath.Join(dir, "work", "failures.csv")); err != nil {
		t.Errorf("failure not logged: %v", err)
	}
}

func TestBenchLadderAnswersFromSeeds(t *testing.T) {
	ctx := context.Background()
	src, _, srcDir := ladderBench(t, "")
	if err := src.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// Another machine's log answers; this one searches nothing.
	seeds := filepath.Join(srcDir, "work", "results.csv")
	b, fake, _ := ladderBench(t, "seed_results: "+seeds+"\n")
	b.FlagsetFilter = "full"
	if err := b.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if want := []int{3, 2}; !slices.Equal(fake.runs, want) {
		t.Errorf("ffmpegs made %v outputs, want only the ladders' %v", fake.runs, want)
	}
}

// TestBenchLadderQualityFrom checks rungs that take their setting from
// another height's answer plus an offset, kept within the flagset's range.
func TestBenchLadderQualityFrom(t *testing.T) {
	ctx := context.Background()
	b, _, dir := ladderBenchSpec(t, "", `ladders:
  full:
    rungs:
      - {flagset: h264, height: 1080}
      - {flagset: h265, height: 2160, quality_from: 1080, quality_offset: 3}
      - {flagset: h264, height: 2160, quality_from: 1080, quality_offset: 100}
`)
	if err := b.Run(ctx); err != nil {
		t.Fatal(err)
	}
	results, err := LoadResults(filepath.Join(dir, "work", "results.csv"))
	if err != nil {
		t.Fatal(err)
	}
	answers := bestByPair(b.Cfg, results, b.scorings)
	answer := func(sample, flagset string) float64 {
		t.Helper()
		row, ok := answers[pair{sample, flagsetKey(b.Cfg.Flagsets[flagset])}]
		if !ok {
			t.Fatalf("no answer for %s on %s", flagset, sample)
		}
		return row.Quality
	}
	got := map[string]float64{}
	for _, r := range results.Rows {
		if r.Ladder == "full" && strings.HasPrefix(r.Sample, "big") {
			got[r.Sample+" "+r.Flagset] = r.Quality
		}
	}
	h265 := b.Cfg.Flagsets["h265"].Quality
	h264 := b.Cfg.Flagsets["h264"].Quality
	want := map[string]float64{
		"big@1080p.mov h264": answer("big@1080p.mov", "h264"),
		"big@2160p.mov h265": min(answer("big@1080p.mov", "h265")+3, h265.Max),
		"big@2160p.mov h264": h264.Max, // the offset runs past the range
	}
	if len(got) != len(want) {
		t.Fatalf("ladder rows %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s at %v, want %v", k, got[k], w)
		}
	}
	// The 2160p rungs' answers aren't what they encode at.
	if got["big@2160p.mov h265"] == answer("big@2160p.mov", "h265") {
		t.Errorf("h265 at 2160 encoded at its own answer, %v", got["big@2160p.mov h265"])
	}
}

func TestLadderArgv(t *testing.T) {
	x264 := &Flagset{Args: []string{"-c:v", "libx264"}, Quality: Quality{Flag: "-crf"}}
	x265 := &Flagset{Args: []string{"-c:v", "libx265"}, Quality: Quality{Flag: "-crf"}}
	tests := []struct {
		name   string
		height uint64
		rungs  []*ladderRung
		graph  string
	}{
		{"two heights", 4320, []*ladderRung{
			{fs: x264, height: 1080, quality: 18.8},
			{fs: x265, height: 1080, quality: 19.5},
			{fs: x265, height: 2160, quality: 18},
		}, "[0:v:0]split=2[h0][h1];[h0]scale=1920:1080:flags=lanczos,split=2[o0][o1];[h1]scale=3840:2160:flags=lanczos[o2]"},
		{"native height only", 1080, []*ladderRung{
			{fs: x264, height: 1080, quality: 22},
			{fs: x265, height: 1080, quality: 21.4},
		}, "[0:v:0]split=2[o0][o1]"},
		{"one rung", 1080, []*ladderRung{{fs: x264, height: 1080, quality: 22}}, "[0:v:0]null[o0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lr := &ladderRun{path: "s.mov", width: tt.height * 16 / 9, height: tt.height, rungs: tt.rungs}
			outs := make([]string, len(tt.rungs))
			for i := range outs {
				outs[i] = string(rune('a'+i)) + ".mp4"
			}
			argv := ladderArgv(lr, outs)
			if g := argv[slices.Index(argv, "-filter_complex")+1]; g != tt.graph {
				t.Errorf("graph %q\nwant %q", g, tt.graph)
			}
			if n := strings.Count(strings.Join(argv, " "), " -i "); n != 1 {
				t.Errorf("%d inputs, want 1: %q", n, argv)
			}
			// Each output: its label, its flagset, its setting, its file.
			for i, r := range tt.rungs {
				at := slices.Index(argv, outs[i])
				m := slices.Index(argv, "[o"+string(rune('0'+i))+"]")
				if at < 0 || m < 0 || m > at || argv[m-1] != "-map" {
					t.Fatalf("output %d not mapped from its label: %q", i, argv)
				}
				out := argv[m:at]
				if !slices.Contains(out, r.fs.Args[1]) || !slices.Contains(out, ftoa(r.quality)) {
					t.Errorf("output %d args %q lack %s at %v", i, out, r.fs.Args[1], r.quality)
				}
			}
		})
	}
}

func TestLadderConfig(t *testing.T) {
	tests := []struct {
		name, flagsets, ladders, want string
	}{
		{"unknown flagset", ladderFlagsets, "ladders:\n  l: {rungs: [{flagset: nope, height: 1080}]}\n", `no flagset "nope"`},
		{"height not a scale", ladderFlagsets, "ladders:\n  l: {rungs: [{flagset: h264, height: 720}]}\n", "isn't one of scales"},
		{"no rungs", ladderFlagsets, "ladders:\n  l: {}\n", "rungs required"},
		{"same name as a flagset", ladderFlagsets, "ladders:\n  h264: {rungs: [{flagset: h264, height: 1080}]}\n", "same name"},
		{"repeated rung", ladderFlagsets, "ladders:\n  l: {rungs: [{flagset: h264, height: 1080}, {flagset: h264, height: 1080}]}\n", "already a rung"},
		{"quality_from not a scale", ladderFlagsets, "ladders:\n  l: {rungs: [{flagset: h264, height: 2160, quality_from: 720}]}\n", "quality_from 720 isn't one of scales"},
		{"offset without quality_from", ladderFlagsets, "ladders:\n  l: {rungs: [{flagset: h264, height: 2160, quality_offset: 2}]}\n", "quality_offset needs quality_from"},
		{"offset on a log scale", ladderFlagsets + "  vbr: {args: [-c:v, libx264], quality: {flag: -b:v, min: 1e6, max: 1e8, step: 0.05, scale: log}}\n",
			"ladders:\n  l: {rungs: [{flagset: vbr, height: 2160, quality_from: 1080, quality_offset: 2}]}\n", "needs a linear quality scale"},
		{"repeated rung with an offset", ladderFlagsets, "ladders:\n  l: {rungs: [{flagset: h264, height: 2160}, {flagset: h264, height: 2160, quality_from: 1080, quality_offset: 1}]}\n", "already a rung"},
		{"different input args", ladderFlagsets + "  hw: {args: [-c:v, libx264], input_args: [-hwaccel, videotoolbox]}\n",
			"ladders:\n  l: {rungs: [{flagset: h264, height: 1080}, {flagset: hw, height: 1080}]}\n", "input_args differ"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bench.yaml")
			writeFile(t, path, "samples: s\nscales: [1080, 2160]\nflagsets:\n"+tt.flagsets+tt.ladders)
			_, err := LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("LoadConfig = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestLadderRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.csv")
	r, err := LoadResults(path)
	if err != nil {
		t.Fatal(err)
	}
	row := Row{Sample: "a.mov", Flagset: "f", Key: "k", Ladder: "full", LadderKey: "0123456789abcdef"}
	if err := r.Append(row); err != nil {
		t.Fatal(err)
	}
	again, err := LoadResults(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := again.Lookup("k"); got.Ladder != row.Ladder || got.LadderKey != row.LadderKey {
		t.Errorf("reloaded ladder %q, key %q; want %q, %q", got.Ladder, got.LadderKey, row.Ladder, row.LadderKey)
	}
}
