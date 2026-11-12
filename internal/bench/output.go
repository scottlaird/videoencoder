package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
)

// OutputInfo is what ffprobe says of an encode's video stream.
type OutputInfo struct {
	Codec   string // ffprobe's codec_name, such as "hevc"
	Profile string // such as "Main 10"
	// Level is ffprobe's level, as it reports it: H.264's level times
	// 10 (41 is 4.1), HEVC's times 30 (153 is 5.1), AV1's
	// seq_level_idx (13 is 5.1). "" if it reports none.
	Level  string
	PixFmt string
	// Keyframes counts the packets flagged as keyframes.
	Keyframes int64
	// PeakKbps1s is the most bits in any one second of the stream, by
	// decode time: how close it comes to a cap a player or a level
	// imposes.
	PeakKbps1s float64
}

// OutputProber reads an encode's OutputInfo.
type OutputProber interface {
	ProbeOutput(ctx context.Context, path string) (OutputInfo, error)
}

// FFProbeOutput is the OutputProber backed by the ffprobe on PATH. It
// reads packet headers only, without decoding.
type FFProbeOutput struct{}

func (FFProbeOutput) ProbeOutput(ctx context.Context, path string) (OutputInfo, error) {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name,profile,level,pix_fmt:packet=dts_time,size,flags",
		"-of", "json", path).Output()
	if err != nil {
		return OutputInfo{}, fmt.Errorf("probing %s: %w", path, err)
	}
	return parseOutputProbe(out)
}

// parseOutputProbe reads ffprobe's JSON for FFProbeOutput.
func parseOutputProbe(data []byte) (OutputInfo, error) {
	var doc struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			Profile   string `json:"profile"`
			Level     *int   `json:"level"`
			PixFmt    string `json:"pix_fmt"`
		} `json:"streams"`
		Packets []struct {
			DTSTime string `json:"dts_time"`
			Size    string `json:"size"`
			Flags   string `json:"flags"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return OutputInfo{}, fmt.Errorf("reading ffprobe output: %w", err)
	}
	if len(doc.Streams) == 0 {
		return OutputInfo{}, fmt.Errorf("no video stream")
	}
	st := doc.Streams[0]
	info := OutputInfo{Codec: st.CodecName, Profile: st.Profile, PixFmt: st.PixFmt}
	// ffprobe gives -99 for "unknown".
	if st.Level != nil && *st.Level >= 0 {
		info.Level = strconv.Itoa(*st.Level)
	}
	type pkt struct {
		t    float64
		bits int64
	}
	var pkts []pkt
	for _, p := range doc.Packets {
		if len(p.Flags) > 0 && p.Flags[0] == 'K' {
			info.Keyframes++
		}
		t, err1 := strconv.ParseFloat(p.DTSTime, 64)
		size, err2 := strconv.ParseInt(p.Size, 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		pkts = append(pkts, pkt{t, size * 8})
	}
	slices.SortStableFunc(pkts, func(a, b pkt) int {
		switch {
		case a.t < b.t:
			return -1
		case a.t > b.t:
			return 1
		}
		return 0
	})
	// The bits in every window [pkts[i].t, pkts[i].t+1).
	var sum, peak int64
	j := 0
	for i := range pkts {
		for j < len(pkts) && pkts[j].t < pkts[i].t+1 {
			sum += pkts[j].bits
			j++
		}
		peak = max(peak, sum)
		sum -= pkts[i].bits
	}
	info.PeakKbps1s = float64(peak) / 1000
	return info, nil
}
