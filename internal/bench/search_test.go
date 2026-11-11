package bench

import (
	"math"
	"testing"
)

// runSearch drives a Search against score(i) with threshold 95 and
// returns the outcome, answer and number of measurements taken.
func runSearch(t *testing.T, n, seed int, withinStep bool, score func(int) float64) (Outcome, int, int) {
	t.Helper()
	s := NewSearch(n, seed)
	s.WithinStep = withinStep
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
	// nooksack2's av1-2160 trials (CRF 10-63, so index = CRF-10): 32
	// 95.885, 35 95.386, 37 94.9905, 41 94.135, and a parabola through
	// them elsewhere.
	nooksack := map[int]float64{22: 95.885, 25: 95.386, 27: 94.9905, 31: 94.135, 26: 95.1}
	av1 := func(i int) float64 {
		if v, ok := nooksack[i]; ok {
			return v
		}
		x := float64(i)
		return 96.92 - 0.0213*x - 0.00131*x*x
	}
	// nooksack2's h265-480 trials (CRF 10-40 in tune's 0.5 steps, so
	// index = 2*(CRF-10)): 20 93.48, 18.5 94.10, 16 94.91, 15.5 95.049,
	// joined by lines.
	h265 := func(i int) float64 {
		xs := []float64{11, 12, 17, 20}
		ys := []float64{95.049, 94.91, 94.10, 93.48}
		k := 1
		for k < len(xs)-1 && float64(i) > xs[k] {
			k++
		}
		return ys[k-1] + (float64(i)-xs[k-1])*(ys[k]-ys[k-1])/(xs[k]-xs[k-1])
	}

	tests := []struct {
		name     string
		n, seed  int
		score    func(int) float64
		step     bool
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
		{name: "nooksack2 av1-2160", n: 53, seed: 22, score: av1, want: Found, answer: 26, maxEvals: 4},
		{name: "nooksack2 h265-480", n: 60, seed: 20, score: h265, want: Found, answer: 11, maxEvals: 5},
		{name: "nooksack2 h265-480 within a step", n: 60, seed: 20, score: h265, step: true, want: Found, answer: 11, maxEvals: 4},
		{name: "within a step still closes a wide bracket", n: 53, seed: 22, score: av1, step: true, want: Found, answer: 26, maxEvals: 4},
		{name: "everything passes", n: 50, seed: 25, score: func(int) float64 { return 99 }, want: AllPass, answer: 50, maxEvals: 6},
		{name: "nothing passes", n: 50, seed: 25, score: func(int) float64 { return 90 }, want: NonePass, answer: -1, maxEvals: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, answer, evals := runSearch(t, tt.n, tt.seed, tt.step, tt.score)
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

func TestSearchAt(t *testing.T) {
	linear := func(i int) float64 { return 100 - 0.05*float64(i) } // passes up to i=100
	tests := []struct {
		name      string
		seed      int
		wantEvals int
	}{
		{"right", 100, 2},
		{"one high", 101, 2},
		{"one low", 99, 3},
		{"far off", 140, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSearchAt(300, tt.seed)
			evals := 0
			for {
				i, out := s.Next()
				if out != Searching {
					if out != Found || s.Answer() != 100 {
						t.Fatalf("got %v, answer %d; want found, 100", out, s.Answer())
					}
					break
				}
				if evals++; evals > 50 {
					t.Fatal("search did not finish")
				}
				m := linear(i) - 95
				s.Observe(i, m >= 0, m)
			}
			if evals > tt.wantEvals {
				t.Errorf("took %d measurements, want at most %d", evals, tt.wantEvals)
			}
		})
	}
}

// runBatch drives a Search k indexes per round against score(i), with
// threshold 95, and returns the outcome, answer, rounds and
// measurements taken.
func runBatch(t *testing.T, s *Search, k int, score func(int) float64) (Outcome, int, int, int) {
	t.Helper()
	seen := map[int]bool{}
	evals := 0
	for rounds := 0; rounds < 100; rounds++ {
		pts, out := s.NextBatch(k)
		if out != Searching {
			return out, s.Answer(), rounds, evals
		}
		if len(pts) == 0 || len(pts) > k {
			t.Fatalf("round %d: %d indexes, want 1..%d", rounds+1, len(pts), k)
		}
		for _, i := range pts {
			if i < 0 || i > s.n {
				t.Fatalf("index %d out of [0, %d]", i, s.n)
			}
			if seen[i] {
				t.Fatalf("index %d measured twice", i)
			}
			seen[i] = true
			evals++
		}
		// The round's results arrive together, in any order.
		for j := len(pts) - 1; j >= 0; j-- {
			m := score(pts[j]) - 95
			s.Observe(pts[j], m >= 0, m)
		}
	}
	t.Fatal("search did not finish in 100 rounds")
	return 0, 0, 0, 0
}

func TestSearchBatch(t *testing.T) {
	scores := map[string]func(int) float64{
		"linear":  func(i int) float64 { return 100 - 0.05*float64(i) },               // passes up to 100
		"curved":  func(i int) float64 { return 100 - 5*math.Pow(float64(i)/180, 2) }, // up to 180
		"plateau": func(i int) float64 { return 100 - 0.3*float64(i/7) },              // up to 118
		"steep":   func(i int) float64 { return 100 - 0.002*float64(i)*float64(i) },   // up to 50
		"low":     func(i int) float64 { return 95.5 - 0.5*float64(i) },               // up to 1
	}
	for name, score := range scores {
		for _, seed := range []int{0, 40, 150, 290} {
			want, wantAns, _ := runSearch(t, 300, seed, false, score)
			_, _, single, _ := runBatch(t, NewSearch(300, seed), 1, score)
			for _, k := range []int{1, 2, 4, 8} {
				out, ans, rounds, _ := runBatch(t, NewSearch(300, seed), k, score)
				if out != want || ans != wantAns {
					t.Errorf("%s seed %d k=%d: %v %d, want %v %d (as one at a time)", name, seed, k, out, ans, want, wantAns)
				}
				if k >= 4 && rounds > single {
					t.Errorf("%s seed %d k=%d: %d rounds, more than one at a time's %d", name, seed, k, rounds, single)
				}
			}
		}
	}
}

func TestSearchBatchAt(t *testing.T) {
	// A seed at the answer, or one off, closes in one round of 4.
	linear := func(i int) float64 { return 100 - 0.05*float64(i) } // passes up to 100
	for _, seed := range []int{99, 100, 101} {
		out, ans, rounds, _ := runBatch(t, NewSearchAt(300, seed), 4, linear)
		if out != Found || ans != 100 || rounds != 1 {
			t.Errorf("seed %d: %v %d in %d rounds, want found 100 in 1", seed, out, ans, rounds)
		}
	}
}

// bandVMAF is hevc_nvenc -preset p7 -tune uhq on band at 1080p, as an
// RTX 4090 measured it: (cq, VMAF), with NVENC's plateaus, where nearby
// cq values give identical encodes.
var bandVMAF = [][2]float64{{32.6, 96.093}, {33.7, 95.241}, {33.8, 95.012}, {33.9, 95.012}, {34.0, 94.803},
	{34.2, 94.803}, {34.3, 94.609}, {34.7, 94.278}, {34.8, 94.043}, {35.7, 93.136}}

// bandScore interpolates bandVMAF at index i, cq 1+0.1i, as for -cq:v
// from 1 in steps of 0.1.
func bandScore(i int) float64 {
	cq := 1 + 0.1*float64(i)
	p := bandVMAF
	if cq <= p[0][0] {
		return p[0][1] + (p[0][0]-cq)*0.8
	}
	for j := 1; j < len(p); j++ {
		if cq <= p[j][0]+1e-9 {
			f := (cq - p[j-1][0]) / (p[j][0] - p[j-1][0])
			return p[j-1][1] + f*(p[j][1]-p[j-1][1])
		}
	}
	return p[len(p)-1][1] - (cq-p[len(p)-1][0])*1.0
}

func TestSearchBatchBand(t *testing.T) {
	// The bracket as encbench had it: cq 32.6 passes and 35.7 fails.
	// Interpolation guesses 33.7, two steps short of the answer on a
	// plateau. Points clustered beside the guess took 4 more rounds;
	// spread out, two settings at a time close it in 2.
	s := NewSearch(500, 256)
	for _, i := range []int{347, 316} { // 35.7, 32.6
		m := bandScore(i) - 95
		s.Observe(i, m >= 0, m)
	}
	out, ans, rounds, _ := runBatch(t, s, 2, bandScore)
	if out != Found || ans != 329 || rounds > 2 { // 33.9
		t.Errorf("%v cq %.1f in %d rounds, want found 33.9 in at most 2", out, 1+0.1*float64(ans), rounds)
	}
}
