package publish

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeStream(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// recording wraps a target and records the order of puts.
type recording struct {
	Target
	puts []string
}

func (r *recording) Put(ctx context.Context, name, localPath string, size int64, meta Meta) error {
	r.puts = append(r.puts, name)
	return r.Target.Put(ctx, name, localPath, size, meta)
}

var stream = map[string]string{
	"dash.mpd":                       "<MPD/>",
	"hls.m3u8":                       "#EXTM3U",
	"poster.json":                    "{}",
	"av1-1080.main.0123456789.mp4":   "video",
	"av1-1080.main.0123456789.m3u8":  "#EXTM3U media",
	"thumbs.abcdef0123/tile-1.jpg":   "tile",
	"poster-sdr-640.fedcba9876.avif": "poster",
}

var streamFiles = []string{"dash.mpd", "hls.m3u8", "poster.json", "av1-1080.main.0123456789.mp4",
	"av1-1080.main.0123456789.m3u8", "thumbs.abcdef0123", "poster-sdr-640.fedcba9876.avif"}

func TestSyncDir(t *testing.T) {
	ctx := context.Background()
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "www", "hike")
	writeStream(t, src, stream)

	target := &recording{Target: Dir{Root: dst}}
	r, err := Sync(ctx, src, streamFiles, target, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Uploaded) != len(stream) || len(r.Skipped) != 0 {
		t.Errorf("first sync: uploaded %d, skipped %d; want %d, 0", len(r.Uploaded), len(r.Skipped), len(stream))
	}
	// Manifests go last.
	if tail := target.puts[len(target.puts)-3:]; !slices.Equal(tail, []string{"dash.mpd", "hls.m3u8", "poster.json"}) {
		t.Errorf("upload order ends %q, want the manifests", tail)
	}
	for name, content := range stream {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(name)))
		if err != nil || string(got) != content {
			t.Errorf("%s = %q, %v", name, got, err)
		}
	}

	// Nothing changed: nothing uploaded.
	target.puts = nil
	if r, err = Sync(ctx, src, streamFiles, target, false); err != nil {
		t.Fatal(err)
	}
	if len(r.Uploaded) != 0 {
		t.Errorf("unchanged sync uploaded %q", r.Uploaded)
	}

	// A same-size change to a small file is caught by its MD5.
	writeStream(t, src, map[string]string{"dash.mpd": "<MPX/>"})
	if r, err = Sync(ctx, src, streamFiles, target, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Uploaded, []string{"dash.mpd"}) {
		t.Errorf("after editing dash.mpd, uploaded %q", r.Uploaded)
	}

	// A rendition replaced by a new key: the old one stays without
	// prune, and goes with it, along with its now-empty directory.
	os.Remove(filepath.Join(src, "av1-1080.main.0123456789.mp4"))
	os.RemoveAll(filepath.Join(src, "thumbs.abcdef0123"))
	writeStream(t, src, map[string]string{"av1-1080.main.9999999999.mp4": "video2"})
	files := slices.DeleteFunc(slices.Clone(streamFiles), func(f string) bool {
		return f == "av1-1080.main.0123456789.mp4" || f == "thumbs.abcdef0123"
	})
	files = append(files, "av1-1080.main.9999999999.mp4")
	if r, err = Sync(ctx, src, files, target, false); err != nil {
		t.Fatal(err)
	}
	if len(r.Deleted) != 0 {
		t.Errorf("deleted %q without prune", r.Deleted)
	}
	if r, err = Sync(ctx, src, files, target, true); err != nil {
		t.Fatal(err)
	}
	if want := []string{"av1-1080.main.0123456789.mp4", "thumbs.abcdef0123/tile-1.jpg"}; !slices.Equal(r.Deleted, want) {
		t.Errorf("prune deleted %q, want %q", r.Deleted, want)
	}
	if _, err := os.Stat(filepath.Join(dst, "thumbs.abcdef0123")); err == nil {
		t.Error("prune left an empty thumbs directory")
	}
}

func TestMetaFor(t *testing.T) {
	tests := []struct{ name, contentType, cache string }{
		{"dash.mpd", "application/dash+xml", "no-cache"},
		{"hls.m3u8", "application/vnd.apple.mpegurl", "no-cache"},
		{"av1-1080.main.0123456789.m3u8", "application/vnd.apple.mpegurl", "public, max-age=31536000, immutable"},
		{"audio.0123456789.mp4", "audio/mp4", "public, max-age=31536000, immutable"},
		{"h264-720.sdr.0123456789.mp4", "video/mp4", "public, max-age=31536000, immutable"},
		{"thumbs.0123456789/tile-3.jpg", "image/jpeg", "public, max-age=31536000, immutable"},
		{"poster-hdr-640.0123456789.avif", "image/avif", "public, max-age=31536000, immutable"},
		{"thumbnail.jpg", "image/jpeg", "no-cache"},
		{"poster.json", "application/json", "no-cache"},
	}
	for _, tt := range tests {
		if m := metaFor(tt.name); m.ContentType != tt.contentType || m.CacheControl != tt.cache {
			t.Errorf("metaFor(%q) = %+v, want %s / %s", tt.name, m, tt.contentType, tt.cache)
		}
	}
}

// TestSyncS3 runs against a real bucket, named by
// $VIDEOENCODER_TEST_S3_BUCKET, using the usual endpoint and credential
// variables. It works under a random prefix and removes only what it
// created there.
func TestSyncS3(t *testing.T) {
	bucket := os.Getenv("VIDEOENCODER_TEST_S3_BUCKET")
	if bucket == "" || testing.Short() {
		t.Skip("set VIDEOENCODER_TEST_S3_BUCKET to test against S3")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	rand.Read(b)
	prefix := "videoencoder-test-" + hex.EncodeToString(b) + "/"
	target, err := NewS3(S3Config{Bucket: bucket, Prefix: prefix, Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		objs, _ := target.List(ctx)
		for name := range objs {
			target.Delete(ctx, name)
		}
	})

	src := t.TempDir()
	writeStream(t, src, stream)
	big := strings.Repeat("x", smallFile+1)
	writeStream(t, src, map[string]string{"av1-2160.main.aaaaaaaaaa.mp4": big})
	files := append(slices.Clone(streamFiles), "av1-2160.main.aaaaaaaaaa.mp4")

	r, err := Sync(ctx, src, files, target, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Uploaded) != len(stream)+1 {
		t.Fatalf("uploaded %d, want %d", len(r.Uploaded), len(stream)+1)
	}
	objs, err := target.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(maps.Keys(objs))
	want := []string{"av1-1080.main.0123456789.m3u8", "av1-1080.main.0123456789.mp4", "av1-2160.main.aaaaaaaaaa.mp4",
		"dash.mpd", "hls.m3u8", "poster-sdr-640.fedcba9876.avif", "poster.json", "thumbs.abcdef0123/tile-1.jpg"}
	if !slices.Equal(got, want) {
		t.Errorf("bucket holds %q, want %q", got, want)
	}

	if r, err = Sync(ctx, src, files, target, false); err != nil {
		t.Fatal(err)
	}
	if len(r.Uploaded) != 0 {
		t.Errorf("unchanged sync uploaded %q", r.Uploaded)
	}

	files = slices.DeleteFunc(files, func(f string) bool { return f == "poster.json" })
	if r, err = Sync(ctx, src, files, target, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Deleted, []string{"poster.json"}) {
		t.Errorf("prune deleted %q", r.Deleted)
	}
}
