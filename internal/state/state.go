// Package state remembers how a video's outputs were built.
//
// Outputs are named by a build key, a hash of everything that affects
// them, and appear only by atomic rename once complete. So an output
// is up to date exactly when the file with its key-name exists, and no
// per-output status is needed. Each video's manifest
// ("<video>.state.json" in its output directory) records, for each slot
// such as "main/av1-2160", the key last built there, and the files last
// written to its stream directory. That lets callers explain a rebuild
// and delete exactly the files a rebuild replaces, even when several
// videos share a directory.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// Entry is the last output built in one slot.
type Entry struct {
	Key    string `json:"key"`
	Output string `json:"output"` // file name, relative to the manifest's directory
	Source string `json:"source"` // identity of the input
	Args   string `json:"args"`   // the command, for explanations
}

// Manifest is one video's build state.
type Manifest struct {
	dir, file string
	Entries   map[string]Entry `json:"entries"`
	// Stream lists the files last written to the stream directory.
	Stream []string `json:"stream,omitempty"`
}

// Load reads the manifest of video in dir; a missing one is empty.
func Load(dir, video string) (*Manifest, error) {
	m := &Manifest{dir: dir, file: video + ".state.json", Entries: map[string]Entry{}}
	data, err := os.ReadFile(filepath.Join(dir, m.file))
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Join(dir, m.file), err)
	}
	if m.Entries == nil {
		m.Entries = map[string]Entry{}
	}
	return m, nil
}

// Save writes the manifest atomically.
func (m *Manifest) Save() error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(m.dir, m.file+".partial")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(m.dir, m.file))
}

// Reasons returned by Explain.
const (
	UpToDate       = ""
	New            = "new"
	SourceChanged  = "source changed"
	SettingsChange = "settings changed"
	OutputMissing  = "output file is missing"
)

// Explain says why the output for slot, which should be built with key
// from source, needs building, or UpToDate if it exists.
func (m *Manifest) Explain(slot, key, source string, outputExists bool) string {
	if outputExists {
		return UpToDate
	}
	prev, ok := m.Entries[slot]
	switch {
	case !ok:
		return New
	case prev.Key == key:
		return OutputMissing
	case prev.Source != source:
		return SourceChanged
	default:
		return SettingsChange
	}
}

// Record notes that slot now holds e, and deletes the output it
// replaces, if any. The caller saves the manifest.
func (m *Manifest) Record(slot string, e Entry) error {
	prev, ok := m.Entries[slot]
	m.Entries[slot] = e
	if ok && prev.Output != e.Output {
		return m.remove(prev.Output)
	}
	return nil
}

// Prune forgets every slot not in keep and deletes its output. The
// caller saves the manifest.
func (m *Manifest) Prune(keep []string) error {
	var errs []error
	for slot, e := range m.Entries {
		if !slices.Contains(keep, slot) {
			delete(m.Entries, slot)
			errs = append(errs, m.remove(e.Output))
		}
	}
	return errors.Join(errs...)
}

// SetStream records that files (names in streamdir) make up the stream
// now, and deletes those of the previous stream that aren't among them.
// The caller saves the manifest.
func (m *Manifest) SetStream(streamdir string, files []string) error {
	var errs []error
	for _, f := range m.Stream {
		if !slices.Contains(files, f) {
			err := os.Remove(filepath.Join(streamdir, f))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	m.Stream = slices.Sorted(slices.Values(files))
	return errors.Join(errs...)
}

func (m *Manifest) remove(name string) error {
	err := os.RemoveAll(filepath.Join(m.dir, name))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing old output: %w", err)
	}
	return nil
}
