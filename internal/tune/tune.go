package tune

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/scottlaird/videoencoder/internal/bench"
	"github.com/scottlaird/videoencoder/internal/config"
	"github.com/scottlaird/videoencoder/internal/ffmpeg"
	"github.com/scottlaird/videoencoder/internal/fileid"
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
	Encodes int // new trial encodes
	Cached  int // trials taken from the results file instead
	// Model names the VMAF model VMAF and Target are on, or is "" for
	// the built-in one.
	Model string
	// HDR is set when Quality and Bitrate are for the rendition's HDR
	// encodes (see config.RenditionHDR).
	HDR bool
}

// Tuner runs the sample encodes and scorings.
type Tuner struct {
	Exec    bench.Executor
	Prober  probe.Prober
	Threads int // libvmaf n_threads
	// Results is a CSV file of every trial, read at the start and
	// appended to as trials finish, so a later run with a different
	// target reuses the encodes that still apply; "" keeps none.
	Results string
	// FFmpegVersion is part of each trial's key: another ffmpeg build
	// may encode differently.
	FFmpegVersion string
}

// Tune searches r's anchor renditions on samples of src (whose video
// is vd), one per tune range, and interpolates the others. targets are r's renditions as
// filtered for the source, none taller than MaxHeight. Scratch files
// go in workdir.
//
// From an HDR source, the renditions of encoders that aren't SDR-only
// are HDR, and are tuned for their HDR settings with tune's HDR target
// and models; the rest are SDR.
func (tu *Tuner) Tune(ctx context.Context, r *config.Resolved, src string, vd probe.VideoData, targets plan.Targets, workdir string) ([]Result, error) {
	for _, t := range targets {
		if t.Height > MaxHeight {
			return nil, fmt.Errorf("can't tune %s: taller than %dp", t.Name, MaxHeight)
		}
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return nil, err
	}
	clips, err := tu.cut(ctx, r.Tune.Ranges, src, vd, workdir)
	if err != nil {
		return nil, err
	}
	hdr := vd.Tone() == probe.HDR
	models, err := loadModels(r.Tune)
	if err != nil {
		return nil, err
	}
	sourceID, err := fileid.Content(src)
	if err != nil {
		return nil, err
	}
	past, err := loadResults(tu.Results)
	if err != nil {
		return nil, err
	}

	results := make([]Result, len(targets))
	byEncoder := map[string][]Point{}
	scorer := func(i int) string {
		t, hdrOut := forTone(targets[i], hdr)
		return models.forTarget(t, hdrOut).ScorerAt(t.Height)
	}
	for enc, idx := range Anchors(targets, r.Tune.Anchors, scorer) {
		for _, i := range idx {
			t, hdrOut := forTone(targets[i], hdr)
			// A rendition is tuned once per tone, so its logs say which.
			log := slog.With("rendition", t.Name, "tone", toneName(hdrOut))
			// SDR-only encoders tone-map HDR, as a render would, and are
			// judged against a tone-mapped reference. The rest are judged
			// with both sides tone-mapped, since VMAF is an SDR metric,
			// unless an HDR model judges them as HDR.
			toneMap := t.Encoder.SDROnly && hdr
			model := models.forTarget(t, hdrOut)
			if m := models.sdr.forHeight(t.Height); !hdrOut && m.HDR {
				log.Warn("HDR VMAF model can't judge an SDR rendition; using the built-in model; set tune.hdr.vmaf_model instead",
					"model", m.Name())
			}
			prefilter := ""
			if hdrOut && !model.HDR {
				prefilter = plan.ToneMapSDR
			}
			scorer := bench.ScorerVersion
			if id := model.ID(); id != "" {
				scorer += " " + id
			}
			var judged []judging
			for _, c := range clips {
				ref := tu.reference(c.refs, vd, t, toneMap, c.dir)
				ref.model = model
				judged = append(judged, judging{clip: c, ref: ref, key: trialKey{
					source: sourceID, start: c.Start, duration: c.Duration,
					reference: fmt.Sprintf("%dx%d tonemap=%v %s", ref.width, ref.height, toneMap, ref.transfer),
					prefilter: prefilter, ffmpeg: tu.FFmpegVersion, scorer: scorer,
				}})
			}
			res, bitrate, err := tu.search(ctx, log, target(r, t, hdrOut), t, judged, toneMap, prefilter, past)
			if err != nil {
				return nil, fmt.Errorf("tuning %s: %w", t.Name, err)
			}
			res.HDR = hdrOut
			results[i] = res
			byEncoder[enc] = append(byEncoder[enc], Point{Height: t.Height, Quality: res.Quality, Bitrate: bitrate})
		}
	}

	for i, t := range targets {
		if results[i].Rendition != "" {
			continue
		}
		_, hdrOut := forTone(t, hdr)
		q, b := Interpolate(byEncoder[t.EncoderName], t.Height)
		spec := qualitySpec(t.Encoder.Codec)
		results[i] = Result{
			Rendition:    t.Name,
			Quality:      spec.IndexToQuality(spec.QualityToIndex(q)),
			Bitrate:      roundBitrate(b),
			Target:       target(r, t, hdrOut),
			Model:        models.forTarget(t, hdrOut).Name(),
			Interpolated: true,
			HDR:          hdrOut,
		}
	}
	return results, nil
}

