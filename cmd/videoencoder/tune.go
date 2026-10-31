package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
func tuneVideo(ctx context.Context, r *config.Resolved, adHoc bool, d deps) error {
	// Score against SDR where there is an SDR source: VMAF is
	// calibrated for SDR, and the tone map is one more variable.
	var vds []probe.VideoData
	for _, l := range r.SourceLabels() {
		vd, err := d.prober.Probe(ctx, r.Sources[l])
		if err != nil {
			return err
		}
		vds = append(vds, vd)
	}
	sdr, _ := toneSources(vds)
	label := r.SourceLabels()[sdr]
	vd := vds[sdr]

	ladder, err := plan.FromConfig(&r.Config)
	if err != nil {
		return err
	}
	var targets, fixed plan.Targets
	for _, t := range ladder.Filter(vd) {
		if r.Renditions[t.Name].Tuned() {
			targets = append(targets, t)
		} else {
			fixed = append(fixed, t)
		}
	}
	for _, t := range fixed {
		fmt.Fprintf(d.stdout, "#   %-10s quality %-5g kept: tune: false\n", t.Name, t.Quality)
	}
	if len(targets) == 0 {
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
	results, err := tu.Tune(ctx, r, r.Sources[label], vd, targets, work)
	if err != nil {
		return err
	}

	date := time.Now().Format(time.DateOnly)
	var updates []config.RenditionUpdate
	fmt.Fprintf(d.stdout, "# %s: tuned on %s of %s from %s\n", r.Name, r.Tune.Duration, label, r.Tune.Start)
	for i, res := range results {
		u := config.RenditionUpdate{Name: res.Rendition, Quality: res.Quality}
		if targets[i].Encoder.CapBitrate {
			u.Bitrate = tune.FormatBitrate(res.Bitrate)
		}
		switch {
		case res.Interpolated:
			u.Comment = fmt.Sprintf("interpolated by tune %s for VMAF %g", date, res.Target)
		case res.Outcome == bench.Found:
			u.Comment = fmt.Sprintf("tuned %s: VMAF %.1f (target %g)", date, res.VMAF, res.Target)
		default:
			u.Comment = fmt.Sprintf("tuned %s: VMAF %.1f, target %g out of range (%s)", date, res.VMAF, res.Target, res.Outcome)
		}
		updates = append(updates, u)
		fmt.Fprintf(d.stdout, "#   %-10s quality %-5g bitrate %-6s %s\n", res.Rendition, res.Quality, tune.FormatBitrate(res.Bitrate), u.Comment)
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
