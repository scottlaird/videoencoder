package benchweb

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/scottlaird/videoencoder/internal/bench"
)

// FilesFormat is the version of the /api/files/index document, under
// the same rules as ResultsFormat.
const FilesFormat = 1

// FileIndex is the /api/files/index document: what the file routes can
// serve.
type FileIndex struct {
	Format  int           `json:"format"`
	Encodes []EncodeFile  `json:"encodes"`
	Samples []SampleEntry `json:"samples"`
	Sources []SourceEntry `json:"sources"`
}

// EncodeFile is an encode the queue can serve, by its SHA-256.
type EncodeFile struct {
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	// Kept is "object" for the object store (Config.Keep "all") or
	// "keeper" for a pair's best encode in keep/.
	Kept string `json:"kept"`
}

// SampleEntry is a sample the queue can serve, by Row.SampleID.
type SampleEntry struct {
	SampleID string `json:"sample_id"`
	Name     string `json:"name"`
	SourceID string `json:"source_id,omitempty"`
	Bytes    int64  `json:"bytes"`
	// ReferenceBytes is the size of the file the sample's encodes are
	// scored against, if it isn't the sample (0 otherwise).
	ReferenceBytes int64 `json:"reference_bytes,omitempty"`
}

// SourceEntry is a source file the queue can serve, by Row.SourceID.
type SourceEntry struct {
	SourceID string `json:"source_id"`
	Name     string `json:"name"`
	Bytes    int64  `json:"bytes"`
}

// keeperIndex maps the SHA-256s logged in results logs to kept best
// encodes, rebuilt when a log changes.
type keeperIndex struct {
	mu    sync.Mutex
	stamp map[string]logStamp
	files map[string]keeperFile // by SHA-256
}

type logStamp struct {
	size int64
	mod  time.Time
}

type keeperFile struct {
	workdir, path string
	bytes         int64
}

func (s *Server) fileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/files/index", s.auth(s.fileIndex))
	mux.HandleFunc("GET /api/files/encode/{sha}", s.auth(s.encodeFile))
	mux.HandleFunc("GET /api/files/sample/{id}", s.auth(s.sampleFile("sample")))
	mux.HandleFunc("GET /api/files/reference/{id}", s.auth(s.sampleFile("reference")))
	mux.HandleFunc("GET /api/files/source/{id}", s.auth(s.sampleFile("source")))
}

// auth requires Server.Token, if set, as a bearer token.
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="encbench"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		h(w, r)
	}
}

func (s *Server) encodeFile(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !bench.IsSHA256(sha) {
		http.Error(w, "not a SHA-256 in lowercase hex", http.StatusBadRequest)
		return
	}
	for _, d := range s.Runner.Dirs() {
		if p := bench.ObjectPath(d.Workdir, sha); isFile(p) {
			serveFile(w, r, []string{d.Workdir}, p, `"`+sha+`"`, sha+".mp4")
			return
		}
	}
	k, err := s.keepers()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if f, ok := k[sha]; ok {
		serveFile(w, r, []string{f.workdir}, f.path, `"`+sha+`"`, sha+".mp4")
		return
	}
	http.NotFound(w, r)
}

func (s *Server) sampleFile(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !bench.IsFileID(id) {
			http.Error(w, "not a sample or source ID", http.StatusBadRequest)
			return
		}
		for _, d := range s.Runner.Dirs() {
			sf, err := bench.ReadSampleFiles(d.Workdir)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			var p string
			switch kind {
			case "source":
				p = sf.Sources[id]
			case "sample":
				p = sf.Samples[id].Path
			case "reference":
				p = sf.Samples[id].Reference
			}
			if p != "" && isFile(p) {
				serveFile(w, r, []string{d.Workdir, d.Samples}, p, "", filepath.Base(p))
				return
			}
		}
		http.NotFound(w, r)
	}
}