// clip is the sample cut from one tune range, and the references made
// from it, which live in dir.
type clip struct {
	config.TuneRange
	sample  string
	seconds float64
	dir     string
	refs    map[refKey]reference
}

// cut cuts a sample from src (whose video is vd) for each range.
func (tu *Tuner) cut(ctx context.Context, ranges []config.TuneRange, src string, vd probe.VideoData, workdir string) ([]*clip, error) {
	var clips []*clip
	for i, tr := range ranges {
		start, err := config.ParseOffset(tr.Start)
		if err != nil {
			return nil, fmt.Errorf("tune.ranges[%d]: %w", i, err)
		}
		length, err := config.ParseOffset(tr.Duration)
		if err != nil {
			return nil, fmt.Errorf("tune.ranges[%d]: %w", i, err)
		}
		// Frame timing leaves a range that ends at the source's end a
		// little short.
		if vd.Duration > 0 && start+length > vd.Duration+time.Second/2 {
			return nil, fmt.Errorf("tune.ranges[%d] (%s) runs past the end of %s, which lasts %v", i, tr, src, vd.Duration.Round(time.Millisecond))
		}
		c := &clip{TuneRange: tr, dir: filepath.Join(workdir, fmt.Sprintf("range%d", i)), refs: map[refKey]reference{}}
		if err := os.MkdirAll(c.dir, 0o755); err != nil {
			return nil, err
		}
		c.sample = filepath.Join(c.dir, "sample.mkv")
		if err := tu.ffmpeg(ctx, "-ss", tr.Start, "-i", src, "-t", tr.Duration, "-map", "0:v:0", "-c", "copy", c.sample); err != nil {
			return nil, fmt.Errorf("cutting sample %s: %w", tr, err)
		}
		cvd, err := tu.Prober.Probe(ctx, c.sample)
		if err != nil {
			return nil, err
		}
		if cvd.Duration <= 0 {
			return nil, fmt.Errorf("sample %s of %s is empty: is it past the end?", tr, src)
		}
		c.seconds = cvd.Duration.Seconds()
		clips = append(clips, c)
	}
	return clips, nil
}

// judging is how a rendition is judged on one clip: the reference it's
// scored against, and the key its trials are recorded under.
type judging struct {
	*clip
	ref reference
	key trialKey
}

// refKey picks a reference: its size, and whether it is tone-mapped.
type refKey struct {
	width, height uint64
	toneMap       bool
}

// reference is the sample at the size a rendition is judged at.
type reference struct {
	path          string
	width, height uint64
	transfer      string // color transfer, as ffprobe names it
	toneMap       bool   // tone-mapped to SDR from an HDR sample
	// model is what renditions are scored with against it.
	model bench.VMAFModel
}

// toneName is how logs name an SDR or HDR rendition.
func toneName(hdr bool) string {
	if hdr {
		return "hdr"
	}
	return "sdr"
}

// forTone returns t as it is encoded from a source that is HDR or
// not, and whether that encode is HDR.
func forTone(t *plan.Target, hdr bool) (*plan.Target, bool) {
	if hdr && !t.Encoder.SDROnly {
		return t.ForHDR(), true
	}
	return t, false
}

// models are the VMAF models tune scores SDR and HDR renditions with.
type models struct{ sdr, hdr modelPair }

// modelPair is base, and uhd for renditions 2160p and up.
type modelPair struct{ base, uhd bench.VMAFModel }

func loadModels(t config.Tune) (models, error) {
	sdr, err := loadPair(t.VMAFModel, t.VMAFModel2160)
	if err != nil {
		return models{}, err
	}
	hdr := sdr
	if t.HDR.VMAFModel != "" {
		if hdr, err = loadPair(t.HDR.VMAFModel, t.HDR.VMAFModel2160); err != nil {
			return models{}, err
		}
	}
	return models{sdr, hdr}, nil
}

