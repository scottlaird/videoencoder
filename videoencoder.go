package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/vansante/go-ffprobe.v2"
)

type VideoData struct {
	Width, Height uint64
	FPS           float64
}

type VideoEncodingTarget struct {
	Width, Height uint64
	Codec         string
	Bitrate       uint64
	CodecQuality  uint64
}

type VideoEncodingTargets []*VideoEncodingTarget

var (
	h264Flag   = flag.String("h264", "on", "on/off/nvenc")
	outdirFlag = flag.String("outdir", "output", "output directory")
)

func MaybeExecute(cmd []string, infile, outfile string) error {
	cmdline := strings.Join(cmd, "  ")
	statusfile := outfile + ".status"
	build := false

	// We don't rebuild files if all of these are true:
	//
	// - There's a `.status` file for the output file with the
	//   exact same command line that we're planning on using to
	//   build it.
	// - The output file is the same age or newer than the input file.
	// - No errors occur while checking files

	status, err := os.ReadFile(statusfile)
	if err != nil {
		slog.Info("Building because status file is missing", "outfile", outfile)
		build = true
	} else if string(status) != cmdline {
		slog.Info("Building because command line doesn't match status file", "outfile", outfile)
		build = true
	} else {
		in, err := os.Stat(infile)
		if err != nil {
			return fmt.Errorf("Unable to stat input file %q: %v", infile, err)
		}
		out, err := os.Stat(outfile)
		if err != nil {
			slog.Info("Building because stat of output file failed", "outfile", outfile)
			build = true
		}
		if in.ModTime().After(out.ModTime()) {
			slog.Info("Building because input file is newer than output file", "outfile", outfile)
			build = true
		}
	}

	if build {
		fmt.Printf("-> Building %q via %q\n", outfile, cmdline)

		os.Remove(statusfile)

		c := exec.Command(cmd[0], cmd[1:len(cmd)]...)
		c.Stderr = os.Stderr

		err := c.Run()

		if err != nil {
			return err
		}

		err = os.WriteFile(statusfile, []byte(cmdline), 0644)
		if err != nil {
			// Weird, but probably transient
			fmt.Printf("Error writing status file: %v\n", err)
		}
	} else {
		fmt.Printf("-> skipping %q\n", outfile)
	}
	return nil
}

func GetVideoData(filename string) (VideoData, error) {
	vd := VideoData{}

	f, err := os.Open(filename)
	if err != nil {
		return vd, fmt.Errorf("unable to open media file %q: %v", filename, err)
	}

	data, err := ffprobe.ProbeReader(context.Background(), f)
	if err != nil {
		return vd, fmt.Errorf("unable to parse media file: %v", err)
	}

	var width, height uint64
	var fps string

	for _, stream := range data.Streams {
		if stream.Width > 0 {
			width = uint64(stream.Width)
		}
		if stream.Height > 0 {
			height = uint64(stream.Height)
		}
		if stream.RFrameRate != "" && stream.RFrameRate != "0/0" {
			fps = stream.RFrameRate
		}
	}

	fpsParts := strings.Split(fps, "/")
	fpsNumerator, err := strconv.ParseFloat(fpsParts[0], 64)
	if err != nil {
		return vd, fmt.Errorf("Unable to parse FPS (%q): %v", fpsParts[0], err)
	}
	if len(fpsParts) > 1 {
		fpsDenominator, err := strconv.ParseFloat(fpsParts[1], 64)
		if err != nil {
			return vd, fmt.Errorf("Unable to parse FPS denominator (%q): %v", fpsParts[1], err)
		}

		vd.FPS = fpsNumerator / fpsDenominator
	} else {
		vd.FPS = fpsNumerator
	}

	vd.Width = width
	vd.Height = height

	return vd, nil
}

