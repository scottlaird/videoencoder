package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Config.Keep's values.
const (
	KeepAnswer = "answer"
	KeepAll    = "all"
)

// ObjectPath is where the object store in workdir keeps an encode
// whose SHA-256 is sha (in hex): workdir/objects/<first two>/<sha>.mp4.
// Byte-identical encodes share one file.
func ObjectPath(workdir, sha string) string {
	return filepath.Join(workdir, "objects", sha[:2], sha+".mp4")
}

// IsSHA256 says whether s is a SHA-256 in lowercase hex, as
// Row.OutSHA256 is.
func IsSHA256(s string) bool { return len(s) == 64 && isLowerHex(s) }

// IsFileID says whether s looks like a fileid content identity or a
// key derived from one, as Row.SampleID and Row.SourceID are.
func IsFileID(s string) bool { return len(s) == 16 && isLowerHex(s) }

func isLowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// storeObject moves the encode at path into the object store, under
// its SHA-256 sha, and returns where it went. If the store already has
// it, path is deleted.
func (b *Bench) storeObject(path, sha string) (string, error) {
	if !IsSHA256(sha) {
		return "", fmt.Errorf("%s: no SHA-256", path)
	}
	dst := ObjectPath(b.Cfg.Workdir, sha)
	if _, err := os.Stat(dst); err == nil {
		os.Remove(path)
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(path, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// linkOrCopy makes dst the same file as src: a hard link, or a copy
// where the filesystem won't link. dst is replaced atomically.
func linkOrCopy(src, dst string) error {
	tmp := dst + ".partial"
	os.Remove(tmp)
	if err := os.Link(src, tmp); err != nil {
		if err := copyFile(src, tmp); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// SampleFiles is workdir/sample-files.json: where the files behind each
// sample ID, source ID and reference are, for serving them by ID (see
// benchweb). Runs add to it; entries for files that are gone stay
// until the ID is seen again with another path.
type SampleFiles struct {
	Format int `json:"format"`
	// Samples maps Row.SampleID to the sample's files.
	Samples map[string]SampleFile `json:"samples"`
	// Sources maps Row.SourceID to the source file's path.
	Sources map[string]string `json:"sources"`
}

// SampleFile is one sample's files, as absolute paths.
type SampleFile struct {
	Name string `json:"name"`
	// Path is the file encoded: the source, or a scaled copy of it.
	Path string `json:"path"`
	// Reference is the file its encodes are scored against, if not
	// Path.
	Reference string    `json:"reference,omitempty"`
	SourceID  string    `json:"source_id"`
	Updated   time.Time `json:"updated"`
}

const sampleFilesFormat = 1

// SampleFilesPath is the SampleFiles of workdir.
func SampleFilesPath(workdir string) string { return filepath.Join(workdir, "sample-files.json") }

// ReadSampleFiles reads workdir's SampleFiles. A workdir without one
// has an empty one.
func ReadSampleFiles(workdir string) (*SampleFiles, error) {
	sf := &SampleFiles{Format: sampleFilesFormat, Samples: map[string]SampleFile{}, Sources: map[string]string{}}
	data, err := os.ReadFile(SampleFilesPath(workdir))
	if errors.Is(err, os.ErrNotExist) {
		return sf, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, sf); err != nil {
		return nil, fmt.Errorf("%s: %w", SampleFilesPath(workdir), err)
	}
	if sf.Format != sampleFilesFormat {
		return nil, fmt.Errorf("%s: format %d, want %d", SampleFilesPath(workdir), sf.Format, sampleFilesFormat)
	}
	if sf.Samples == nil {
		sf.Samples = map[string]SampleFile{}
	}
	if sf.Sources == nil {
		sf.Sources = map[string]string{}
	}
	return sf, nil
}

// recordSampleFiles adds samples to the workdir's SampleFiles.
func (b *Bench) recordSampleFiles(samples []*sample) error {
	sf, err := ReadSampleFiles(b.Cfg.Workdir)
	if err != nil {
		return err
	}
	changed := false
	now := time.Now().UTC()
	for _, s := range samples {
		e := SampleFile{Name: s.name, Path: absPath(s.path), SourceID: s.srcID}
		if s.ref != nil {
			e.Reference = absPath(s.ref.path)
		}
		if old, ok := sf.Samples[s.id]; !ok || old.Name != e.Name || old.Path != e.Path || old.Reference != e.Reference || old.SourceID != e.SourceID {
			e.Updated = now
			sf.Samples[s.id] = e
			changed = true
		}
		if s.srcID != "" {
			src := absPath(filepath.Join(b.Cfg.Samples, s.source))
			if sf.Sources[s.srcID] != src {
				sf.Sources[s.srcID] = src
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	path := SampleFilesPath(b.Cfg.Workdir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
