// Package plan turns a source video into the ffmpeg jobs that encode
// its renditions, audio and poster.
package plan

import (
	"fmt"
	"path/filepath"

	"github.com/scottlaird/videoencoder/internal/ffmpeg"
	"github.com/scottlaird/videoencoder/internal/probe"
)

// Target is one rendition to encode. Width is filled in by Filter.
type Target struct {
	Width, Height uint64
	Codec         string
	Bitrate       uint64
	CodecQuality  uint64
}

type Targets []*Target

// DefaultTargets is the built-in rendition ladder. With thumbnails, it
// adds a 180p JPEG sequence for seek thumbnails.
func DefaultTargets(thumbnails bool) Targets {
	t := Targets{
		{Height: 480, Codec: "h264", Bitrate: 1000000, CodecQuality: 18},
		{Height: 720, Codec: "h264", Bitrate: 2000000, CodecQuality: 18},
		{Height: 1080, Codec: "h264", Bitrate: 4000000, CodecQuality: 18},
		{Height: 480, Codec: "h265", Bitrate: 800000, CodecQuality: 20},
		{Height: 720, Codec: "h265", Bitrate: 1000000, CodecQuality: 20},
		{Height: 1080, Codec: "h265", Bitrate: 2000000, CodecQuality: 20},
		{Height: 1440, Codec: "h265", Bitrate: 3000000, CodecQuality: 20},
		{Height: 2160, Codec: "h265", Bitrate: 4000000, CodecQuality: 20},
		{Height: 480, Codec: "av1", Bitrate: 700000, CodecQuality: 28},
		{Height: 720, Codec: "av1", Bitrate: 1000000, CodecQuality: 30},
		{Height: 1080, Codec: "av1", Bitrate: 2000000, CodecQuality: 31},
		{Height: 1440, Codec: "av1", Bitrate: 3000000, CodecQuality: 32},
		{Height: 2160, Codec: "av1", Bitrate: 4000000, CodecQuality: 32},
		{Height: 3240, Codec: "av1", Bitrate: 5000000, CodecQuality: 32},
		{Height: 4320, Codec: "av1", Bitrate: 8000000, CodecQuality: 32},
	}
	if thumbnails {
		t = append(t, &Target{Height: 180, Codec: "jpg"})
	}
	return t
}

// Filter fits the ladder to a source: it drops targets taller than the
// source and computes each target's width from the source aspect
// ratio. A codec whose ladder stops short of the source height gets
// one extra rendition at full source resolution instead of an upscale.
// Targets must be in ascending height order within each codec.
func (targets Targets) Filter(vd probe.VideoData) Targets {
	filtered := Targets{}
	codecMaxHeight := make(map[string]uint64)

	for _, t := range targets {
		nt := *t
		switch {
		case t.Height <= vd.Height:
			nt.Width = scaledWidth(vd, t.Height)
		case codecMaxHeight[t.Codec] < vd.Height:
			nt.Width, nt.Height = vd.Width, vd.Height
		default:
			continue
		}
		filtered = append(filtered, &nt)
		codecMaxHeight[t.Codec] = max(codecMaxHeight[t.Codec], nt.Height)
	}
	return filtered
}

// scaledWidth keeps the source aspect ratio at height, rounded to an
// even number because the encoders reject odd widths.
func scaledWidth(vd probe.VideoData, height uint64) uint64 {
	w := uint64(float64(vd.Width)*float64(height)/float64(vd.Height) + 0.5)
	return w &^ 1
}

func baseCommand(infile string) []string {
	return []string{
		"ffmpeg",
		"-loglevel", "warning", "-stats",
		"-y",
		"-i", infile,
	}
}

// Job returns the ffmpeg job that encodes infile to t under outdir.
// seq distinguishes the outputs of different sources (e.g. HDR and SDR
// versions of one video).
func (t *Target) Job(infile, outdir string, seq int) (*ffmpeg.Job, error) {
	size := fmt.Sprintf("%dx%d", t.Width, t.Height)
	quality := fmt.Sprintf("%d", t.CodecQuality)
	bitrate := fmt.Sprintf("%d", t.Bitrate)
	burst := fmt.Sprintf("%d", t.Bitrate*3/2)
	output := filepath.Join(outdir, fmt.Sprintf("video_%d_%s_%dx%d_%d.mp4", seq, t.Codec, t.Width, t.Height, t.Bitrate))

	var args []string
	switch t.Codec {
	case "av1":
		args = []string{
			"-s", size,
			"-c:v", "libsvtav1",
			"-crf", quality,
			"-preset", "5",
			"-an",
		}
	case "h264":
		args = []string{
			"-s", size,
			"-c:v", "libx264",
			"-preset", "faster",
			"-crf", quality,
			"-b:v", bitrate,
			"-maxrate", burst,
			"-bufsize", burst,
			"-an",
		}
	case "h265":
		args = []string{
			"-s", size,
			"-c:v", "libx265",
			"-preset", "faster",
			"-profile:v", "main422-10", // needed for macOS playback
			"-tag:v", "hvc1", // needed for macOS playback
			"-crf", quality,
			"-b:v", bitrate,
			"-maxrate", burst,
			"-bufsize", burst,
			"-an",
		}
	case "jpg":
		args = []string{
			"-s", size,
			"-r", "1",
			"-f", "image2",
		}
		output = filepath.Join(outdir, "thumbnail-%d.jpg")
	default:
		return nil, fmt.Errorf("unknown codec %q", t.Codec)
	}

	return &ffmpeg.Job{Input: infile, Output: output, Base: baseCommand(infile), Args: args}, nil
}

// AudioJob extracts the audio of infile into outdir/audio.mp4.
func AudioJob(infile, outdir string) *ffmpeg.Job {
	return &ffmpeg.Job{
		Input:  infile,
		Output: filepath.Join(outdir, "audio.mp4"),
		Base:   baseCommand(infile),
		Args:   []string{"-vn"},
	}
}

// PosterJob grabs the frame at 1s of infile into outdir/thumbnail.jpg.
func PosterJob(infile, outdir string) *ffmpeg.Job {
	return &ffmpeg.Job{
		Input:  infile,
		Output: filepath.Join(outdir, "thumbnail.jpg"),
		Base:   baseCommand(infile),
		Args: []string{
			"-ss", "00:00:01.000",
			"-vframes", "1",
			"-update", "1",
		},
	}
}