func (s *Server) fileIndex(w http.ResponseWriter, r *http.Request) {
	idx := FileIndex{Format: FilesFormat, Encodes: []EncodeFile{}, Samples: []SampleEntry{}, Sources: []SourceEntry{}}
	seen := map[string]bool{}
	dirs := s.Runner.Dirs()
	for _, d := range dirs {
		objects := filepath.Join(d.Workdir, "objects")
		subs, _ := os.ReadDir(objects)
		for _, sub := range subs {
			if !sub.IsDir() {
				continue
			}
			files, _ := os.ReadDir(filepath.Join(objects, sub.Name()))
			for _, f := range files {
				sha, ok := strings.CutSuffix(f.Name(), ".mp4")
				if !ok || !bench.IsSHA256(sha) || seen[sha] {
					continue
				}
				if info, err := f.Info(); err == nil && info.Mode().IsRegular() {
					seen[sha] = true
					idx.Encodes = append(idx.Encodes, EncodeFile{SHA256: sha, Bytes: info.Size(), Kept: "object"})
				}
			}
		}
	}
	k, err := s.keepers()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for sha, f := range k {
		if !seen[sha] {
			seen[sha] = true
			idx.Encodes = append(idx.Encodes, EncodeFile{SHA256: sha, Bytes: f.bytes, Kept: "keeper"})
		}
	}
	for _, d := range dirs {
		sf, err := bench.ReadSampleFiles(d.Workdir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for id, e := range sf.Samples {
			if seen["sample:"+id] {
				continue
			}
			size := fileSize(e.Path)
			if size < 0 {
				continue
			}
			seen["sample:"+id] = true
			se := SampleEntry{SampleID: id, Name: e.Name, SourceID: e.SourceID, Bytes: size}
			if e.Reference != "" {
				se.ReferenceBytes = max(0, fileSize(e.Reference))
			}
			idx.Samples = append(idx.Samples, se)
		}
		for id, p := range sf.Sources {
			if seen["source:"+id] {
				continue
			}
			if size := fileSize(p); size >= 0 {
				seen["source:"+id] = true
				idx.Sources = append(idx.Sources, SourceEntry{SourceID: id, Name: filepath.Base(p), Bytes: size})
			}
		}
	}
	slices.SortFunc(idx.Encodes, func(a, b EncodeFile) int { return strings.Compare(a.SHA256, b.SHA256) })
	slices.SortFunc(idx.Samples, func(a, b SampleEntry) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(idx.Sources, func(a, b SourceEntry) int { return strings.Compare(a.Name, b.Name) })
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(idx)
}

// keepers returns the kept best encodes named by the results logs, by
// SHA-256: those whose file is still there at the logged size.
func (s *Server) keepers() (map[string]keeperFile, error) {
	s.keep.mu.Lock()
	defer s.keep.mu.Unlock()
	dirs := s.Runner.Dirs()
	stamp := map[string]logStamp{}
	for _, d := range dirs {
		p := filepath.Join(d.Workdir, "results.csv")
		if info, err := os.Stat(p); err == nil {
			stamp[p] = logStamp{info.Size(), info.ModTime()}
		}
	}
	if s.keep.files != nil && mapsEqual(stamp, s.keep.stamp) {
		return s.keep.files, nil
	}
	files := map[string]keeperFile{}
	for _, d := range dirs {
		p := filepath.Join(d.Workdir, "results.csv")
		if _, ok := stamp[p]; !ok {
			continue
		}
		res, err := bench.ReadResults(p)
		if err != nil {
			return nil, err
		}
		keepDir := filepath.Join(d.Workdir, "keep")
		for i := range res.Rows {
			row := &res.Rows[i]
			if !bench.IsSHA256(row.OutSHA256) || row.Ladder != "" {
				continue
			}
			if _, ok := files[row.OutSHA256]; ok {
				continue
			}
			k := bench.KeeperPath(keepDir, row.Sample, row.Flagset, row.Quality)
			if fileSize(k) == row.Bytes {
				files[row.OutSHA256] = keeperFile{workdir: d.Workdir, path: k, bytes: row.Bytes}
			}
		}
	}
	s.keep.stamp, s.keep.files = stamp, files
	return files, nil
}

func mapsEqual(a, b map[string]logStamp) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w.size != v.size || !w.mod.Equal(v.mod) {
			return false
		}
	}
	return true
}

// serveFile serves path, which must be under one of roots, through an
// os.Root there, so neither ".." nor a symlink can reach outside it.
// etag, if set, is sent and honored for conditional and range
// requests.
func serveFile(w http.ResponseWriter, r *http.Request, roots []string, path, etag, name string) {
	// Configs' directories are as written in their work files, often
	// relative to where the queue runs, while sample-files.json has
	// absolute paths, so compare the two absolute.
	path, err := filepath.Abs(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		if root, err = filepath.Abs(root); err != nil {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		rt, err := os.OpenRoot(root)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rt.Close()
		// A sample is often a symlink to a master kept elsewhere; the
		// link is in the samples dir because the config put it there,
		// so follow it, but only that last link, and only to a file.
		if st, err := rt.Lstat(rel); err == nil && st.Mode()&fs.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(path)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			if st, err := os.Stat(target); err != nil || !st.Mode().IsRegular() {
				http.Error(w, "file unavailable", http.StatusForbidden)
				return
			}
			rt.Close()
			if rt, err = os.OpenRoot(filepath.Dir(target)); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer rt.Close()
			rel = filepath.Base(target)
		}
		if _, err := rt.Stat(rel); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(w, r)
			} else {
				http.Error(w, "file unavailable", http.StatusForbidden)
			}
			return
		}
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, rt.FS(), filepath.ToSlash(rel))
		return
	}
	http.Error(w, "file is outside the queue's directories", http.StatusForbidden)
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// fileSize is p's size, or -1 if it isn't a regular file.
func fileSize(p string) int64 {
	info, err := os.Stat(p)
	if err != nil || !info.Mode().IsRegular() {
		return -1
	}
	return info.Size()
}
