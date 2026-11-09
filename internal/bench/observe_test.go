package bench

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

func TestStatus(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "samples", "big.mov"), "8k hdr")
	writeFile(t, filepath.Join(dir, "samples", "hd.mov"), "1080p sdr")
	c := benchConfig(t, dir, "  x: {args: [-c:v, libx265]}\n  h264: {args: [-c:v, libx264], sdr_only: true}\n")
	c.Scales = []uint64{1080, 2160}
	ctx := context.Background()

	str := func(ps []PairStatus) []string {
		var out []string
		for _, p := range ps {
			out = append(out, fmt.Sprintf("%s %s done=%v skipped=%v", p.Sample, p.Flagset, p.Done, p.Skipped))
		}
		return out
	}
	b := &Bench{Cfg: c, Exec: newFake(18.7), Prober: sizedProber{}}
	got, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"big.mov h264 done=false skipped=true",
		"big.mov x done=false skipped=false",
		"hd.mov h264 done=false skipped=false",
		"hd.mov x done=false skipped=false",
	}
	if !slices.Equal(str(got), want) {
		t.Errorf("Status() before running = %q, want %q", str(got), want)
	}

	// One pair run: only it is done, though big.mov has two scales.
	var events []Event
	run := &Bench{Cfg: c, Exec: newFake(18.7), Prober: sizedProber{}, SampleFilter: "big.mov", FlagsetFilter: "x",
		Observe: func(e Event) { events = append(events, e) }}
	if err := run.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want[1] = "big.mov x done=true skipped=false"
	if !slices.Equal(str(got), want) {
		t.Errorf("Status() after running big.mov x = %q, want %q", str(got), want)
	}

	kinds := map[EventKind]int{}
	finished := map[string]Event{}
	for _, e := range events {
		kinds[e.Kind]++
		if e.Source != "big.mov" || e.Flagset != "x" {
			t.Errorf("event for %s/%s %s, want only big.mov x", e.Source, e.Sample, e.Flagset)
		}
		if e.Kind == Finished {
			finished[e.Sample] = e
		}
	}
	if kinds[Encoding] == 0 || kinds[Scored] != kinds[Encoding] {
		t.Errorf("%d Encoding and %d Scored events, want the same nonzero count", kinds[Encoding], kinds[Scored])
	}
	for _, s := range []string{"big@1080p.mov", "big@2160p.mov"} {
		e, ok := finished[s]
		if !ok {
			t.Errorf("no Finished event for %s", s)
			continue
		}
		if e.Quality != 18.7 || e.Outcome != Found || !e.Pass || e.VMAF < 95 {
			t.Errorf("Finished %s = %+v, want 18.7 found with its VMAF", s, e)
		}
	}
}
