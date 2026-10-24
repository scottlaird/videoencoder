package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestWriteAliases(t *testing.T) {
	dir := t.TempDir()
	images := []posterImage{
		{Src: "poster-gainmap-640.a.jpg", Tone: "gainmap", Format: "jpg", Width: 640},
		{Src: "poster-gainmap-1920.b.jpg", Tone: "gainmap", Format: "jpg", Width: 1920},
		{Src: "poster-sdr-640.c.jpg", Tone: "SDR", Format: "jpg", Width: 640},
		{Src: "poster-sdr-640.d.webp", Tone: "SDR", Format: "webp", Width: 640},
		{Src: "poster-sdr-1920.e.jpg", Tone: "SDR", Format: "jpg", Width: 1920},
		{Src: "poster-hdr-640.f.avif", Tone: "HDR", Format: "avif", Width: 640},
	}
	for _, im := range images {
		if err := os.WriteFile(filepath.Join(dir, im.Src), []byte(im.Src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := writeAliases(dir, 1920, images)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"poster-640.jpg", "poster-1920.jpg", "poster.jpg", "poster-640.webp"}
	if !slices.Equal(files, want) {
		t.Errorf("files = %q, want %q", files, want)
	}
	for alias, src := range map[string]string{
		"poster-640.jpg":  "poster-gainmap-640.a.jpg",
		"poster-1920.jpg": "poster-gainmap-1920.b.jpg",
		"poster.jpg":      "poster-gainmap-1920.b.jpg",
		"poster-640.webp": "poster-sdr-640.d.webp",
	} {
		if got, _ := os.ReadFile(filepath.Join(dir, alias)); string(got) != src {
			t.Errorf("%s is a copy of %q, want %q", alias, got, src)
		}
	}
	if len(images[2].Aliases) != 0 || len(images[5].Aliases) != 0 {
		t.Error("an SDR image with a gain-mapped alternative, or an HDR image, got an alias")
	}
	if !slices.Equal(images[1].Aliases, []string{"poster-1920.jpg", "poster.jpg"}) {
		t.Errorf("full-width aliases = %q", images[1].Aliases)
	}
}
