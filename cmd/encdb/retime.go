package main

import (
	"path/filepath"
	"slices"
	"strings"
)

// answerKey groups a search's rows: its answer is the highest quality
// setting that passed.
type answerKey struct{ sample, flagset, scorer string }

// answers is the index of each answer (VMAF >= 95 against the sample
// itself, highest passing quality; ties go to the later row) among
// rows for which keep is true.
func answers(rows []row, keep func(*row) bool) map[answerKey]int {
	best := map[answerKey]int{}
	for i := range rows {
		r := &rows[i]
		if !keep(r) || r.Reference != r.Sample || r.VMAF == nil || *r.VMAF < 95 {
			continue
		}
		k := answerKey{r.Sample, r.Flagset, r.Scorer}
		if j, ok := best[k]; !ok || r.Quality >= rows[j].Quality {
			best[k] = i
		}
	}
	return best
}

// retime gives a host's answers the times of a later timing run, in
// place, and returns how many it changed. A timing run (rt.Log, such as
// render1's after a memory upgrade) may re-time just one of several
// flagsets whose encodes are identical; each earlier answer whose
// output file matches one of the run's answers (same sample, scorer,
// encoder and variant, and the same SHA-256, or the same size where
// either lacks one) takes that answer's wall, user and system times,
// preferring the run's answer for the same flagset. Variants must match
// because the same file can take different work to make (hardware
// decoding, say), except for rt.SameTime's tokens. Only rt.Families'
// encoders are re-timed, if it names any. Changed rows' log column says
// so.
func retime(rows []row, rt Retime) int {
	in := func(r *row) bool {
		return r.Host == rt.Host &&
			(len(rt.Families) == 0 || slices.Contains(rt.Families, attributes(r.Encoder, r.Flagset, r.Args).Family))
	}
	old := answers(rows, func(r *row) bool { return in(r) && r.From != rt.Log })
	fresh := answers(rows, func(r *row) bool { return in(r) && r.From == rt.Log })
	type group struct{ sample, scorer, encoder, variant string }
	groupOf := func(r *row) group {
		v := strings.Split(attributes(r.Encoder, r.Flagset, r.Args).Variant, "-")
		v = slices.DeleteFunc(v, func(t string) bool { return t == "default" || slices.Contains(rt.SameTime, t) })
		return group{r.Sample, r.Scorer, r.Encoder, strings.Join(v, "-")}
	}
	byGroup := map[group][]int{}
	for _, i := range fresh {
		g := groupOf(&rows[i])
		byGroup[g] = append(byGroup[g], i)
	}
	n := 0
	note := " (times: " + filepath.Base(rt.Log) + ")"
	for _, i := range old {
		r := &rows[i]
		match := -1
		for _, j := range byGroup[groupOf(r)] {
			f := &rows[j]
			if !sameFile(r, f) {
				continue
			}
			if match < 0 || f.Flagset == r.Flagset {
				match = j
			}
		}
		if match < 0 {
			continue
		}
		f := &rows[match]
		if r.WallS == f.WallS && r.UserS == f.UserS && r.SysS == f.SysS {
			continue
		}
		r.WallS, r.UserS, r.SysS = f.WallS, f.UserS, f.SysS
		r.From += note
		n++
	}
	return n
}

// sameFile says if a and b encoded the same file: the first encodes
// predate encbench's output hash, so size stands in for it.
func sameFile(a, b *row) bool {
	if a.OutSHA256 != "" && b.OutSHA256 != "" {
		return a.OutSHA256 == b.OutSHA256
	}
	return a.Bytes == b.Bytes
}
