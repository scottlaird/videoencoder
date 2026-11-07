package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/scottlaird/videoencoder/internal/plan"
)

// copySources puts each source file into streamdir under its own base
// name, for copy_sources. It returns the names, for the stream's file
// list. A copy that already matches its source's size and modification
// time is kept, so multi-gigabyte sources aren't recopied every render.
func copySources(streamdir string, sources []plan.Source) ([]string, error) {
	var names []string
	seen := map[string]string{}
	for _, src := range sources {
		name := filepath.Base(src.Path)
		if other, ok := seen[name]; ok {
			return nil, fmt.Errorf("copy_sources: sources %q and %q are both named %s", other, src.Label, name)
		}
		seen[name] = src.Label
		if err := copyLarge(src.Path, filepath.Join(streamdir, name)); err != nil {
			return nil, fmt.Errorf("copy_sources: %w", err)
		}
		names = append(names, name)
	}
	return names, nil
}

// copyLarge copies src to dst unless dst already has src's size and
// modification time. It copies through a temporary file, which it gives
// src's modification time. It never hard-links: the copy is published,
// and nothing done to it should be able to reach the source.
func copyLarge(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if dt, err := os.Stat(dst); err == nil && dt.Size() == st.Size() && dt.ModTime().Equal(st.ModTime()) {
		return nil
	}
	tmp := dst + ".partial"
	if err := copyContents(src, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chtimes(tmp, st.ModTime(), st.ModTime()); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func copyContents(src, dst string) error {
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
		return fmt.Errorf("copying %q: %w", src, err)
	}
	return out.Close()
}
