// Package plan turns a source video into the ffmpeg jobs that encode
// its renditions, audio and poster.
package plan

import (
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/scottlaird/videoencoder/internal/config"
	"github.com/scottlaird/videoencoder/internal/ffmpeg"
	"github.com/scottlaird/videoencoder/internal/probe"
)

// Target is one rendition to encode. Width is filled in by Filter.
type Target struct {
	Name          string // rendition name
	EncoderName   string
	Encoder       config.Encoder
	Width, Height uint64
	Quality       float64
	Bitrate       uint64
	Args          []string
}

type Targets []*Target

// FromConfig returns the rendition ladder of c, ordered by encoder and
// height.
func FromConfig(c *config.Config) (Targets, error) {
	var t Targets
	for _, name := range c.RenditionNames() {
		r := c.Renditions[name]
		bitrate, err := config.ParseBitrate(r.Bitrate)
		if err != nil {
			return nil, fmt.Errorf("rendition %q: %w", name, err)
		}
		t = append(t, &Target{
			Name:        name,
			EncoderName: r.Encoder,
			Encoder:     *c.Encoders[r.Encoder],
			Height:      r.Height,
			Quality:     r.Quality,
			Bitrate:     bitrate,
			Args:        r.Args,
		})
	}
	return t, nil
}

// Filter fits the ladder to a source: it drops targets taller than the
// source and computes each target's width from the source aspect
// ratio. A codec whose ladder stops short of the source height gets
// one extra rendition at full source resolution instead of an upscale.
// Targets must be in ascending height order within each encoder.
func (targets Targets) Filter(vd probe.VideoData) Targets {
	filtered := Targets{}
	maxHeight := make(map[string]uint64)

	for _, t := range targets {
		nt := *t
		switch {
		case t.Height <= vd.Height:
			nt.Width = scaledWidth(vd, t.Height)
		case maxHeight[t.EncoderName] < vd.Height:
			nt.Width, nt.Height = vd.Width, vd.Height
		default:
			continue
		}
		filtered = append(filtered, &nt)
		maxHeight[t.EncoderName] = max(maxHeight[t.EncoderName], nt.Height)
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
func (t *Target) Job(infile, outdir string, seq int) *ffmpeg.Job {
	args := []string{
		"-s", fmt.Sprintf("%dx%d", t.Width, t.Height),
		"-c:v", t.Encoder.Codec,
	}
	args = append(args, t.Encoder.Args...)
	args = append(args, t.Encoder.QualityFlag, strconv.FormatFloat(t.Quality, 'f', -1, 64))
	if t.Encoder.CapBitrate && t.Bitrate > 0 {
		burst := fmt.Sprint(t.Bitrate * 3 / 2)
		args = append(args, "-b:v", fmt.Sprint(t.Bitrate), "-maxrate", burst, "-bufsize", burst)
	}
	args = append(args, t.Args...)
	args = append(args, "-an")

	output := filepath.Join(outdir, fmt.Sprintf("video_%d_%s_%dx%d_%d.mp4", seq, t.EncoderName, t.Width, t.Height, t.Bitrate))
	return &ffmpeg.Job{Input: infile, Output: output, Base: baseCommand(infile), Args: args}
}

// SeekThumbnailJob writes one 180p JPEG per second of infile, for seek
// thumbnails.
func SeekThumbnailJob(vd probe.VideoData, infile, outdir string) *ffmpeg.Job {
	const height = 180
	return &ffmpeg.Job{
		Input:  infile,
		Output: filepath.Join(outdir, "thumbnail-%d.jpg"),
		Base:   baseCommand(infile),
		Args: []string{
			"-s", fmt.Sprintf("%dx%d", scaledWidth(vd, height), height),
			"-r", "1",
			"-f", "image2",
		},
	}
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
