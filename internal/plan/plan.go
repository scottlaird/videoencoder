// Package plan turns a source video into the ffmpeg jobs that encode
// its renditions, audio and poster.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/scottlaird/videoencoder/internal/config"
	"github.com/scottlaird/videoencoder/internal/ffmpeg"
	"github.com/scottlaird/videoencoder/internal/fileid"
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

// Source is one input file of a video.
type Source struct {
	Label string // from the config's sources map
	Path  string
	// ID identifies the file's content and modification time, so a
	// replaced or re-exported source rebuilds everything made from it.
	ID string
}

// NewSource identifies the file at path.
func NewSource(label, path string) (Source, error) {
	content, err := fileid.Content(path)
	if err != nil {
		return Source{}, fmt.Errorf("source %q: %w", label, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return Source{}, err
	}
	return Source{Label: label, Path: path, ID: fmt.Sprintf("%s-%d", content, st.ModTime().Unix())}, nil
}

func baseCommand(infile string) []string {
	return []string{
		"ffmpeg",
		"-loglevel", "warning", "-stats",
		"-y",
		"-i", infile,
	}
}

// keyLength is how much of the build key goes in file names.
const keyLength = 10

// newJob makes a job whose output is named "<stem>.<key><ext>" in
// outdir. The key covers the source's identity and every argument, with
// the input path left out so moving the source doesn't rebuild.
func newJob(src Source, outdir, slot, stem, ext string, args []string) *ffmpeg.Job {
	base := baseCommand(src.Path)
	h := sha256.New()
	fmt.Fprintf(h, "build/v1\x00%s", src.ID)
	for _, a := range base[:len(base)-1] {
		fmt.Fprintf(h, "\x00%s", a)
	}
	for _, a := range args {
		fmt.Fprintf(h, "\x00%s", a)
	}
	key := hex.EncodeToString(h.Sum(nil))[:keyLength]
	return &ffmpeg.Job{
		Input:  src.Path,
		Output: filepath.Join(outdir, stem+"."+key+ext),
		Base:   base,
		Args:   args,
		Slot:   slot,
		Key:    key,
		Source: src.ID,
	}
}

// Job returns the ffmpeg job that encodes src to t under outdir, as
// "<rendition>.<source label>.<key>.mp4".
func (t *Target) Job(src Source, outdir string) *ffmpeg.Job {
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
	return newJob(src, outdir, src.Label+"/"+t.Name, t.Name+"."+src.Label, ".mp4", args)
}

// SeekThumbnailJob writes one 180p JPEG per second of src into a
// "thumbs.<key>" directory, for seek thumbnails.
func SeekThumbnailJob(vd probe.VideoData, src Source, outdir string) *ffmpeg.Job {
	const height = 180
	j := newJob(src, outdir, "thumbs", "thumbs", "", []string{
		"-s", fmt.Sprintf("%dx%d", scaledWidth(vd, height), height),
		"-r", "1",
		"-f", "image2",
	})
	j.Output = filepath.Join(j.Output, "%d.jpg")
	return j
}

// AudioJob extracts the audio of src.
func AudioJob(src Source, outdir string) *ffmpeg.Job {
	return newJob(src, outdir, "audio", "audio", ".mp4", []string{"-vn"})
}

// PosterJob grabs the frame at 1s of src.
func PosterJob(src Source, outdir string) *ffmpeg.Job {
	return newJob(src, outdir, "poster", "poster", ".jpg", []string{
		"-ss", "00:00:01.000",
		"-vframes", "1",
		"-update", "1",
	})
}
