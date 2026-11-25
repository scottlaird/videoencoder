package score

import (
	"encoding/json"
	"net/http"
	"time"
)

// ScoresFormat is the version of the /api/scores document. It changes
// only if a field is renamed, removed or changes meaning.
const ScoresFormat = 1

// ScoresDoc is the /api/scores document: every score in the log, in
// log order.
type ScoresDoc struct {
	Format int        `json:"format"`
	Scores []ScoreRow `json:"scores"`
}

// ScoreRow is a Score, with scores.csv's column names.
type ScoreRow struct {
	Key       string    `json:"key"`
	Scorer    string    `json:"scorer"`
	Metric    string    `json:"metric"`
	Value     float64   `json:"value"`
	SampleID  string    `json:"sample_id"`
	SourceID  string    `json:"source_id"`
	OutSHA256 string    `json:"out_sha256"`
	ScoreS    float64   `json:"score_s"`
	Host      string    `json:"host"`
	Device    string    `json:"device"`
	Time      time.Time `json:"time"`
	From      string    `json:"from"`
	Sample    string    `json:"sample"`
	Flagset   string    `json:"flagset"`
	Quality   float64   `json:"quality"`
}

// Handler serves a Runner's state and log:
//
//	GET /api/state   the runner's State
//	GET /api/scores  every score logged (see ScoresDoc)
//
// It has no login and no actions.
func Handler(r *Runner) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, r.Snapshot())
	})
	mux.HandleFunc("GET /api/scores", func(w http.ResponseWriter, _ *http.Request) {
		doc := ScoresDoc{Format: ScoresFormat, Scores: []ScoreRow{}}
		for _, s := range r.Log.Rows() {
			doc.Scores = append(doc.Scores, ScoreRow(s))
		}
		writeJSON(w, doc)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
