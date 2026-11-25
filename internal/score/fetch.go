package score

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/scottlaird/videoencoder/internal/benchweb"
)

// Client reads an encbench queue's web view.
type Client struct {
	Base  string // such as "http://render3:8090"
	Token string
	HTTP  *http.Client
}

func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s%s: %s: %s", c.Base, path, resp.Status, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	resp, err := c.get(ctx, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("GET %s%s: %w", c.Base, path, err)
	}
	return nil
}

// Results is the queue's /api/results.
func (c *Client) Results(ctx context.Context) (*benchweb.Results, error) {
	var r benchweb.Results
	if err := c.getJSON(ctx, "/api/results", &r); err != nil {
		return nil, err
	}
	if r.Format != benchweb.ResultsFormat {
		return nil, fmt.Errorf("%s: results format %d, want %d", c.Base, r.Format, benchweb.ResultsFormat)
	}
	return &r, nil
}

// Index is the queue's /api/files/index.
func (c *Client) Index(ctx context.Context) (*benchweb.FileIndex, error) {
	var idx benchweb.FileIndex
	if err := c.getJSON(ctx, "/api/files/index", &idx); err != nil {
		return nil, err
	}
	if idx.Format != benchweb.FilesFormat {
		return nil, fmt.Errorf("%s: files format %d, want %d", c.Base, idx.Format, benchweb.FilesFormat)
	}
	return &idx, nil
}

// Cache holds files fetched from queues, by content: encodes by
// SHA-256 (checked as they arrive) and samples and references by
// sample ID (checked by size). Files are shared by every queue that
// has them. Past MaxBytes, the least recently used files that aren't
// in use are removed.
type Cache struct {
	Dir      string
	MaxBytes int64

	mu    sync.Mutex
	inUse map[string]int
}

// Encode returns the path of the encode with SHA-256 sha, fetching it
// from c if it isn't cached. Call Release with the path when done.
func (k *Cache) Encode(ctx context.Context, c *Client, sha string, size int64) (string, error) {
	path := filepath.Join(k.Dir, "encodes", sha[:2], sha+".mp4")
	return k.fetch(ctx, c, "/api/files/encode/"+sha, path, size, sha)
}

// Sample returns the path of a sample's file of kind "sample" or
// "reference", fetching it from c if it isn't cached.
func (k *Cache) Sample(ctx context.Context, c *Client, kind, id string, size int64) (string, error) {
	dir := filepath.Join(k.Dir, kind+"s")
	if m, _ := filepath.Glob(filepath.Join(dir, id+".*")); len(m) > 0 {
		for _, p := range m {
			if !strings.HasSuffix(p, ".partial") {
				return k.fetch(ctx, c, "/api/files/"+kind+"/"+id, p, size, "")
			}
		}
	}
	return k.fetch(ctx, c, "/api/files/"+kind+"/"+id, filepath.Join(dir, id), size, "")
}

func (k *Cache) fetch(ctx context.Context, c *Client, route, path string, size int64, sha string) (string, error) {
	k.mu.Lock()
	if k.inUse == nil {
		k.inUse = map[string]int{}
	}
	k.mu.Unlock()
	if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && (size <= 0 || st.Size() == size) {
		now := time.Now()
		os.Chtimes(path, now, now)
		k.hold(path)
		return path, nil
	}
	resp, err := c.get(ctx, route)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if filepath.Ext(path) == "" { // a sample: take its extension from the server's name for it
		ext := ".mov"
		if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
			if e := filepath.Ext(params["filename"]); e != "" && len(e) <= 6 && !strings.ContainsAny(e, `/\`) {
				ext = e
			}
		}
		path += ext
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && size > 0 && n != size {
		err = fmt.Errorf("got %d bytes, want %d", n, size)
	}
	if err == nil && sha != "" && hex.EncodeToString(h.Sum(nil)) != sha {
		err = fmt.Errorf("SHA-256 doesn't match")
	}
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("fetching %s%s: %w", c.Base, route, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	k.hold(path)
	k.evict()
	return path, nil
}

func (k *Cache) hold(path string) {
	k.mu.Lock()
	k.inUse[path]++
	k.mu.Unlock()
}

// Release says a path Encode or Sample returned is no longer in use.
func (k *Cache) Release(paths ...string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, p := range paths {
		if k.inUse[p]--; k.inUse[p] <= 0 {
			delete(k.inUse, p)
		}
	}
}

// evict removes the least recently used files not in use until the
// cache fits MaxBytes.
func (k *Cache) evict() {
	if k.MaxBytes <= 0 {
		return
	}
	type entry struct {
		path string
		size int64
		mod  time.Time
	}
	var files []entry
	var total int64
	filepath.WalkDir(k.Dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".partial") {
			return nil
		}
		if info, err := d.Info(); err == nil {
			files = append(files, entry{p, info.Size(), info.ModTime()})
			total += info.Size()
		}
		return nil
	})
	slices.SortFunc(files, func(a, b entry) int { return a.mod.Compare(b.mod) })
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, f := range files {
		if total <= k.MaxBytes {
			return
		}
		if k.inUse[f.path] > 0 {
			continue
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}
