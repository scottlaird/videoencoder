package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/scottlaird/videoencoder/internal/bench"
	"github.com/scottlaird/videoencoder/internal/config"
	"github.com/scottlaird/videoencoder/internal/plan"
	"github.com/scottlaird/videoencoder/internal/probe"
	"github.com/scottlaird/videoencoder/internal/tune"
)

// tuneVideo searches for r's rendition qualities on a sample and writes
// them into its sidecar. Bitrates are written for renditions whose
// encoder caps bitrate, since there the bitrate sets the cap.
//
// SDR and HDR renditions are tuned apart, each on its own source: SDR
// on the SDR source, HDR (written as hdr.quality) on the HDR one. With
// only an HDR source, SDR-only encoders' renditions are tuned on it,
// tone-mapped as a render would make them.
func tuneVideo(ctx context.Context, r *config.Resolved, adHoc bool, d deps) error {
	var vds []probe.VideoData
	for _, l := range r.SourceLabels() {
		vd, err := d.prober.Probe(ctx, r.Sources[l])
		if err != nil {
			return err
		}
		vds = append(vds, vd)
	}
	sdr, hdr := toneSources(vds)
	hasSDR := vds[sdr].Tone() != probe.HDR

	ladder, err := plan.FromConfig(&r.Config)
	if err != nil {
		return err
	}
	type pass struct {
		label   string
		vd      probe.VideoData
		targets plan.Targets
	}
	var srcs []int
	if hasSDR {
		srcs = append(srcs, sdr)
	}
	if hdr >= 0 {
		srcs = append(srcs, hdr)
	}
	var passes []pass
	reported := map[string]bool{}
	for _, i := range srcs {
		p := pass{label: r.SourceLabels()[i], vd: vds[i]}
		isHDR := vds[i].Tone() == probe.HDR
		for _, t := range ladder.Filter(vds[i]) {
			rd := r.Renditions[t.Name]
			skip := ""
			switch {
			case isHDR && hasSDR && t.Encoder.SDROnly:
				continue // tuned on the SDR source
			case rd.Follows() != "":
				skip = "follows " + rd.Follows()
			case !rd.Tuned():
				skip = fmt.Sprintf("quality %-5g kept: tune: false", t.Quality)
			case t.Height > tune.MaxHeight:
				skip = fmt.Sprintf("quality %-5g kept: taller than %dp", t.Quality, tune.MaxHeight)
			default:
				p.targets = append(p.targets, t)
				continue
			}
			if !reported[t.Name] {
				reported[t.Name] = true
				fmt.Fprintf(d.stdout, "#   %-10s %s\n", t.Name, skip)
			}
		}
		if len(p.targets) > 0 {
			passes = append(passes, p)
		}
	}
	if len(passes) == 0 {
		return nil
	}

	if err := os.MkdirAll(r.Outdir, 0o755); err != nil {
		return err
	}
	work, err := os.MkdirTemp(r.Outdir, ".tune-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	version, err := bench.FFmpegVersion(ctx)
	if err != nil {
		return err
	}
	tu := &tune.Tuner{
		Exec: d.exec, Prober: d.prober, Threads: 4,
		Results:       filepath.Join(r.Outdir, tune.ResultsFile),
		FFmpegVersion: version,
	}

	date := time.Now().Format(time.DateOnly)
	var updates []config.RenditionUpdate
	for _, p := range passes {
		// Each pass has its own scratch directory, since references are
		// named by height alone.
		pw := filepath.Join(work, p.label)
		slog.Info("Tuning", "source", p.label, "renditions", len(p.targets))
		results, err := tu.Tune(ctx, r, r.Sources[p.label], p.vd, p.targets, pw)
		if err != nil {
			return err
		}
		var ranges []string
		for _, tr := range r.Tune.Ranges {
			ranges = append(ranges, tr.String())
		}
		fmt.Fprintf(d.stdout, "# %s: tuned on %s of %s\n", r.Name, strings.Join(ranges, " and "), p.label)
		for i, res := range results {
			u := config.RenditionUpdate{Name: res.Rendition, Quality: res.Quality, HDR: res.HDR, TunedFor: p.targets[i].Encoder.Codec}
			if p.targets[i].Encoder.CapBitrate {
				u.Bitrate = tune.FormatBitrate(res.Bitrate)
			}
			metric := "VMAF"
			if res.Model != "" {
				metric = res.Model
			}
			switch {
			case res.Interpolated:
				u.Comment = fmt.Sprintf("interpolated by tune %s for %s %g", date, metric, res.Target)
			case res.Outcome == bench.Found:
				u.Comment = fmt.Sprintf("tuned %s: %s %.1f (target %g)", date, metric, res.VMAF, res.Target)
			default:
				u.Comment = fmt.Sprintf("tuned %s: %s %.1f, target %g out of range (%s)", date, metric, res.VMAF, res.Target, res.Outcome)
			}
			updates = append(updates, u)
			name := res.Rendition
			if res.HDR {
				name += " hdr"
			}
			fmt.Fprintf(d.stdout, "#   %-14s quality %-5g bitrate %-6s %s\n", name, res.Quality, tune.FormatBitrate(res.Bitrate), u.Comment)
		}
	}

	var sources map[string]string
	if adHoc {
		sources = map[string]string{}
		for l, p := range r.Sources {
			if rel, err := filepath.Rel(r.Dir, p); err == nil {
				p = rel
			}
			sources[l] = p
		}
	}
	sidecar := filepath.Join(r.Dir, r.Name+config.SidecarSuffix)
	if err := config.UpdateSidecar(sidecar, sources, updates); err != nil {
		return err
	}
	fmt.Fprintf(d.stdout, "# wrote %s\n", sidecar)
	return nil
}
