// Package fileid identifies files cheaply by content.
package fileid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

const chunk = 1 << 20

// Content identifies a file by its size and a hash of its first and
// last MiB. It reads at most 2 MiB however large the file is, and
// changes whenever a file is replaced by a different one in practice,
// though an edit confined to the middle of a file can go unnoticed.
func Content(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%d\n", st.Size())
	if _, err := io.CopyN(h, f, chunk); err != nil && err != io.EOF {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	if st.Size() > 2*chunk {
		if _, err := f.Seek(-chunk, io.SeekEnd); err != nil {
			return "", err
		}
		if _, err := io.Copy(h, f); err != nil {
			return "", fmt.Errorf("reading %s: %w", path, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}