func loadPair(base, uhd string) (modelPair, error) {
	load := func(path string) (bench.VMAFModel, error) {
		if path == "" || path == config.BuiltinVMAFModel {
			return bench.VMAFModel{}, nil
		}
		return bench.LoadVMAFModel(path)
	}
	var p modelPair
	var err error
	if p.base, err = load(base); err != nil {
		return modelPair{}, err
	}
	p.uhd = p.base
	if uhd != "" {
		if p.uhd, err = load(uhd); err != nil {
			return modelPair{}, err
		}
	}
	return p, nil
}

func (m modelPair) forHeight(h uint64) bench.VMAFModel {
	if h >= 2160 {
		return m.uhd
	}
	return m.base
}

// forTarget returns the model t is scored with as an SDR or HDR
// rendition. An HDR model can't judge SDR, so SDR renditions fall back
// from one to the built-in model.
func (m models) forTarget(t *plan.Target, hdr bool) bench.VMAFModel {
	if hdr {
		return m.hdr.forHeight(t.Height)
	}
	if model := m.sdr.forHeight(t.Height); !model.HDR {
		return model
	}
	return bench.VMAFModel{}
}

// MaxHeight is the tallest rendition tune can judge: VMAF's largest
// model is trained for 2160p, and its scores say nothing reliable
// about 6K or 8K.
const MaxHeight = 2160

// reference returns the reference that t is judged against, without
// making it; makeReference does that when a trial first needs it.
// Each rendition is judged at its own size, so its score measures
// what the encoder lost rather than what the smaller size can't hold:
// the snow in a 4K clip can't survive a scale to 480p however well
// that's encoded.
func (tu *Tuner) reference(refs map[refKey]reference, vd probe.VideoData, t *plan.Target, toneMap bool, workdir string) reference {
	w, h := t.Width, t.Height
	key := refKey{w, h, toneMap}
	if ref, ok := refs[key]; ok {
		return ref
	}
	name, transfer := fmt.Sprintf("ref-%d.mkv", h), vd.ColorTransfer
	if toneMap {
		name, transfer = fmt.Sprintf("ref-%d-sdr.mkv", h), "bt709" // what ToneMapSDR writes
	}
	ref := reference{
		path:     filepath.Join(workdir, name),
		width:    w,
		height:   h,
		transfer: transfer,
		toneMap:  toneMap,
	}
	refs[key] = ref
	return ref
}

