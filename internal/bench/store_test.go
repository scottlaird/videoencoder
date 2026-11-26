package bench

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestKeepAll(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "samples", "a.mov"), "sample a")
	cfg := benchConfig(t, dir, "  x: {args: [-c:v, libx265], quality: {step: 0.5}}\n")
	cfg.Keep = KeepAll
	b := &Bench{Cfg: cfg, Exec: newFake(18.7), Prober: fakeProber{}}
	if err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")
	results, err := LoadResults(filepath.Join(work, "results.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(results.Rows) < 2 {
		t.Fatalf("got %d rows, want a search of several encodes", len(results.Rows))
	}
	for _, r := range results.Rows {
		if r.SourceID == "" || r.SourceID != r.SampleID {
			t.Errorf("%v: source_id %q, want the unscaled sample's id %q", r.Quality, r.SourceID, r.SampleID)
		}
		if got := fileSizeOf(ObjectPath(work, r.OutSHA256)); got != r.Bytes {
			t.Errorf("%v: object is %d bytes, want %d", r.Quality, got, r.Bytes)
		}
	}
	tmp, _ := os.ReadDir(filepath.Join(work, "tmp"))
	if len(tmp) != 0 {
		t.Errorf("%d files left in tmp, want none", len(tmp))
	}
	kept, _ := filepath.Glob(filepath.Join(work, "keep", "*", "*.mp4"))
	if len(kept) != 1 {
		t.Fatalf("kept %q, want one answer", kept)
	}
	keeper, err := os.Stat(kept[0])
	if err != nil {
		t.Fatal(err)
	}
	var best Row
	for _, r := range results.Rows {
		if obj, err := os.Stat(ObjectPath(work, r.OutSHA256)); err == nil && os.SameFile(keeper, obj) {
			best = r
		}
	}
	if best.Key == "" {
		t.Fatal("the keeper isn't a hard link to any encode's object")
	}
	if want := KeeperPath(filepath.Join(work, "keep"), "a.mov", "x", best.Quality); kept[0] != want {
		t.Errorf("keeper %s, want %s", kept[0], want)
	}

	sf, err := ReadSampleFiles(work)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := sf.Samples[best.SampleID]
	if !ok || e.Name != "a.mov" || e.Path != filepath.Join(dir, "samples", "a.mov") || e.Reference != "" || e.SourceID != best.SourceID {
		t.Errorf("sample-files entry = %+v (found %v), want a.mov at its path", e, ok)
	}
	if got := sf.Sources[best.SourceID]; got != filepath.Join(dir, "samples", "a.mov") {
		t.Errorf("source path = %q, want the sample", got)
	}
}

func TestKeepAnswerKeepsNoObjects(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "samples", "a.mov"), "sample a")
	b := &Bench{Cfg: benchConfig(t, dir, "  x: {args: [-c:v, libx265]}\n"), Exec: newFake(18.7), Prober: fakeProber{}}
	if b.Cfg.Keep != KeepAnswer {
		t.Errorf("default keep = %q, want %q", b.Cfg.Keep, KeepAnswer)
	}
	if err := b.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "work", "objects")); !os.IsNotExist(err) {
		t.Errorf("objects dir: %v, want none with keep: answer", err)
	}
	kept, _ := filepath.Glob(filepath.Join(dir, "work", "keep", "*", "*.mp4"))
	if len(kept) != 1 {
		t.Errorf("kept %d encodes, want 1", len(kept))
	}
}

func TestKeepConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bench.yaml")
	writeFile(t, path, "samples: samples\nkeep: some\nflagsets:\n  x: {args: [-c:v, libx265]}\n")
	if _, err := LoadConfig(path); err == nil {
		t.Error("keep: some loaded, want an error")
	}
}

func TestIDChecks(t *testing.T) {
	for _, c := range []struct {
		s          string
		sha, field bool
	}{
		{"0123456789abcdef", false, true},
		{"0123456789ABCDEF", false, false},
		{"0123456789abcde", false, false},
		{"../../etc/passwd", false, false},
		{"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", true, false},
		{"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdeg", false, false},
	} {
		if got := IsSHA256(c.s); got != c.sha {
			t.Errorf("IsSHA256(%q) = %v, want %v", c.s, got, c.sha)
		}
		if got := IsFileID(c.s); got != c.field {
			t.Errorf("IsFileID(%q) = %v, want %v", c.s, got, c.field)
		}
	}
}

func fileSizeOf(p string) int64 {
	info, err := os.Stat(p)
	if err != nil {
		return -1
	}
	return info.Size()
}

func TestScoringsRecordsSampleFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "samples", "a.mov"), "sample a")
	writeFile(t, filepath.Join(dir, "samples", "b.mov"), "sample b")
	b := &Bench{Cfg: benchConfig(t, dir, "  x: {args: [-c:v, libx265]}\n"), Exec: newFake(18.7), Prober: fakeProber{}}
	if _, err := b.Scorings(context.Background()); err != nil {
		t.Fatal(err)
	}
	sf, err := ReadSampleFiles(b.Cfg.Workdir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sf.Samples) != 2 || len(sf.Sources) != 2 {
		t.Errorf("recorded %d samples and %d sources, want both of each from a listing", len(sf.Samples), len(sf.Sources))
	}
}
