package benchweb

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/scottlaird/videoencoder/internal/bench"
)

func shaOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func get(t *testing.T, url string, header map[string]string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

// fileQueue serves a queue whose workdir has an object, a keeper named
// by the results log, a keeper whose file doesn't match the log, and
// sample files including two that point outside the queue.
func fileQueue(t *testing.T) (srv *Server, url string, files map[string]string) {
	t.Helper()
	srv, hs, _, _ := start(t)
	base := filepath.Dir(srv.Runner.Dir)
	work, samples := filepath.Join(base, "bench"), filepath.Join(base, "samples")
	files = map[string]string{"object": "object bytes", "keeper": "keeper bytes", "stale": "stale"}

	write(t, bench.ObjectPath(work, shaOf(files["object"])), files["object"])
	log, err := bench.LoadResults(filepath.Join(work, "results.csv"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []bench.Row{
		{Sample: "a.mov", Flagset: "x", Quality: 24, Key: "k1", Bytes: int64(len(files["keeper"])), OutSHA256: shaOf(files["keeper"])},
		{Sample: "a.mov", Flagset: "y", Quality: 24, Key: "k2", Bytes: 99, OutSHA256: shaOf(files["stale"])},
	} {
		if err := log.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	write(t, bench.KeeperPath(filepath.Join(work, "keep"), "a.mov", "x", 24), files["keeper"])
	write(t, bench.KeeperPath(filepath.Join(work, "keep"), "a.mov", "y", 24), files["stale"])

	outside := filepath.Join(t.TempDir(), "secret.mov")
	write(t, outside, "secret")
	if err := os.Symlink(outside, filepath.Join(samples, "link.mov")); err != nil {
		t.Fatal(err)
	}
	sf := bench.SampleFiles{Format: 1,
		Samples: map[string]bench.SampleFile{
			"00000000000000aa": {Name: "a.mov", Path: filepath.Join(samples, "a.mov"), Reference: filepath.Join(samples, "b.mov"), SourceID: "00000000000000bb"},
			"00000000000000cc": {Name: "secret.mov", Path: outside},
			"00000000000000dd": {Name: "link.mov", Path: filepath.Join(samples, "link.mov")},
		},
		Sources: map[string]string{"00000000000000bb": filepath.Join(samples, "a.mov")},
	}
	data, _ := json.Marshal(sf)
	write(t, bench.SampleFilesPath(work), string(data))
	return srv, hs.URL, files
}

func TestFileRoutes(t *testing.T) {
	_, url, files := fileQueue(t)
	tests := []struct {
		path string
		code int
		body string
	}{
		{"/api/files/encode/" + shaOf(files["object"]), 200, files["object"]},
		{"/api/files/encode/" + shaOf(files["keeper"]), 200, files["keeper"]},
		{"/api/files/encode/" + shaOf(files["stale"]), 404, ""}, // the keeper isn't the logged file
		{"/api/files/encode/" + shaOf("never"), 404, ""},
		{"/api/files/encode/ABC", 400, ""},
		{"/api/files/sample/00000000000000aa", 200, "video"},
		{"/api/files/reference/00000000000000aa", 200, "video"},
		{"/api/files/source/00000000000000bb", 200, "video"},
		{"/api/files/sample/00000000000000cc", 403, ""}, // outside the queue's directories
		{"/api/files/sample/00000000000000dd", 403, ""}, // a symlink out of them
		{"/api/files/sample/00000000000000ee", 404, ""},
		{"/api/files/sample/..%2f..%2fetc", 400, ""},
	}
	for _, tt := range tests {
		code, body, _ := get(t, url+tt.path, nil)
		if code != tt.code || (tt.body != "" && body != tt.body) {
			t.Errorf("GET %s = %d %q, want %d %q", tt.path, code, body, tt.code, tt.body)
		}
	}

	path := "/api/files/encode/" + shaOf(files["object"])
	code, body, h := get(t, url+path, map[string]string{"Range": "bytes=0-5"})
	if code != http.StatusPartialContent || body != "object" || h.Get("ETag") != `"`+shaOf(files["object"])+`"` {
		t.Errorf("range GET = %d %q etag %q, want 206 %q and the SHA-256 as ETag", code, body, h.Get("ETag"), "object")
	}
	if code, _, _ := get(t, url+path, map[string]string{"If-None-Match": h.Get("ETag")}); code != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", code)
	}
}

func TestFileIndex(t *testing.T) {
	_, url, files := fileQueue(t)
	code, body, _ := get(t, url+"/api/files/index", nil)
	if code != 200 {
		t.Fatalf("index: %d %s", code, body)
	}
	var idx FileIndex
	if err := json.Unmarshal([]byte(body), &idx); err != nil {
		t.Fatal(err)
	}
	kept := map[string]string{}
	for _, e := range idx.Encodes {
		kept[e.SHA256] = e.Kept
	}
	if len(kept) != 2 || kept[shaOf(files["object"])] != "object" || kept[shaOf(files["keeper"])] != "keeper" {
		t.Errorf("encodes = %v, want the object and the matching keeper", idx.Encodes)
	}
	names := map[string]SampleEntry{}
	for _, s := range idx.Samples {
		names[s.Name] = s
	}
	if a := names["a.mov"]; a.SampleID != "00000000000000aa" || a.Bytes != 5 || a.ReferenceBytes != 5 || a.SourceID != "00000000000000bb" {
		t.Errorf("a.mov = %+v", a)
	}
	if len(idx.Sources) != 1 || idx.Sources[0].SourceID != "00000000000000bb" {
		t.Errorf("sources = %+v", idx.Sources)
	}
}

func TestFileToken(t *testing.T) {
	srv, url, files := fileQueue(t)
	srv.Token = "s3cret"
	path := url + "/api/files/encode/" + shaOf(files["object"])
	for _, h := range []map[string]string{nil, {"Authorization": "Bearer wrong"}, {"Authorization": "s3cret"}} {
		if code, _, _ := get(t, path, h); code != http.StatusUnauthorized {
			t.Errorf("GET with %v = %d, want 401", h, code)
		}
	}
	if code, body, _ := get(t, path, map[string]string{"Authorization": "Bearer s3cret"}); code != 200 || body != files["object"] {
		t.Errorf("GET with the token = %d %q, want 200", code, body)
	}
	if code, _, _ := get(t, url+"/api/state", nil); code != 200 {
		t.Errorf("GET /api/state without the token = %d, want 200: only the file routes need it", code)
	}
}

func TestServeFileRelativeRoot(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "samples", "a.mov"), "video")
	t.Chdir(dir)
	for _, c := range []struct{ root, path string }{
		{"samples", filepath.Join(dir, "samples", "a.mov")}, // a relative config dir, an absolute manifest path
		{filepath.Join(dir, "samples"), "samples/a.mov"},
		{"./work/../samples", "samples/a.mov"},
	} {
		rec := httptest.NewRecorder()
		serveFile(rec, httptest.NewRequest("GET", "/x", nil), []string{c.root}, c.path, "", "a.mov")
		if rec.Code != 200 || rec.Body.String() != "video" {
			t.Errorf("root %q, path %q: %d %q, want 200", c.root, c.path, rec.Code, rec.Body.String())
		}
	}
}

// TestSampleFilesSharedWorkdir has two configs share a workdir, and so
// its sample-files.json, with a samples directory each. The second
// config's samples directory is a symlink farm: its reference links to
// a file in the first config's samples directory.
func TestSampleFilesSharedWorkdir(t *testing.T) {
	srv, hs, b, _ := start(t)
	base := filepath.Dir(srv.Runner.Dir)
	work, samples2 := filepath.Join(base, "bench"), filepath.Join(base, "samples2")
	write(t, filepath.Join(samples2, "c.mov"), "other video")
	if err := os.Symlink(filepath.Join("..", "samples", "b.mov"), filepath.Join(samples2, "ref.mov")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(srv.Runner.Dir, "20-other.yaml"),
		"samples: ../samples2\nworkdir: ../bench\nflagsets:\n  x: {args: [-c:v, libx265, -preset, fast]}\n")
	sf := bench.SampleFiles{Format: 1, Samples: map[string]bench.SampleFile{
		"00000000000000ff": {Name: "c.mov", Path: filepath.Join(samples2, "c.mov"), Reference: filepath.Join(samples2, "ref.mov")},
	}}
	data, _ := json.Marshal(sf)
	write(t, bench.SampleFilesPath(work), string(data))
	b.release <- struct{}{} // the work files are rescanned before the next unit
	waitFor(t, "the second config to load", func() bool { return len(srv.Runner.Dirs()) == 2 })

	for _, tt := range []struct{ path, body string }{
		{"/api/files/sample/00000000000000ff", "other video"},
		{"/api/files/reference/00000000000000ff", "video"},
	} {
		if code, body, _ := get(t, hs.URL+tt.path, nil); code != 200 || body != tt.body {
			t.Errorf("GET %s = %d %q, want 200 %q", tt.path, code, body, tt.body)
		}
	}
}
