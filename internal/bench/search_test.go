package bench

import (
	"math"
	"testing"
)

// runSearch drives a Search against score(i) with threshold 95 and
// returns the outcome, answer and number of measurements taken.
func runSearch(t *testing.T, n, seed int, score func(int) float64) (Outcome, int, int) {
	t.Helper()
	s := NewSearch(n, seed)
	seen := map[int]bool{}
	for evals := 0; evals < 100; {
		i, out := s.Next()
		if out != Searching {
			return out, s.Answer(), evals
		}
		if seen[i] {
			t.Fatalf("search measured index %d twice", i)
		}
		seen[i] = true
		evals++
		m := score(i) - 95
		s.Observe(i, m >= 0, m)
	}
	t.Fatal("search did not finish in 100 measurements")
	return 0, 0, 0
}

func TestSearch(t *testing.T) {
	linear := func(i int) float64 { return 100 - 0.05*float64(i) } // passes up to i=100
	curved := func(i int) float64 { return 100 - 5*math.Pow(float64(i)/180, 2) }
	// NVENC-style plateaus: groups of 7 indexes share one score.
	plateau := func(i int) float64 { return 100 - 0.3*float64(i/7) }

	tests := []struct {
		name     string
		n, seed  int
		score    func(int) float64
		want     Outcome
		answer   int
		maxEvals int
	}{
		{name: "linear from middle", n: 300, seed: 150, score: linear, want: Found, answer: 100, maxEvals: 8},
		{name: "linear from good seed", n: 300, seed: 98, score: linear, want: Found, answer: 100, maxEvals: 6},
		{name: "linear from bottom", n: 300, seed: 0, score: linear, want: Found, answer: 100, maxEvals: 10},
		{name: "curved", n: 300, seed: 150, score: curved, want: Found, answer: 180, maxEvals: 9},
		{name: "plateaus", n: 300, seed: 150, score: plateau, want: Found, answer: 118, maxEvals: 10},
		{name: "integer range", n: 53, seed: 26, score: func(i int) float64 { return 100 - float64(i)/3 }, want: Found, answer: 15, maxEvals: 6},
		{name: "everything passes", n: 50, seed: 25, score: func(int) float64 { return 99 }, want: AllPass, answer: 50, maxEvals: 6},
		{name: "nothing passes", n: 50, seed: 25, score: func(int) float64 { return 90 }, want: NonePass, answer: -1, maxEvals: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, answer, evals := runSearch(t, tt.n, tt.seed, tt.score)
			if out != tt.want || answer != tt.answer {
				t.Errorf("got %v at %d, want %v at %d", out, answer, tt.want, tt.answer)
			}
			if evals > tt.maxEvals {
				t.Errorf("took %d measurements, want at most %d", evals, tt.maxEvals)
			}
		})
	}
}

func TestQualityIndex(t *testing.T) {
	crf := Quality{Min: 10, Max: 40, Step: 0.1, HigherIsBetter: ptr(false)}
	vt := Quality{Min: 1, Max: 100, Step: 1, HigherIsBetter: ptr(true)}
	tests := []struct {
		q     Quality
		i     int
		value float64
	}{
		{crf, 0, 10},
		{crf, 87, 18.7},
		{crf, 300, 40},
		{vt, 0, 100},
		{vt, 35, 65},
		{vt, 99, 1},
	}
	for _, tt := range tests {
		if got := tt.q.indexToQuality(tt.i); got != tt.value {
			t.Errorf("indexToQuality(%d) = %v, want %v", tt.i, got, tt.value)
		}
		if got := tt.q.qualityToIndex(tt.value); got != tt.i {
			t.Errorf("qualityToIndex(%v) = %d, want %d", tt.value, got, tt.i)
		}
	}
	if got := crf.qualityToIndex(99); got != 300 {
		t.Errorf("qualityToIndex(out of range) = %d, want clamped 300", got)
	}
}
