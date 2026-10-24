// Package probe reads the video properties videoencoder needs from a
// media file via ffprobe.
package probe

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/vansante/go-ffprobe.v2"
)

// VideoData describes the first real video stream of a file.
type VideoData struct {
	Width, Height  uint64
	FPS            float64
	ColorTransfer  string
	ColorPrimaries string
	// Duration is the container's duration.
	Duration time.Duration
}

// Tone is the dynamic range of a video, as used for DASH labels.
type Tone string

const (
	SDR            Tone = "SDR"
	HDR            Tone = "HDR"
	UnknownTone    Tone = "Unknown"
	transferPQ          = "smpte2084"
	transferHLG         = "arib-std-b67"
	transferRec709      = "bt709"
)

// Tone classifies the video by its color transfer characteristic.
func (vd VideoData) Tone() Tone {
	switch vd.ColorTransfer {
	case transferPQ, transferHLG:
		return HDR
	case transferRec709:
		return SDR
	}
	return UnknownTone
}

// Prober returns the VideoData of a file. It exists so callers can be
// tested without ffprobe.
type Prober interface {
	Probe(ctx context.Context, filename string) (VideoData, error)
}

// FFProbe is the Prober backed by the ffprobe binary on PATH.
type FFProbe struct{}

// Probe runs ffprobe on filename. The path is passed to ffprobe rather
// than piped, so files whose index is at the end (common for .mov)
// work.
func (FFProbe) Probe(ctx context.Context, filename string) (VideoData, error) {
	data, err := ffprobe.ProbeURL(ctx, filename)
	if err != nil {
		return VideoData{}, fmt.Errorf("probing %q: %w", filename, err)
	}
	vd, err := fromStreams(filename, data.Streams)
	if err != nil {
		return VideoData{}, err
	}
	if data.Format != nil {
		vd.Duration = data.Format.Duration()
	}
	return vd, nil
}

func fromStreams(filename string, streams []*ffprobe.Stream) (VideoData, error) {
	for _, s := range streams {
		if s.CodecType != "video" || s.Disposition.AttachedPic != 0 {
			continue
		}
		fps, err := ParseFrameRate(s.RFrameRate)
		if err != nil {
			return VideoData{}, fmt.Errorf("probing %q: %w", filename, err)
		}
		return VideoData{
			Width:          uint64(s.Width),
			Height:         uint64(s.Height),
			FPS:            fps,
			ColorTransfer:  s.ColorTransfer,
			ColorPrimaries: s.ColorPrimaries,
		}, nil
	}
	return VideoData{}, fmt.Errorf("probing %q: no video stream", filename)
}

// ParseFrameRate parses ffprobe's rational frame rates ("30000/1001")
// as well as plain numbers ("25").
func ParseFrameRate(s string) (float64, error) {
	num, den, isRatio := strings.Cut(s, "/")
	n, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing frame rate %q: %w", s, err)
	}
	if !isRatio {
		return n, nil
	}
	d, err := strconv.ParseFloat(den, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing frame rate %q: %w", s, err)
	}
	if d == 0 {
		return 0, fmt.Errorf("parsing frame rate %q: zero denominator", s)
	}
	return n / d, nil
}
