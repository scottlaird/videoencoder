package bench

import (
	"math"
	"slices"
)

// The search works on step indexes rather than quality values. Index 0
// is the best quality in range and index n the worst, whichever way the
// encoder's numbers run, so "passes" is true for low indexes and the
// answer is the highest passing index.

// Outcome says how a search ended.
type Outcome int

const (
	// Searching means the search needs more measurements.
	Searching Outcome = iota
	// Found means the answer is the passing end of a one-step bracket,
	// or (with WithinStep) a pass whose next step is expected to fail.
	Found
	// AllPass means even the worst quality in range passes; widen the
	// range to find the real answer.
	AllPass
	// NonePass means even the best quality in range fails.
	NonePass
)

func (o Outcome) String() string {
	return [...]string{"searching", "found", "all-pass", "none-pass"}[o]
}

// Search finds the worst passing quality index in [0, n], assuming
// passing is (roughly) monotonic in the index.
//
// Every measurement is expensive, so each step guesses where the margin
// crosses zero from the margins seen so far. Until both sides of the
// boundary are known, it extrapolates the line through the two
// measurements nearest the boundary, limited to twice a stride that
// doubles with each step, and steps by that stride when the line is no
// use (one point, a flat stretch, or noise sloping the wrong way). Once
// the boundary is bracketed, it interpolates: a parabola through the
// bracket's ends and the nearest other measurement, since margins fall
// faster as quality drops, or else a line between the ends (regula
// falsi). It bisects whenever the same end moves twice in a row, which
// is when interpolation stalls.
type Search struct {
	// WithinStep ends the search at a pass whose margin is smaller than
	// the drop expected over one more step, judged from the slope to the
	// nearest other measurement, instead of measuring that step to be
	// sure it fails. Otherwise the search always closes the bracket.
	WithinStep bool

	n      int
	seed   int
	stride int

	lo, hi             int // highest known pass, lowest known fail; -1 if unknown
	loMargin, hiMargin float64
	lastSide           int // +1 lo moved last, -1 hi moved last
	repeats            int // consecutive moves of the same end

	seen []measured // measurements with finite margins, by index
}

// measured is one measurement: an index and its margin.
type measured struct {
	i int
	m float64
}

// NewSearch searches indexes [0, n] starting from seed.
func NewSearch(n, seed int) *Search {
	return &Search{
		n:      n,
		seed:   min(max(seed, 0), n),
		stride: max(1, n/16),
		lo:     -1,
		hi:     -1,
	}
}

// NewSearchAt is NewSearch for a seed that is probably the answer
// already, such as another machine's: it measures the seed's neighbor
// first and widens only if that doesn't close the bracket, so a right
// guess takes two measurements.
func NewSearchAt(n, seed int) *Search {
	s := NewSearch(n, seed)
	s.stride = 1
	return s
}

// Next returns the index to measure next, or the outcome when done. For
// Found and AllPass, Answer gives the result.
func (s *Search) Next() (int, Outcome) {
	switch {
	case s.lo < 0 && s.hi < 0:
		return s.seed, Searching
	case s.WithinStep && s.nextFails():
		return 0, Found
	case s.hi < 0:
		if s.lo == s.n {
			return 0, AllPass
		}
		next := min(s.lo+s.stride, s.n)
		if x, ok := s.extrapolate(len(s.seen)-2, len(s.seen)-1); ok {
			next = clampRound(x, s.lo+1, min(s.lo+2*s.stride, s.n))
		}
		return next, Searching
	case s.lo < 0:
		if s.hi == 0 {
			return 0, NonePass
		}
		next := max(s.hi-s.stride, 0)
		if x, ok := s.extrapolate(0, 1); ok {
			next = clampRound(x, max(s.hi-2*s.stride, 0), s.hi-1)
		}
		return next, Searching
	case s.hi-s.lo <= 1:
		return 0, Found
	}

	mid := (s.lo + s.hi) / 2
	if s.repeats >= 2 {
		return mid, Searching
	}
	if x, ok := s.parabola(); ok {
		return clampRound(x, s.lo+1, s.hi-1), Searching
	}
	// The margin is how far a measurement is above the threshold, so it
	// is >= 0 at lo and < 0 at hi; interpolate to where it crosses zero.
	frac := s.loMargin / (s.loMargin - s.hiMargin)
	if math.IsNaN(frac) || math.IsInf(frac, 0) {
		return mid, Searching
	}
	i := s.lo + int(math.Round(frac*float64(s.hi-s.lo)))
	return min(max(i, s.lo+1), s.hi-1), Searching
}

// extrapolate returns where the line through measurements a and b (as
// indexes into s.seen) crosses zero, if both exist and the line falls
// as the index rises, as margins do.
func (s *Search) extrapolate(a, b int) (float64, bool) {
	if a < 0 || b >= len(s.seen) || a == b {
		return 0, false
	}
	p, q := s.seen[a], s.seen[b]
	slope := (q.m - p.m) / float64(q.i-p.i)
	if !(slope < 0) {
		return 0, false
	}
	return float64(p.i) - p.m/slope, true
}

