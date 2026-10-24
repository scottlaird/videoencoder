// Package pack bundles encoded renditions into DASH and HLS streams
// with Shaka Packager.
package pack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/scottlaird/videoencoder/internal/ffmpeg"
	"github.com/scottlaird/videoencoder/internal/probe"
)

// Packager runs shaka-packager (as "packager" on PATH).
type Packager struct {
	Runner ffmpeg.Runner
	Prober probe.Prober
}

// Package writes dash.mpd, hls.m3u8 and the segmented media into
// streamdir, and returns the path of the DASH manifest. Each video is
// labelled HDR or SDR so players can offer the choice.
//
// Every media file and playlist is named after its input, so it also
// returns the names of every file it wrote in streamdir.
func (p *Packager) Package(ctx context.Context, streamdir string, videos []string, audio string) (string, []string, error) {
	if err := os.MkdirAll(streamdir, 0o755); err != nil {
		return "", nil, fmt.Errorf("creating stream directory: %w", err)
	}
	argv, err := p.argv(ctx, streamdir, videos, audio)
	if err != nil {
		return "", nil, err
	}
	if err := p.Runner.Run(ctx, argv); err != nil {
		return "", nil, err
	}
	return filepath.Join(streamdir, "dash.mpd"), streamFiles(append([]string{audio}, videos...)), nil
}

// streamFiles returns the names Package writes for inputs.
func streamFiles(inputs []string) []string {
	names := []string{"dash.mpd", "hls.m3u8"}
	for _, in := range inputs {
		base := filepath.Base(in)
		names = append(names, base, playlistName(base))
	}
	return names
}

func playlistName(base string) string {
	return strings.TrimSuffix(base, filepath.Ext(base)) + ".m3u8"
}

func (p *Packager) argv(ctx context.Context, streamdir string, videos []string, audio string) ([]string, error) {
	abase := filepath.Base(audio)
	argv := []string{
		"packager",
		fmt.Sprintf("in=%s,stream=audio,output=%s,playlist_name=%s", audio, filepath.Join(streamdir, abase), playlistName(abase)),
	}

	for _, v := range videos {
		vd, err := p.Prober.Probe(ctx, v)
		if err != nil {
			return nil, err
		}
		base := filepath.Base(v)
		argv = append(argv, fmt.Sprintf("in=%s,stream=video,output=%s,playlist_name=%s,dash_label=%s",
			v, filepath.Join(streamdir, base), playlistName(base), vd.Tone()))
	}

	argv = append(argv,
		"--segment_duration=5",
		"--allow_codec_switching",
		"--min_buffer_time=10",
		"--mpd_output="+filepath.Join(streamdir, "dash.mpd"),
		"--hls_master_playlist_output="+filepath.Join(streamdir, "hls.m3u8"),
		"--allow_approximate_segment_timeline",
		"--default_language=en",
	)
	return argv, nil
}
