package pack

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/scottlaird/videoencoder/internal/probe"
)

type fakeProber map[string]probe.VideoData

func (f fakeProber) Probe(_ context.Context, name string) (probe.VideoData, error) {
	vd, ok := f[name]
	if !ok {
		return probe.VideoData{}, errors.New("no such file")
	}
	return vd, nil
}

// recorder records the packager command and writes a minimal MP4 for
// each output, as packager would.
type recorder struct{ argv []string }

func (r *recorder) Run(_ context.Context, argv []string) error {
	r.argv = argv
	for _, a := range argv {
		for _, field := range strings.Split(a, ",") {
			if out, ok := strings.CutPrefix(field, "output="); ok {
				if err := os.WriteFile(out, []byte("\x00\x00\x00\x08free"), 0o644); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func TestPackage(t *testing.T) {
	prober := fakeProber{
		"t/hdr.mp4": {ColorTransfer: "smpte2084"},
		"t/sdr.mp4": {ColorTransfer: "bt709"},
	}
	r := &recorder{}
	p := &Packager{Runner: r, Prober: prober}
	streamdir := t.TempDir()

	mpd, files, err := p.Package(context.Background(), streamdir, []string{"t/hdr.mp4", "t/sdr.mp4"}, "t/audio.mp4")
	if err != nil {
		t.Fatalf("Package() = %v", err)
	}
	if want := streamdir + "/dash.mpd"; mpd != want {
		t.Errorf("manifest = %q, want %q", mpd, want)
	}
	want := []string{
		"packager",
		"in=t/audio.mp4,stream=audio,output=" + streamdir + "/audio.mp4,playlist_name=audio.m3u8",
		"in=t/hdr.mp4,stream=video,output=" + streamdir + "/hdr.mp4,playlist_name=hdr.m3u8,dash_label=HDR",
		"in=t/sdr.mp4,stream=video,output=" + streamdir + "/sdr.mp4,playlist_name=sdr.m3u8,dash_label=SDR",
	}
	if !slices.Equal(r.argv[:len(want)], want) {
		t.Errorf("argv starts\n %q\nwant\n %q", r.argv[:len(want)], want)
	}
	wantFiles := []string{"dash.mpd", "hls.m3u8", "audio.mp4", "audio.m3u8", "hdr.mp4", "hdr.m3u8", "sdr.mp4", "sdr.m3u8"}
	if !slices.Equal(files, wantFiles) {
		t.Errorf("files = %q, want %q", files, wantFiles)
	}
}

func TestPackageSilent(t *testing.T) {
	r := &recorder{}
	p := &Packager{Runner: r, Prober: fakeProber{"t/v.mp4": {ColorTransfer: "bt709"}}}
	_, files, err := p.Package(context.Background(), t.TempDir(), []string{"t/v.mp4"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(r.argv, " "), "stream=audio") {
		t.Errorf("silent video packaged with an audio stream: %q", r.argv)
	}
	if want := []string{"dash.mpd", "hls.m3u8", "v.mp4", "v.m3u8"}; !slices.Equal(files, want) {
		t.Errorf("files = %q, want %q", files, want)
	}
}

func TestPackageProbeError(t *testing.T) {
	p := &Packager{Runner: &recorder{}, Prober: fakeProber{}}
	if _, _, err := p.Package(context.Background(), t.TempDir(), []string{"missing.mp4"}, "a.mp4"); err == nil {
		t.Error("Package() with an unprobeable video: got nil error, want one")
	}
}