// nextFails reports whether the line from lo to the measurement nearest
// it falls below zero one step past lo.
func (s *Search) nextFails() bool {
	if s.lo < 0 || s.lo == s.n {
		return false
	}
	var lo, near measured
	dist := -1
	for _, p := range s.seen {
		if p.i == s.lo {
			lo = p
		} else if d := abs(p.i - s.lo); dist < 0 || d < dist {
			near, dist = p, d
		}
	}
	if lo.i != s.lo || dist < 0 {
		return false
	}
	slope := (near.m - lo.m) / float64(near.i-lo.i)
	return slope < 0 && lo.m+slope < 0
}

// parabola returns where the parabola through the bracket's ends and
// the measurement nearest them crosses zero, if it falls across the
// whole bracket and so crosses exactly once inside it.
func (s *Search) parabola() (float64, bool) {
	var ends [2]measured
	found := 0
	third, dist := measured{}, -1
	for _, p := range s.seen {
		switch {
		case p.i == s.lo:
			ends[0] = p
			found++
		case p.i == s.hi:
			ends[1] = p
			found++
		default:
			d := min(abs(p.i-s.lo), abs(p.i-s.hi))
			if dist < 0 || d < dist {
				third, dist = p, d
			}
		}
	}
	if found != 2 || dist < 0 {
		return 0, false
	}
	// Newton's form through x0, x1, x2, then the usual coefficients.
	x0, x1, x2 := float64(ends[0].i), float64(ends[1].i), float64(third.i)
	d01 := (ends[1].m - ends[0].m) / (x1 - x0)
	d12 := (third.m - ends[1].m) / (x2 - x1)
	a := (d12 - d01) / (x2 - x0)
	b := d01 - a*(x0+x1)
	c := ends[0].m - d01*x0 + a*x0*x1
	slope := func(x float64) float64 { return 2*a*x + b }
	if !(slope(x0) < 0 && slope(x1) < 0) {
		return 0, false
	}
	if math.Abs(a) < 1e-12 {
		return -c / b, true
	}
	disc := b*b - 4*a*c
	if disc < 0 {
		return 0, false
	}
	for _, x := range []float64{(-b - math.Sqrt(disc)) / (2 * a), (-b + math.Sqrt(disc)) / (2 * a)} {
		if x > x0 && x < x1 {
			return x, true
		}
	}
	return 0, false
}

// clampRound rounds x to the nearest index in [lo, hi].
func clampRound(x float64, lo, hi int) int {
	return min(max(int(math.Round(x)), lo), hi)
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}

// Observe records whether index i passed, and by how much (margin >= 0
// when it passed, < 0 when it failed).
func (s *Search) Observe(i int, pass bool, margin float64) {
	side := -1
	if pass {
		side = 1
		if s.hi < 0 && s.lo >= 0 {
			s.stride *= 2
		}
		if s.lo < 0 || i > s.lo {
			s.lo, s.loMargin = i, margin
		}
	} else {
		if s.lo < 0 && s.hi >= 0 {
			s.stride *= 2
		}
		if s.hi < 0 || i < s.hi {
			s.hi, s.hiMargin = i, margin
		}
	}
	if !math.IsNaN(margin) && !math.IsInf(margin, 0) {
		at, _ := slices.BinarySearchFunc(s.seen, i, func(p measured, i int) int { return p.i - i })
		if at < len(s.seen) && s.seen[at].i == i {
			s.seen[at].m = margin
		} else {
			s.seen = slices.Insert(s.seen, at, measured{i, margin})
		}
	}
	if side == s.lastSide {
		s.repeats++
	} else {
		s.repeats = 1
	}
	s.lastSide = side
}

// Answer is the worst passing index found so far, or -1 if none.
func (s *Search) Answer() int { return s.lo }

// indexToQuality converts a step index to the encoder's quality value.
func (q Quality) indexToQuality(i int) float64 {
	v := float64(i) * q.Step
	if *q.HigherIsBetter {
		return roundTo(q.Max-v, q.Step)
	}
	return roundTo(q.Min+v, q.Step)
}

// qualityToIndex is the inverse of indexToQuality, rounding to the
// nearest step and clamping to the range.
func (q Quality) qualityToIndex(v float64) int {
	var i float64
	if *q.HigherIsBetter {
		i = (q.Max - v) / q.Step
	} else {
		i = (v - q.Min) / q.Step
	}
	return min(max(int(math.Round(i)), 0), q.steps())
}

// roundTo rounds v to a multiple of step, with the float noise of
// decimal steps like 0.1 removed.
func roundTo(v, step float64) float64 {
	r := math.Round(v/step) * step
	return math.Round(r*1e6) / 1e6
}