func (targets VideoEncodingTargets) FilterTargets(vd VideoData) VideoEncodingTargets {
	filteredTargets := []*VideoEncodingTarget{}
	codecMaxHeight := make(map[string]uint64)

	for _, t := range targets {
		if t.Height <= vd.Height {
			// use FP math and round, just to avoid weird corner cases.
			newWidth := uint64(float64(vd.Width)*float64(t.Height)/float64(vd.Height) + 0.5)
			if newWidth&1 == 1 {
				// if the width is odd, then subtract one.  libh264 at least doesn't like odd-numbered widths.
				newWidth--
			}
			nt := VideoEncodingTarget{
				Height:       t.Height,
				Width:        newWidth,
				Codec:        t.Codec,
				Bitrate:      t.Bitrate,
				CodecQuality: t.CodecQuality,
			}
			filteredTargets = append(filteredTargets, &nt)
			if nt.Height > codecMaxHeight[t.Codec] {
				codecMaxHeight[t.Codec] = nt.Height
			}
		} else if codecMaxHeight[t.Codec] < vd.Height {
			// Add a full-resolution version if we *could*
			// add a higher-res version, but it'd require
			// upscaling.
			nt := VideoEncodingTarget{
				Height:       vd.Height,
				Width:        vd.Width,
				Codec:        t.Codec,
				Bitrate:      t.Bitrate,
				CodecQuality: t.CodecQuality,
			}
			codecMaxHeight[t.Codec] = vd.Height
			filteredTargets = append(filteredTargets, &nt)
		}
	}
	return filteredTargets
}

func (target VideoEncodingTarget) GenerateVideoWithFFMPEG(infile, outpath string) (string, error) {
	outfile := fmt.Sprintf("video_%s_%dx%d_%d.mp4", target.Codec, target.Width, target.Height, target.Bitrate)
	outfile = filepath.Join(outpath, outfile)

	codecOptions := []string{}

	switch target.Codec {
	case "av1":
		codecOptions = []string{
			"-c:v", "libsvtav1",
			"-crf", fmt.Sprintf("%d", target.CodecQuality), // Quality
		}
	case "h264":
		codecOptions = []string{
			"-c:v", "libx264",
			"-preset", "faster",
			"-crf", fmt.Sprintf("%d", target.CodecQuality), // Quality
			"-b:v", fmt.Sprintf("%d", target.Bitrate), // Set bitrate
			"-maxrate", fmt.Sprintf("%d", target.Bitrate*3/2), // Allow bursting a bit higher,
		}
	case "h265":
		codecOptions = []string{
			"-c:v", "libx265",
			"-preset", "faster",
			"-crf", fmt.Sprintf("%d", target.CodecQuality), // Quality
			"-b:v", fmt.Sprintf("%d", target.Bitrate), // Set bitrate
			"-maxrate", fmt.Sprintf("%d", target.Bitrate*3/2), // Allow bursting a bit higher,
		}
	}

	cmd := []string{
		"ffmpeg",
		"-loglevel", "warning", "-stats",
		"-y",
		"-i", infile,
		"-s", fmt.Sprintf("%dx%d", target.Width, target.Height), // resolution
	}

	cmd = append(cmd, codecOptions...)
	cmd = append(cmd,
		"-an",                                             // No audio
		"-bufsize", fmt.Sprintf("%d", target.Bitrate*3/2), // Set buffer to max rate
		outfile,
	)

	MaybeExecute(cmd, infile, outfile)

	return outfile, nil
}

func GenerateAudioWithFFMPEG(infile, outpath string) (string, error) {
	outfile := fmt.Sprintf("audio.mp4")
	outfile = filepath.Join(outpath, outfile)

	cmd := []string{
		"ffmpeg",
		"-loglevel", "warning", "-stats",
		"-y", // Overwrite output file
		"-i", infile,
		"-vn", // No video
		outfile,
	}

	MaybeExecute(cmd, infile, outfile)

	return outfile, nil
}

func GenerateThumbnailWithFFMPEG(infile, outpath string) (string, error) {
	outfile := fmt.Sprintf("thumbnail.jpg")
	outfile = filepath.Join(outpath, outfile)

	cmd := []string{
		"ffmpeg",
		"-loglevel", "warning", "-stats",
		"-y", // Overwrite output file
		"-i", infile,
		"-ss", "00:00:01.000",
		"-vframes", "1",
		"-update", "1", // Just write 1 image and don't gripe.
		outfile,
	}

	MaybeExecute(cmd, infile, outfile)

	return outfile, nil
}

