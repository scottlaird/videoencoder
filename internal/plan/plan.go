// Package plan turns a source video into the ffmpeg jobs that encode
// its renditions, audio and poster.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/scottlaird/videoencoder/internal/config"
	"github.com/scottlaird/videoencoder/internal/ffmpeg"
	"github.com/scottlaird/videoencoder/internal/fileid"
	"github.com/scottlaird/videoencoder/internal/gainmap"
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
// outdir, with the standard base command.
func newJob(src Source, outdir, slot, stem, ext string, args []string) *ffmpeg.Job {
	return newJobWithBase(src, baseCommand(src.Path), outdir, slot, stem, ext, args)
}

// newJobWithBase is newJob with a given base command, which must end
// with the input path. The key covers the source's identity and every
// argument, with the input path left out so moving the source doesn't
// rebuild.
func newJobWithBase(src Source, base []string, outdir, slot, stem, ext string, args []string) *ffmpeg.Job {
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

// SeekThumbnailJob writes src's seek-thumbnail tiles into a
// "thumbs.<key>" directory as tile-1.jpg, tile-2.jpg, ...: one frame
// every t.Interval seconds, t.Height high, tiled t.Columns x t.Rows.
func SeekThumbnailJob(t config.Thumbnails, vd probe.VideoData, src Source, outdir string) *ffmpeg.Job {
	// select, unlike fps, takes the first frame even when the video is
	// shorter than one interval. MJPEG needs full-range YUV.
	filter := fmt.Sprintf(`select='isnan(prev_selected_t)+gte(t-prev_selected_t\,%s)',scale=%d:%d,format=yuvj420p,tile=%dx%d`,
		strconv.FormatFloat(t.Interval, 'f', -1, 64), scaledWidth(vd, t.Height), t.Height, t.Columns, t.Rows)
	j := newJob(src, outdir, "thumbs", "thumbs", "", []string{
		"-vf", filter,
		"-fps_mode", "vfr",
		"-q:v", "5",
		"-f", "image2",
		"-start_number", "1",
	})
	j.Output = filepath.Join(j.Output, "tile-%d.jpg")
	return j
}

// ThumbWidth is the width of each seek thumbnail of vd.
func ThumbWidth(t config.Thumbnails, vd probe.VideoData) uint64 { return scaledWidth(vd, t.Height) }

// AudioJob extracts the audio of src.
func AudioJob(src Source, outdir string) *ffmpeg.Job {
	return newJob(src, outdir, "audio", "audio", ".mp4", []string{"-vn"})
}

// Poster is one poster image to make.
type Poster struct {
	Job           *ffmpeg.Job
	Source        string // label of the source it's taken from
	Tone          probe.Tone
	Format        string
	Width, Height uint64
}

// Encoder settings for gain-mapped posters, chosen by measurement (see
// DESIGN.md). They are part of each poster's key, so changing them
// rebuilds the posters.
var (
	GainMapJPEGQuality = gainmap.Quality{Base: 85, GainMap: 70}
	GainMapAVIFArgs    = []string{"-q", "55", "--qgain-map", "50", "--yuv-gain-map", "400", "-s", "6"}
)

// GainMapPoster is a gain-mapped poster: an SDR base taken from SDR
// (tone-mapped first if ToneMap is set) and a gain map that brightens
// it to HDR. It isn't a single ffmpeg job, so it has its own key.
type GainMapPoster struct {
	Slot, Key, Output string
	SDR, HDR          Source
	ToneMap           bool
	Offset            string
	Format            string
	Width, Height     uint64
}

// SourceID combines the identities of both inputs, for the manifest.
func (g *GainMapPoster) SourceID() string { return g.SDR.ID + "+" + g.HDR.ID }

// SDRFilter is the ffmpeg filter giving the SDR base at the poster's
// size, as 8-bit RGB.
func (g *GainMapPoster) SDRFilter() string {
	f := fmt.Sprintf("scale=%d:%d:flags=lanczos,format=rgb24", g.Width, g.Height)
	if g.ToneMap {
		f = tonemapSDR + "," + f
	}
	return f
}

// HDRLinearFilter gives the HDR rendition as planar float RGB in linear
// light with BT.709 primaries, scaled so that 203 nits, the BT.2408
// reference white, is 1.0, matching SDR white.
func (g *GainMapPoster) HDRLinearFilter() string {
	return fmt.Sprintf("scale=%d:%d:flags=lanczos,zscale=t=linear:npl=203,format=gbrpf32le,zscale=p=bt709,format=gbrpf32le", g.Width, g.Height)
}

// HDRFilter gives the HDR rendition as 16-bit RGB, still PQ/BT.2020.
func (g *GainMapPoster) HDRFilter() string {
	return fmt.Sprintf("scale=%d:%d:flags=lanczos,format=rgb48be", g.Width, g.Height)
}

// GainMapPosters returns the gain-mapped posters for p, with the SDR
// base from sdr (whose video is sdrVD) and the HDR rendition from hdr.
// If sdr is the HDR source itself, the base is tone-mapped.
func GainMapPosters(p config.Poster, sdr Source, sdrVD probe.VideoData, hdr Source, outdir string) []GainMapPoster {
	toneMap := sdrVD.Tone() == probe.HDR
	var out []GainMapPoster
	for _, w := range posterWidths(p, sdrVD) {
		h := uint64(float64(sdrVD.Height)*float64(w)/float64(sdrVD.Width)+0.5) &^ 1
		for _, f := range p.GainMapFormats {
			hsh := sha256.New()
			fmt.Fprintf(hsh, "gainmap/v1\x00%s\x00%s\x00%v\x00%s\x00%s\x00%d\x00%d\x00%+v\x00%q\x00%v",
				sdr.ID, hdr.ID, toneMap, p.Offset, f, w, h, GainMapJPEGQuality, GainMapAVIFArgs, gainmap.MaxHeadroom)
			key := hex.EncodeToString(hsh.Sum(nil))[:keyLength]
			out = append(out, GainMapPoster{
				Slot:    fmt.Sprintf("poster/gainmap-%d-%s", w, f),
				Key:     key,
				Output:  filepath.Join(outdir, fmt.Sprintf("poster-gainmap-%d.%s.%s", w, key, f)),
				SDR:     sdr,
				HDR:     hdr,
				ToneMap: toneMap,
				Offset:  p.Offset,
				Format:  f,
				Width:   w,
				Height:  h,
			})
		}
	}
	return out
}

// posterWidths returns p's widths that fit vd, or vd's own width if
// none do.
func posterWidths(p config.Poster, vd probe.VideoData) []uint64 {
	widths := slices.DeleteFunc(slices.Clone(p.Widths), func(w uint64) bool { return w > vd.Width })
	if len(widths) == 0 {
		widths = []uint64{vd.Width}
	}
	return widths
}

// tonemapSDR converts HDR (PQ or HLG) to BT.709 SDR.
const tonemapSDR = "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv"

// Posters returns the jobs for p's poster images in tone, taken from
// src (whose video is vd). An SDR poster from an HDR source is tone
// mapped. HDR posters are AVIF only. Widths larger than the source are
// dropped; if that drops them all, the source's own width is used.
func Posters(p config.Poster, src Source, vd probe.VideoData, tone probe.Tone, outdir string) []Poster {
	base := []string{"ffmpeg", "-loglevel", "warning", "-y", "-ss", p.Offset, "-i", src.Path}
	widths := posterWidths(p, vd)
	fromHDR := vd.Tone() == probe.HDR

	var out []Poster
	for _, w := range widths {
		h := uint64(float64(vd.Height)*float64(w)/float64(vd.Width)+0.5) &^ 1
		for _, f := range p.Formats {
			if tone == probe.HDR && f != "avif" {
				continue
			}
			args := posterArgs(tone, fromHDR, f, w, h)
			stem := fmt.Sprintf("poster-%s-%d", strings.ToLower(string(tone)), w)
			slot := fmt.Sprintf("poster/%s-%d-%s", strings.ToLower(string(tone)), w, f)
			out = append(out, Poster{
				Job:    newJobWithBase(src, base, outdir, slot, stem, "."+f, args),
				Source: src.Label,
				Tone:   tone,
				Format: f,
				Width:  w,
				Height: h,
			})
		}
	}
	return out
}

func posterArgs(tone probe.Tone, fromHDR bool, format string, w, h uint64) []string {
	var filters []string
	if tone == probe.SDR && fromHDR {
		filters = append(filters, tonemapSDR)
	}
	filters = append(filters, fmt.Sprintf("scale=%d:%d:flags=lanczos", w, h))
	pixFmt := map[bool]string{false: "yuv420p", true: "yuv420p10le"}[tone == probe.HDR]
	if format == "jpg" {
		pixFmt = "yuvj420p"
	}
	filters = append(filters, "format="+pixFmt)

	args := []string{"-vf", strings.Join(filters, ","), "-frames:v", "1", "-update", "1"}
	switch format {
	case "avif":
		args = append(args, "-c:v", "libaom-av1", "-still-picture", "1", "-crf", "30", "-cpu-used", "6")
	case "jpg":
		args = append(args, "-q:v", "3")
	case "webp":
		args = append(args, "-c:v", "libwebp", "-quality", "80")
	}
	if tone == probe.HDR {
		args = append(args, "-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc")
	} else {
		args = append(args, "-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709")
	}
	return args
}
