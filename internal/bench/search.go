package bench

import (
	"math"
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
	// Found means the answer is the passing end of a one-step bracket.
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
// It brackets the boundary by stepping outward from a seed with a
// doubling stride, then narrows the bracket by interpolating between
// the margins at its ends (regula falsi), bisecting whenever the same
// end moves twice in a row, which is when interpolation stalls.
type Search struct {
	n      int
	seed   int
	stride int

	lo, hi             int // highest known pass, lowest known fail; -1 if unknown
	loMargin, hiMargin float64
	lastSide           int // +1 lo moved last, -1 hi moved last
	repeats            int // consecutive moves of the same end
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

// Next returns the index to measure next, or the outcome when done. For
// Found and AllPass, Answer gives the result.
func (s *Search) Next() (int, Outcome) {
	switch {
	case s.lo < 0 && s.hi < 0:
		return s.seed, Searching
	case s.hi < 0:
		if s.lo == s.n {
			return 0, AllPass
		}
		return min(s.lo+s.stride, s.n), Searching
	case s.lo < 0:
		if s.hi == 0 {
			return 0, NonePass
		}
		return max(s.hi-s.stride, 0), Searching
	case s.hi-s.lo <= 1:
		return 0, Found
	}

	mid := (s.lo + s.hi) / 2
	if s.repeats >= 2 {
		return mid, Searching
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
