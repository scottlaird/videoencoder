package tune

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/scottlaird/videoencoder/internal/bench"
	"github.com/scottlaird/videoencoder/internal/config"
	"github.com/scottlaird/videoencoder/internal/plan"
	"github.com/scottlaird/videoencoder/internal/probe"
)

// Result is the tuned setting for one rendition.
type Result struct {
	Rendition    string
	Quality      float64
	Bitrate      uint64 // bits per second, rounded
	VMAF         float64
	Target       float64
	Interpolated bool
	// Outcome is the search's; for an anchor that ran out of range, the
	// quality is the range limit.
	Outcome bench.Outcome
	Encodes int
}

// Tuner runs the sample encodes and scorings.
type Tuner struct {
	Exec    bench.Executor
	Prober  probe.Prober
	Threads int // libvmaf n_threads
}

// Tune searches r's anchor renditions on a sample of src (whose video
// is vd) and interpolates the others. targets are r's renditions as
// filtered for the source. Scratch files go in workdir.
func (tu *Tuner) Tune(ctx context.Context, r *config.Resolved, src string, vd probe.VideoData, targets plan.Targets, workdir string) ([]Result, error) {
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return nil, err
	}
	sample := filepath.Join(workdir, "sample.mkv")
	if err := tu.ffmpeg(ctx, "-ss", r.Tune.Start, "-i", src, "-t", r.Tune.Duration, "-map", "0:v:0", "-c", "copy", sample); err != nil {
		return nil, fmt.Errorf("cutting sample: %w", err)
	}
	sampleVD, err := tu.Prober.Probe(ctx, sample)
	if err != nil {
		return nil, err
	}
	if sampleVD.Duration <= 0 {
		return nil, fmt.Errorf("sample of %s is empty: is tune.start past its end?", src)
	}
	hdr := vd.Tone() == probe.HDR

	refs := map[refKey]reference{}
	results := make([]Result, len(targets))
	byEncoder := map[string][]Point{}
	for enc, idx := range Anchors(targets, r.Tune.Anchors) {
		for _, i := range idx {
			t := targets[i]
			// SDR-only encoders tone-map HDR, as a render would, and are
			// judged against a tone-mapped reference. The rest are judged
			// with both sides tone-mapped, since VMAF is an SDR metric.
			toneMap := t.Encoder.SDROnly && hdr
			prefilter := ""
			if hdr && !toneMap {
				prefilter = plan.ToneMapSDR
			}
			ref, err := tu.reference(ctx, refs, sample, vd, t.Height, toneMap, workdir)
			if err != nil {
				return nil, err
			}
			res, bitrate, err := tu.search(ctx, r, t, sample, sampleVD.Duration.Seconds(), ref, toneMap, prefilter, workdir)
			if err != nil {
				return nil, fmt.Errorf("tuning %s: %w", t.Name, err)
			}
			results[i] = res
			byEncoder[enc] = append(byEncoder[enc], Point{Height: t.Height, Quality: res.Quality, Bitrate: bitrate})
		}
	}

	for i, t := range targets {
		if results[i].Rendition != "" {
			continue
		}
		q, b := Interpolate(byEncoder[t.EncoderName], t.Height)
		spec := qualitySpec(t.Encoder.Codec)
		results[i] = Result{
			Rendition:    t.Name,
			Quality:      spec.IndexToQuality(spec.QualityToIndex(q)),
			Bitrate:      roundBitrate(b),
			Target:       target(r, t),
			Interpolated: true,
		}
	}
	return results, nil
}

// refKey picks a reference: its height, and whether it is tone-mapped.
type refKey struct {
	height  uint64
	toneMap bool
}

// reference is the sample at the size a rendition is judged at.
type reference struct {
	path          string
	width, height uint64
	transfer      string // color transfer, as ffprobe names it
}

// viewingHeight is the size a rendition of height h is scored at:
// 1080p for HD and below, 2160p above, as Netflix's VMAF models
// assume; never more than the source.
func viewingHeight(h, source uint64) uint64 {
	v := uint64(1080)
	if h > 1080 {
		v = 2160
	}
	return min(v, source)
}

