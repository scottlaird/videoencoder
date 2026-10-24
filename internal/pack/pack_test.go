package pack

import (
	"context"
	"errors"
	"slices"
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

type recorder struct{ argv []string }

func (r *recorder) Run(_ context.Context, argv []string) error {
	r.argv = argv
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

	mpd, err := p.Package(context.Background(), streamdir, []string{"t/hdr.mp4", "t/sdr.mp4"}, "t/audio.mp4")
	if err != nil {
		t.Fatalf("Package() = %v", err)
	}
	if want := streamdir + "/dash.mpd"; mpd != want {
		t.Errorf("manifest = %q, want %q", mpd, want)
	}
	want := []string{
		"packager",
		"in=t/audio.mp4,stream=audio,output=" + streamdir + "/audio.mp4",
		"in=t/hdr.mp4,stream=video,output=" + streamdir + "/hdr.mp4,dash_label=HDR",
		"in=t/sdr.mp4,stream=video,output=" + streamdir + "/sdr.mp4,dash_label=SDR",
	}
	if !slices.Equal(r.argv[:len(want)], want) {
		t.Errorf("argv starts\n %q\nwant\n %q", r.argv[:len(want)], want)
	}
}

func TestPackageProbeError(t *testing.T) {
	p := &Packager{Runner: &recorder{}, Prober: fakeProber{}}
	if _, err := p.Package(context.Background(), t.TempDir(), []string{"missing.mp4"}, "a.mp4"); err == nil {
		t.Error("Package() with an unprobeable video: got nil error, want one")
	}
}
