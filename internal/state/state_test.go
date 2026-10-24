package state

import (
	"os"
	"path/filepath"
	"testing"
)

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestExplain(t *testing.T) {
	m := &Manifest{Entries: map[string]Entry{
		"main/av1-1080": {Key: "k1", Source: "s1"},
	}}
	tests := []struct {
		name   string
		slot   string
		key    string
		source string
		exists bool
		want   string
	}{
		{"exists", "main/av1-1080", "k2", "s2", true, UpToDate},
		{"never built", "main/av1-2160", "k1", "s1", false, New},
		{"same key, file gone", "main/av1-1080", "k1", "s1", false, OutputMissing},
		{"source changed", "main/av1-1080", "k2", "s2", false, SourceChanged},
		{"settings changed", "main/av1-1080", "k2", "s1", false, SettingsChange},
	}
	for _, tt := range tests {
		if got := m.Explain(tt.slot, tt.key, tt.source, tt.exists); got != tt.want {
			t.Errorf("%s: Explain() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestRecordPruneAndReload(t *testing.T) {
	dir := t.TempDir()
	touch := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := Load(dir, "v")
	if err != nil || len(m.Entries) != 0 {
		t.Fatalf("Load(empty) = %+v, %v", m, err)
	}

	touch("a.k1.mp4")
	touch("b.k1.mp4")
	for slot, out := range map[string]string{"a": "a.k1.mp4", "b": "b.k1.mp4"} {
		if err := m.Record(slot, Entry{Key: "k1", Output: out}); err != nil {
			t.Fatal(err)
		}
	}

	// Rebuilding slot a with a new key deletes the old file.
	touch("a.k2.mp4")
	if err := m.Record("a", Entry{Key: "k2", Output: "a.k2.mp4"}); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, "a.k1.mp4")) || !exists(filepath.Join(dir, "a.k2.mp4")) {
		t.Error("Record didn't replace a.k1.mp4 with a.k2.mp4")
	}

	// Pruning to just a deletes b.
	if err := m.Prune([]string{"a"}); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, "b.k1.mp4")) {
		t.Error("Prune left b.k1.mp4")
	}
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}

	again, err := Load(dir, "v")
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Entries) != 1 || again.Entries["a"].Key != "k2" {
		t.Errorf("reloaded entries = %+v", again.Entries)
	}
}

func TestSetStream(t *testing.T) {
	stream := t.TempDir()
	for _, f := range []string{"a.mp4", "b.mp4", "other-video.mp4"} {
		if err := os.WriteFile(filepath.Join(stream, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manifest{Stream: []string{"a.mp4", "b.mp4", "gone.mp4"}}
	if err := m.SetStream(stream, []string{"c.mp4", "a.mp4"}); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(stream, "b.mp4")) {
		t.Error("b.mp4, dropped from the stream, wasn't deleted")
	}
	if !exists(filepath.Join(stream, "a.mp4")) || !exists(filepath.Join(stream, "other-video.mp4")) {
		t.Error("deleted a file still in the stream, or one this video never wrote")
	}
	if got := m.Stream; len(got) != 2 || got[0] != "a.mp4" || got[1] != "c.mp4" {
		t.Errorf("Stream = %v, want [a.mp4 c.mp4]", got)
	}
}