func RunShakaPackager(streampath string, videofiles []string, audiofile string) (string, error) {
	dashfile := filepath.Join(streampath, "dash.mpd")
	audiobasename := filepath.Base(audiofile)

	cmd := []string{
		"packager",
		fmt.Sprintf("in=%s,stream=audio,output=%s/%s", audiofile, streampath, audiobasename),
	}

	for _, videofile := range videofiles {
		basename := filepath.Base(videofile)
		cmd = append(cmd, fmt.Sprintf("in=%s,stream=video,output=%s/%s", videofile, streampath, basename))
	}

	cmd = append(cmd,
		"--segment_duration=5",
		"--allow_codec_switching",
		"--min_buffer_time=10",
		fmt.Sprintf("--mpd_output=%s", dashfile),
		fmt.Sprintf("--hls_master_playlist_output=%s/hls.m3u8", streampath),
		"--allow_approximate_segment_timeline",
		"--default_language=en",
	)

	c := exec.Command(cmd[0], cmd[1:len(cmd)]...)
	c.Stderr = os.Stderr

	err := c.Run()

	return dashfile, err
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage:\n\n")
	fmt.Fprintf(os.Stderr, "videoencoder filename\n")
}

func main() {
	flag.Parse()

	filename := flag.Arg(0)

	if filename == "" {
		usage()
		os.Exit(1)
	}

	vd, err := GetVideoData(filename)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Info is %+v\n", vd)

	defaultTargets := VideoEncodingTargets{
		{
			Height:       480,
			Codec:        "h264",
			Bitrate:      1000000,
			CodecQuality: 18,
		},
		{
			Height:       720,
			Codec:        "h264",
			Bitrate:      2000000,
			CodecQuality: 18,
		},
		{
			Height:       1080,
			Codec:        "h264",
			Bitrate:      4000000,
			CodecQuality: 18,
		},
		{
			Height:       720,
			Codec:        "h265",
			Bitrate:      1000000,
			CodecQuality: 20,
		},
		{
			Height:       1080,
			Codec:        "h265",
			Bitrate:      2000000,
			CodecQuality: 20,
		},
		{
			Height:       1440,
			Codec:        "h265",
			Bitrate:      3000000,
			CodecQuality: 20,
		},
		{
			Height:       2160,
			Codec:        "h265",
			Bitrate:      4000000,
			CodecQuality: 20,
		},
		{
			Height:       720,
			Codec:        "av1",
			Bitrate:      1000000,
			CodecQuality: 25,
		},
		{
			Height:       1080,
			Codec:        "av1",
			Bitrate:      2000000,
			CodecQuality: 25,
		},
		{
			Height:       1440,
			Codec:        "av1",
			Bitrate:      3000000,
			CodecQuality: 25,
		},
		{
			Height:       2160,
			Codec:        "av1",
			Bitrate:      4000000,
			CodecQuality: 25,
		},
		{
			Height:       4320,
			Codec:        "av1",
			Bitrate:      8000000,
			CodecQuality: 25,
		},
	}

	ft := defaultTargets.FilterTargets(vd)

	for _, t := range ft {
		fmt.Printf("Targets: %v\n", *t)
	}

	videofiles := []string{}
	for _, t := range ft {
		v, err := t.GenerateVideoWithFFMPEG(filename, *outdirFlag)
		if err != nil {
			panic(err)
		}

		videofiles = append(videofiles, v)
	}
	audiofile, err := GenerateAudioWithFFMPEG(filename, *outdirFlag)
	if err != nil {
		panic(err)
	}

	thumbnail, err := GenerateThumbnailWithFFMPEG(filename, *outdirFlag)
	if err != nil {
		panic(err)
	}

	mpdfile, err := RunShakaPackager(filepath.Join(*outdirFlag, "stream"), videofiles, audiofile)
	if err != nil {
		panic(err)
	}

	fmt.Printf("MPD generated in %q\n", mpdfile)
	fmt.Printf("Thumbnail in %q\n", thumbnail)
}