func (tu *Tuner) reference(ctx context.Context, refs map[refKey]reference, sample string, vd probe.VideoData, h uint64, toneMap bool, workdir string) (reference, error) {
	vh := viewingHeight(h, vd.Height)
	key := refKey{vh, toneMap}
	if ref, ok := refs[key]; ok {
		return ref, nil
	}
	name, transfer := fmt.Sprintf("ref-%d.mkv", vh), vd.ColorTransfer
	if toneMap {
		name, transfer = fmt.Sprintf("ref-%d-sdr.mkv", vh), "bt709" // what ToneMapSDR writes
	}
	ref := reference{
		path:     filepath.Join(workdir, name),
		width:    uint64(float64(vd.Width)*float64(vh)/float64(vd.Height)+0.5) &^ 1,
		height:   vh,
		transfer: transfer,
	}
	// FFV1 is lossless, so the reference adds no error of its own.
	vf := fmt.Sprintf("scale=%d:%d:flags=lanczos", ref.width, ref.height)
	if toneMap {
		vf = plan.ToneMapSDR + "," + vf
	}
	err := tu.ffmpeg(ctx, "-i", sample, "-vf", vf, "-c:v", "ffv1", "-level", "3", ref.path)
	if err != nil {
		return reference{}, fmt.Errorf("making %dp reference: %w", vh, err)
	}
	refs[key] = ref
	return ref, nil
}

// qualitySpec is the search range for an encoder: its bench defaults,
// with fractional steps coarsened to 0.5, finer than a few seconds of
// sample can resolve anyway.
func qualitySpec(codec string) bench.Quality {
	q, ok := bench.DefaultQuality(codec)
	if !ok {
		q = bench.Quality{Min: 0, Max: 63, Step: 1, HigherIsBetter: new(bool)}
	}
	if q.Step < 0.5 {
		q.Step = 0.5
	}
	return q
}

func target(r *config.Resolved, t *plan.Target) float64 {
	if rd := r.Renditions[t.Name]; rd != nil && rd.TargetVMAF > 0 {
		return rd.TargetVMAF
	}
	return r.Tune.TargetVMAF
}

// search finds the worst quality setting at which t, encoded from the
// sample without bitrate caps (and tone-mapped first with toneMap),
// reaches its target VMAF. It returns the result and the bitrate of
// that encode.
func (tu *Tuner) search(ctx context.Context, r *config.Resolved, t *plan.Target, sample string, seconds float64, ref reference, toneMap bool, prefilter, workdir string) (Result, float64, error) {
	spec := qualitySpec(t.Encoder.Codec)
	goal := target(r, t)
	s := bench.NewSearch(spec.Steps(), spec.QualityToIndex(t.Quality))
	type point struct{ vmaf, bitrate float64 }
	seen := map[int]point{}
	src := plan.Source{Label: "tune", Path: sample, ID: "sample"}

	for {
		i, outcome := s.Next()
		if outcome != bench.Searching {
			best := s.Answer()
			if best < 0 {
				// Nothing passes: the best quality in range is the most
				// that can be done.
				best = 0
				if _, ok := seen[0]; !ok {
					return Result{}, 0, fmt.Errorf("no quality setting reached VMAF %.1f", goal)
				}
			}
			p := seen[best]
			res := Result{
				Rendition: t.Name, Quality: spec.IndexToQuality(best), Bitrate: roundBitrate(p.bitrate),
				VMAF: p.vmaf, Target: goal, Outcome: outcome, Encodes: len(seen),
			}
			slog.Info("Tuned", "rendition", t.Name, "quality", res.Quality, "vmaf", p.vmaf,
				"bitrate", FormatBitrate(res.Bitrate), "outcome", outcome, "encodes", len(seen))
			return res, p.bitrate, nil
		}

		q := spec.IndexToQuality(i)
		trial := *t
		trial.Quality = q
		trial.Encoder.CapBitrate = false
		out := filepath.Join(workdir, fmt.Sprintf("%s.q%v.mp4", t.Name, q))
		job := trial.Job(src, workdir, toneMap)
		args := append([]string{"-i", sample}, job.Args...)
		if err := tu.ffmpeg(ctx, append(args, out)...); err != nil {
			return Result{}, 0, err
		}
		st, err := os.Stat(out)
		if err != nil {
			return Result{}, 0, err
		}
		m, err := bench.Score(ctx, tu.Exec, out, ref.path, ref.width, ref.height, tu.Threads, workdir, filepath.Base(out), prefilter, ref.transfer)
		os.Remove(out)
		if err != nil {
			return Result{}, 0, err
		}
		seen[i] = point{vmaf: m.VMAF, bitrate: float64(st.Size()) * 8 / seconds}
		margin := m.VMAF - goal
		slog.Info("Trial", "rendition", t.Name, "quality", q, "vmaf", m.VMAF, "pass", margin >= 0)
		s.Observe(i, margin >= 0, margin)
	}
}

func (tu *Tuner) ffmpeg(ctx context.Context, args ...string) error {
	argv := slices.Concat([]string{"ffmpeg", "-hide_banner", "-loglevel", "error", "-nostats", "-y"}, args)
	_, err := tu.Exec.Exec(ctx, argv)
	return err
}
