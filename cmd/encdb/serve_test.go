package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHandler(t *testing.T) {
	data := filepath.Join(t.TempDir(), "mine.parquet")
	os.WriteFile(data, []byte("0123456789"), 0o644)
	srv := httptest.NewServer(handler(data))
	defer srv.Close()

	get := func(path, rng string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}

	if resp, _ := get("/facets.html", ""); resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("GET /facets.html: %s, Cache-Control %q", resp.Status, resp.Header.Get("Cache-Control"))
	}
	// -data replaces the snapshot, and DuckDB can read it in ranges.
	resp, body := get("/encodes.parquet", "bytes=2-4")
	if resp.StatusCode != http.StatusPartialContent || body != "234" {
		t.Errorf("GET /encodes.parquet bytes=2-4: %s %q, want 206 %q", resp.Status, body, "234")
	}
}
