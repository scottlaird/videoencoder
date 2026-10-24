package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/scottlaird/videoencoder/internal/ffmpeg"
	"github.com/scottlaird/videoencoder/internal/gainmap"
	"github.com/scottlaird/videoencoder/internal/plan"
	"github.com/scottlaird/videoencoder/internal/state"
)

// avifGainMapTool combines an SDR and an HDR image into a gain-mapped
// AVIF. It is part of libavif.
const avifGainMapTool = "avifgainmaputil"

// checkGainMapTools reports a missing external tool before any work
// starts, rather than after the renditions are encoded.
func checkGainMapTools(gms []plan.GainMapPoster) error {
	for _, g := range gms {
		if g.Format == "avif" {
			if _, err := exec.LookPath(avifGainMapTool); err != nil {
				return fmt.Errorf("gain-mapped AVIF posters need %s from libavif; install it, or remove avif from poster.gainmap_formats", avifGainMapTool)
			}
			return nil
		}
	}
	return nil
}

// buildGainMaps builds the gain-mapped posters that are missing (or
// match the --rebuild glob), recording each in the manifest.
func buildGainMaps(ctx context.Context, d deps, b *built, rebuild string) error {
	for _, g := range b.gainmaps {
		_, err := os.Stat(g.Output)
		exists := err == nil
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		force := forced(rebuild, &ffmpeg.Job{Slot: g.Slot})
		reason := b.manifest.Explain(g.Slot, g.Key, g.SourceID(), exists)
		if reason == state.UpToDate && !force {
			slog.Info("Up to date", "slot", g.Slot, "output", filepath.Base(g.Output))
			continue
		}
		if force {
			reason = "forced"
		}
		slog.Info("Building", "slot", g.Slot, "output", filepath.Base(g.Output), "reason", reason)
		if err := buildGainMap(ctx, d.runner, g); err != nil {
			return fmt.Errorf("%s: %w", g.Slot, err)
		}
		if err := b.manifest.Record(g.Slot, state.Entry{
			Key: g.Key, Output: filepath.Base(g.Output), Source: g.SourceID(), Args: g.SDRFilter() + " | " + g.HDRFilter(),
		}); err != nil {
			return err
		}
		if err := b.manifest.Save(); err != nil {
			return err
		}
	}
	return nil
}

// buildGainMap writes g.Output, by way of a partial file and scratch
// frames that are removed afterwards.
func buildGainMap(ctx context.Context, r ffmpeg.Runner, g plan.GainMapPoster) error {
	tmp, err := os.MkdirTemp(filepath.Dir(g.Output), ".gainmap-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	partial := filepath.Join(tmp, "out."+g.Format)

	frame := func(src plan.Source, filter, out string, format ...string) error {
		argv := []string{"ffmpeg", "-loglevel", "error", "-y", "-ss", g.Offset, "-i", src.Path,
			"-frames:v", "1", "-update", "1", "-vf", filter}
		argv = append(argv, format...)
		return r.Run(ctx, append(argv, out))
	}

	switch g.Format {
	case "jpg":
		sdrRaw, hdrRaw := filepath.Join(tmp, "sdr.raw"), filepath.Join(tmp, "hdr.raw")
		if err := frame(g.SDR, g.SDRFilter(), sdrRaw, "-f", "rawvideo"); err != nil {
			return err
		}
		if err := frame(g.HDR, g.HDRLinearFilter(), hdrRaw, "-f", "rawvideo"); err != nil {
			return err
		}
		if err := composeJPEG(sdrRaw, hdrRaw, int(g.Width), int(g.Height), partial); err != nil {
			return err
		}
	case "avif":
		sdrPNG, hdrPNG := filepath.Join(tmp, "sdr.png"), filepath.Join(tmp, "hdr.png")
		if err := frame(g.SDR, g.SDRFilter(), sdrPNG); err != nil {
			return err
		}
		if err := frame(g.HDR, g.HDRFilter(), hdrPNG); err != nil {
			return err
		}
		// CICP: BT.709/sRGB base, BT.2020/PQ alternate.
		argv := []string{avifGainMapTool, "combine", sdrPNG, hdrPNG, partial,
			"--cicp-base", "1/13/6", "--cicp-alternate", "9/16/9",
			"--max-headroom", strconv.FormatFloat(gainmap.MaxHeadroom, 'f', -1, 64)}
		err := r.Run(ctx, append(argv, plan.GainMapAVIFArgs...))
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("can't make a gain-mapped %s", g.Format)
	}
	return os.Rename(partial, g.Output)
}

// composeJPEG reads the raw frames ffmpeg wrote (8-bit RGB SDR, planar
// float GBR linear HDR) and writes an Ultra HDR JPEG.
func composeJPEG(sdrRaw, hdrRaw string, w, h int, out string) error {
	sdrPix, err := os.ReadFile(sdrRaw)
	if err != nil {
		return err
	}
	if len(sdrPix) != 3*w*h {
		return fmt.Errorf("SDR frame is %d bytes, want %d", len(sdrPix), 3*w*h)
	}
	sdr := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range w * h {
		copy(sdr.Pix[4*i:4*i+3], sdrPix[3*i:3*i+3])
		sdr.Pix[4*i+3] = 255
	}

	hdrPix, err := os.ReadFile(hdrRaw)
	if err != nil {
		return err
	}
	if len(hdrPix) != 12*w*h {
		return fmt.Errorf("HDR frame is %d bytes, want %d", len(hdrPix), 12*w*h)
	}
	hdr := &gainmap.Linear{Width: w, Height: h, Pix: make([]float32, 3*w*h)}
	plane := func(p, i int) float32 {
		return math.Float32frombits(binary.LittleEndian.Uint32(hdrPix[4*(p*w*h+i):]))
	}
	for i := range w * h {
		// gbrpf32le planes are G, B, R.
		hdr.Pix[3*i], hdr.Pix[3*i+1], hdr.Pix[3*i+2] = plane(2, i), plane(0, i), plane(1, i)
	}

	gm, m, err := gainmap.Compute(sdr, hdr, 1)
	if err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	err = gainmap.WriteJPEG(bw, sdr, gm, m, plan.GainMapJPEGQuality)
	return errors.Join(err, bw.Flush(), f.Close())
}

// gainMapDesc describes g for plan output.
func gainMapDesc(g plan.GainMapPoster) string {
	from := g.SDR.Label
	if g.ToneMap {
		from += " tone-mapped"
	}
	return fmt.Sprintf("%s %dx%d from %s + %s", g.Slot, g.Width, g.Height, from, strings.TrimSpace(g.HDR.Label))
}
