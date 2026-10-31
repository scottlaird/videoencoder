package bench

import (
	"cmp"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
)

type pair struct{ sample, fkey string }

// Best is the answer for one (sample, flagset): its smallest passing
// encode.
type Best struct {
	Row
	Name    string // current flagset name
	Outcome Outcome
	Encodes int // logged encodes for this pair, i.e. what the answer cost
	// ColorFlag names the color measures ("shift", "flicker") on which
	// this answer stands out from the sample's other answers; see
	// flagColor. Empty if none.
	ColorFlag string
}

// Color outliers: an answer is flagged when its measure is at least
// outlierRatio times the median of its sample's answers and exceeds it
// by at least the absolute margin, so that a sample whose answers all
// measure near zero flags nothing. Neither measure is a good pass
// criterion on its own: flicker reads camera and subject motion as
// flicker, and shift varies with the encoder's speed preset on fast
// motion. They are flags for a look by eye, as the AV1 cast on Pyramid
// Creek needed.
const (
	outlierRatio         = 1.5
	shiftOutlierMargin   = 0.75
	flickerOutlierMargin = 0.3
)

// flagColor sets ColorFlag on each answer that is a color outlier
// within its sample.
func flagColor(bests []Best) {
	bySample := map[string][]int{}
	for i, b := range bests {
		bySample[b.Sample] = append(bySample[b.Sample], i)
	}
	for _, idx := range bySample {
		shift := median(idx, bests, func(m Metrics) float64 { return m.ColorShiftP999 })
		flicker := median(idx, bests, func(m Metrics) float64 { return m.ColorFlickerP999 })
		for _, i := range idx {
			m := bests[i].Metrics
			var flags []string
			if outlier(m.ColorShiftP999, shift, shiftOutlierMargin) {
				flags = append(flags, "shift")
			}
			if outlier(m.ColorFlickerP999, flicker, flickerOutlierMargin) {
				flags = append(flags, "flicker")
			}
			bests[i].ColorFlag = strings.Join(flags, "+")
		}
	}
}

func median(idx []int, bests []Best, get func(Metrics) float64) float64 {
	var xs []float64
	for _, i := range idx {
		if v := get(bests[i].Metrics); !math.IsNaN(v) {
			xs = append(xs, v)
		}
	}
	if len(xs) == 0 {
		return math.NaN()
	}
	slices.Sort(xs)
	if n := len(xs); n%2 == 0 {
		return (xs[n/2-1] + xs[n/2]) / 2
	}
	return xs[len(xs)/2]
}

func outlier(v, median, margin float64) bool {
	return !math.IsNaN(v) && !math.IsNaN(median) && v >= outlierRatio*median && v-median >= margin
}

// bestByPair finds, for every sample and current flagset, the passing
// encode with the worst quality setting under the current criteria.
func bestByPair(c *Config, results *Results) map[pair]Row {
	byKey := map[string]*Flagset{}
	for _, name := range c.FlagsetNames() {
		byKey[flagsetKey(c.Flagsets[name])] = c.Flagsets[name]
	}
	best := map[pair]Row{}
	for _, r := range results.Rows {
		fs, ok := byKey[r.FlagsetKey]
		if !ok || r.Scorer != scorer || c.margin(r.Metrics) < 0 {
			continue
		}
		p := pair{r.Sample, r.FlagsetKey}
		cur, seen := best[p]
		if !seen || fs.Quality.qualityToIndex(r.Quality) > fs.Quality.qualityToIndex(cur.Quality) {
			best[p] = r
		}
	}
	return best
}

// Bests returns the answers for the current flagsets, sorted by sample
// and then by size.
func Bests(c *Config, results *Results) []Best {
	names := map[string]string{}
	for _, name := range c.FlagsetNames() {
		names[flagsetKey(c.Flagsets[name])] = name
	}
	counts := map[pair]int{}
	for _, r := range results.Rows {
		if r.Scorer == scorer {
			counts[pair{r.Sample, r.FlagsetKey}]++
		}
	}

	var out []Best
	for p, r := range bestByPair(c, results) {
		name := names[p.fkey]
		q := c.Flagsets[name].Quality
		outcome := Found
		if q.qualityToIndex(r.Quality) == q.steps() {
			outcome = AllPass
		}
		out = append(out, Best{Row: r, Name: name, Outcome: outcome, Encodes: counts[p]})
	}
	slices.SortFunc(out, func(a, b Best) int {
		return cmp.Or(cmp.Compare(a.Sample, b.Sample), cmp.Compare(a.Bytes, b.Bytes), cmp.Compare(a.Name, b.Name))
	})
	flagColor(out)
	return out
}

var bestColumns = []string{
	"sample", "flagset", "encoder", "args", "quality_flag", "quality", "outcome", "encodes",
	"bytes", "kbps", "wall_s", "user_s", "vmaf", "vmaf_neg", "vmaf_p1", "psnr_cb", "psnr_cr", "ciede2000",
	"color_shift_p99", "color_shift_p999", "color_flicker_p99", "color_flicker_p999", "color_flag",
}

// WriteBest rewrites best.csv from the log.
func WriteBest(path string, c *Config, results *Results) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	w.Write(bestColumns)
	for _, b := range Bests(c, results) {
		m := b.Metrics
		w.Write([]string{
			b.Sample, b.Name, b.Encoder, b.Args, b.QualityFlag, ftoa(b.Quality), b.Outcome.String(),
			strconv.Itoa(b.Encodes), strconv.FormatInt(b.Bytes, 10), ftoa(b.Kbps), ftoa(b.WallS), ftoa(b.UserS),
			ftoa(m.VMAF), ftoa(m.VMAFNeg), ftoa(m.VMAFP1), ftoa(m.PSNRCb), ftoa(m.PSNRCr), ftoa(m.CIEDE2000),
			ftoa(m.ColorShiftP99), ftoa(m.ColorShiftP999), ftoa(m.ColorFlickerP99), ftoa(m.ColorFlickerP999), b.ColorFlag,
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Report writes the answers as Markdown tables, one per sample.
func Report(w io.Writer, c *Config, results *Results) {
	sample := ""
	for _, b := range Bests(c, results) {
		if b.Sample != sample {
			sample = b.Sample
			fmt.Fprintf(w, "\n### %s\n\n", sample)
			fmt.Fprintln(w, "| flags | kbps | wall s | user s | vmaf | vmaf_neg | vmaf p1 | ciede2000 | color shift p99.9 | color flicker p99.9 | look at | encodes |")
			fmt.Fprintln(w, "|:--|--:|--:|--:|--:|--:|--:|--:|--:|--:|:--|--:|")
		}
		flags := fmt.Sprintf("`%s %s %s`", b.Args, b.QualityFlag, ftoa(b.Quality))
		if b.Outcome == AllPass {
			flags += " (range limit)"
		}
		m := b.Metrics
		fmt.Fprintf(w, "| %s | %.0f | %.2f | %.2f | %.3f | %.3f | %.3f | %.2f | %.2f | %.2f | %s | %d |\n",
			flags, b.Kbps, b.WallS, b.UserS, m.VMAF, m.VMAFNeg, m.VMAFP1, m.CIEDE2000, m.ColorShiftP999, m.ColorFlickerP999, b.ColorFlag, b.Encodes)
	}
}
