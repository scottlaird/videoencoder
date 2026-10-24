package fileid

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestContent(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	big := bytes.Repeat([]byte("abcdefgh"), 3*chunk/8)
	tail := bytes.Clone(big)
	tail[len(tail)-1] = 'X'
	middle := bytes.Clone(big)
	middle[len(middle)/2] = 'X'

	id := func(p string) string {
		t.Helper()
		s, err := Content(p)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := id(write("a", big)), id(write("b", big))
	if a != b {
		t.Errorf("identical files: %s != %s", a, b)
	}
	if id(write("tail", tail)) == a {
		t.Error("changing the last byte didn't change the ID")
	}
	if id(write("small", []byte("x"))) == id(write("small2", []byte("y"))) {
		t.Error("different small files have the same ID")
	}
	if id(write("middle", middle)) != a {
		t.Error("an edit in the middle changed the ID; the contract says it may not, and the cost model relies on not reading it")
	}
	if _, err := Content(filepath.Join(dir, "missing")); err == nil {
		t.Error("Content(missing) = nil error")
	}
}
