package ffmpeg

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDropNoise(t *testing.T) {
	var out strings.Builder
	d := newDropNoise(&out)
	fmt.Fprint(d, "E1107 08:02:12.725558 165312328 box_definitions.cc:655] NOTIMPLEMENTED: SampleDecryption type 0 is not handled. Skipping.\n")
	fmt.Fprint(d, "[vost#0:0/libsvtav1 @ 0x563ab78d0980] Discarding stream metadata 'vendor_id' because the stream is being re-encoded. Use '-keep_metadata vendor_id' to keep it.\n")
	fmt.Fprint(d, "frame=  10 fps=1.0\rframe=  20 fps=2.0\r")
	fmt.Fprint(d, "[libx264 @ 0x1] a real war")
	fmt.Fprint(d, "ning\nE1107 packager.cc:12] Packaging failed\n")
	fmt.Fprint(d, "no newline at the end")
	d.Flush()
	want := "frame=  10 fps=1.0\rframe=  20 fps=2.0\r" +
		"[libx264 @ 0x1] a real warning\nE1107 packager.cc:12] Packaging failed\n" +
		"no newline at the end"
	if out.String() != want {
		t.Errorf("passed\n %q\nwant\n %q", out.String(), want)
	}
}

func TestChattyToolQuietUnlessItFails(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "avifgainmaputil")
	script := "#!/bin/sh\necho 'AVIF to be written:'\necho ' * Resolution     : 1920x1080' >&2\nexit $1\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	r := ExecRunner{Stdout: &out, Stderr: &out}
	if err := r.Run(context.Background(), []string{tool, "0"}); err != nil {
		t.Fatalf("succeeding run: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("a successful run wrote %q; want nothing", out.String())
	}
	if err := r.Run(context.Background(), []string{tool, "1"}); err == nil {
		t.Fatal("failing run returned no error")
	}
	if !strings.Contains(out.String(), "AVIF to be written:") || !strings.Contains(out.String(), "Resolution") {
		t.Errorf("a failed run wrote %q; want its output, to explain the failure", out.String())
	}
}
