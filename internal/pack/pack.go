// Package pack bundles encoded renditions into DASH and HLS streams
// with Shaka Packager.
package pack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/scottlaird/videoencoder/internal/ffmpeg"
	"github.com/scottlaird/videoencoder/internal/probe"
)

// Packager runs shaka-packager (as "packager" on PATH).
type Packager struct {
	Runner ffmpeg.Runner
	Prober probe.Prober
	// Thumbnails asks packager for a seek-thumbnail adaptation set.
	// Packager 3.9 doesn't support this, so the run will fail.
	Thumbnails bool
}

// Package writes dash.mpd, hls.m3u8 and the segmented media into
// streamdir, and returns the path of the DASH manifest. Each video is
// labelled HDR or SDR so players can offer the choice.
func (p *Packager) Package(ctx context.Context, streamdir string, videos []string, audio string) (string, error) {
	if err := os.MkdirAll(streamdir, 0o755); err != nil {
		return "", fmt.Errorf("creating stream directory: %w", err)
	}
	argv, err := p.argv(ctx, streamdir, videos, audio)
	if err != nil {
		return "", err
	}
	if err := p.Runner.Run(ctx, argv); err != nil {
		return "", err
	}
	return filepath.Join(streamdir, "dash.mpd"), nil
}

func (p *Packager) argv(ctx context.Context, streamdir string, videos []string, audio string) ([]string, error) {
	argv := []string{
		"packager",
		fmt.Sprintf("in=%s,stream=audio,output=%s", audio, filepath.Join(streamdir, filepath.Base(audio))),
	}

	for _, v := range videos {
		vd, err := p.Prober.Probe(ctx, v)
		if err != nil {
			return nil, err
		}
		argv = append(argv, fmt.Sprintf("in=%s,stream=video,output=%s,dash_label=%s",
			v, filepath.Join(streamdir, filepath.Base(v)), vd.Tone()))
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
	if p.Thumbnails {
		argv = append(argv,
			"--add_thumbnail_adaptation_set",
			fmt.Sprintf("mimeType=image/jpg:bandwidth=10000,height=180,media=%s/thumbnails-$Number$.jpg,timescale=1,duration=1", streamdir),
		)
	}
	return argv, nil
}