// makeReference writes ref from sample, unless it already exists.
func (tu *Tuner) makeReference(ctx context.Context, sample string, ref reference) error {
	if _, err := os.Stat(ref.path); err == nil {
		return nil
	}
	// FFV1 is lossless, so the reference adds no error of its own.
	vf := plan.Scale(ref.width, ref.height)
	if ref.toneMap {
		// ToneMapSDR leaves float RGB, which FFV1 stores only at level
		// 4 in ffmpeg 8 and up (older ones converted it silently), so
		// convert to 16-bit RGB, which is lossless enough to score.
		vf = plan.ToneMapSDR + "," + vf + ",format=gbrp16le"
	}
	if err := tu.ffmpeg(ctx, "-i", sample, "-vf", vf, "-c:v", "ffv1", "-level", "3", ref.path); err != nil {
		return fmt.Errorf("making %dp reference: %w", ref.height, err)
	}
	return nil
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

func target(r *config.Resolved, t *plan.Target, hdr bool) float64 {
	return r.Tune.Target(r.Renditions[t.Name], hdr)
}

// search finds the worst quality setting at which t, encoded from
// each clip without bitrate caps (and tone-mapped first with toneMap),
// reaches goal on all of them. A setting's trial is the combination of
// its clips' (see combine), and the result's bitrate is its. Trials
// recorded in past under each clip's key are used instead of encoding
// again, and new ones are recorded there.
func (tu *Tuner) search(ctx context.Context, log *slog.Logger, goal float64, t *plan.Target, clips []judging, toneMap bool, prefilter string, past *results) (Result, float64, error) {
	spec := qualitySpec(t.Encoder.Codec)
	s := bench.NewSearch(spec.Steps(), spec.QualityToIndex(t.Quality))
	// One step is worth less than a few seconds of sample can measure.
	s.WithinStep = true
	trialJob := func(c judging, q float64) *ffmpeg.Job {
		tr := *t
		tr.Quality = q
		tr.Encoder.CapBitrate = false
		return tr.Job(plan.Source{Label: "tune", Path: c.sample, ID: "sample"}, c.dir, toneMap)
	}
	for i := range clips {
		clips[i].key.args = withoutQuality(trialJob(clips[i], 0).Args, t.Encoder.QualityFlag)
	}
	// lookup returns the trial at q if every clip's is recorded.
	lookup := func(q float64) (trial, []trial, bool) {
		var each []trial
		for _, c := range clips {
			p, ok := past.lookup(c.key, q)
			if !ok {
				return trial{}, nil, false
			}
			each = append(each, p)
		}
		return combine(each), each, true
	}

	// Start from the closest bracket the past trials give, judged
	// against this target: the worst passing setting and the best
	// failing one below it.
	seen := map[int]trial{}
	pass, fail := -1, -1
	for i := 0; i <= spec.Steps(); i++ {
		if p, _, ok := lookup(spec.IndexToQuality(i)); ok {
			seen[i] = p
			if p.vmaf >= goal {
				pass = i
			}
		}
	}
	for i := range seen {
		if seen[i].vmaf < goal && i > pass && (fail < 0 || i < fail) {
			fail = i
		}
	}
	for _, i := range []int{pass, fail} {
		if i >= 0 {
			s.Observe(i, seen[i].vmaf >= goal, seen[i].vmaf-goal)
		}
	}

	encodes := 0
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
				VMAF: p.vmaf, Target: goal, Model: clips[0].ref.model.Name(), Outcome: outcome, Encodes: encodes, Cached: len(seen) - encodes,
			}
			log.Info("Tuned", "quality", res.Quality, "vmaf", p.vmaf,
				"bitrate", FormatBitrate(res.Bitrate), "outcome", outcome, "encodes", encodes, "cached", res.Cached)
			return res, p.bitrate, nil
		}

		q := spec.IndexToQuality(i)
		p, cached := seen[i]
		var each []trial
		if !cached {
			for _, c := range clips {
				ct, ok := past.lookup(c.key, q)
				if !ok {
					var err error
					if ct, err = tu.trial(ctx, trialJob(c, q), c.sample, c.seconds, c.ref, prefilter, c.dir); err != nil {
						return Result{}, 0, err
					}
					if err := past.record(c.key, t.Name, q, ct); err != nil {
						return Result{}, 0, fmt.Errorf("recording trial: %w", err)
					}
				}
				each = append(each, ct)
			}
			encodes++
			p = combine(each)
			seen[i] = p
		} else if len(clips) > 1 {
			_, each, _ = lookup(q)
		}
		margin := p.vmaf - goal
		attrs := []any{"quality", q, "vmaf", p.vmaf, "pass", margin >= 0, "cached", cached}
		if len(clips) > 1 && each != nil {
			vmafs := make([]float64, len(each))
			for j, e := range each {
				vmafs[j] = e.vmaf
			}
			attrs = append(attrs, "ranges", vmafs)
		}
		log.Info("Trial", attrs...)
		s.Observe(i, margin >= 0, margin)
	}
}

// combine is a setting's trial over several clips: its worst VMAF,
// since it passes only if every clip does, and its highest bitrate, so
// a cap taken from it holds for the hardest clip.
func combine(each []trial) trial {
	c := each[0]
	for _, e := range each[1:] {
		c.vmaf = min(c.vmaf, e.vmaf)
		c.bitrate = max(c.bitrate, e.bitrate)
	}
	return c
}

// trial encodes the sample with job and scores it against ref.
func (tu *Tuner) trial(ctx context.Context, job *ffmpeg.Job, sample string, seconds float64, ref reference, prefilter, workdir string) (trial, error) {
	if err := tu.makeReference(ctx, sample, ref); err != nil {
		return trial{}, err
	}
	out := job.Output
	args := append([]string{"-i", sample}, job.Args...)
	if err := tu.ffmpeg(ctx, append(args, out)...); err != nil {
		return trial{}, err
	}
	st, err := os.Stat(out)
	if err != nil {
		return trial{}, err
	}
	m, err := bench.Score(ctx, tu.Exec, out, ref.path, ref.width, ref.height, tu.Threads, workdir, filepath.Base(out), prefilter, ref.transfer, ref.model)
	os.Remove(out)
	if err != nil {
		return trial{}, err
	}
	return trial{vmaf: m.VMAF, bitrate: float64(st.Size()) * 8 / seconds}, nil
}

// withoutQuality returns args with the value after the last flag
// replaced by a placeholder, so trials at every quality share a key.
func withoutQuality(args []string, flag string) []string {
	out := slices.Clone(args)
	for i := len(out) - 2; i >= 0; i-- {
		if out[i] == flag {
			out[i+1] = "{quality}"
			break
		}
	}
	return out
}

func (tu *Tuner) ffmpeg(ctx context.Context, args ...string) error {
	argv := slices.Concat([]string{"ffmpeg", "-hide_banner", "-loglevel", "error", "-nostats", "-y"}, args)
	_, err := tu.Exec.Exec(ctx, argv)
	return err
}
