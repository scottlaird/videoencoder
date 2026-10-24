// Package publish copies a finished stream to where it is served from:
// a local directory or an S3-compatible bucket.
package publish

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Object is what a target knows about one stored file.
type Object struct {
	Size int64
	// MD5 is the hex MD5 of the content, or "" if the target doesn't
	// know it (an S3 multipart upload's ETag, say).
	MD5 string
}

// Target is somewhere a stream can be published. Names are
// slash-separated and relative to the target's root.
type Target interface {
	List(ctx context.Context) (map[string]Object, error)
	Put(ctx context.Context, name, localPath string, size int64, meta Meta) error
	Delete(ctx context.Context, name string) error
	String() string
}

// Meta is the HTTP metadata stored with an object, where the target
// supports it.
type Meta struct {
	ContentType  string
	CacheControl string
}

// smallFile is the size up to which files are compared by content;
// larger ones are compared by name and size. Media files are named by
// a hash of how they were built and packaged deterministically, so a
// large file with the same name and size is the same file, and hashing
// gigabytes on every publish would be wasted work.
const smallFile = 16 << 20

// manifests are uploaded after everything they reference, so a player
// never sees one that points at a missing object.
var manifests = []string{"dash.mpd", "hls.m3u8", "poster.json"}

// Report says what Sync did.
type Report struct {
	Uploaded, Skipped, Deleted []string
}

// Sync makes t hold the files named in files (relative to dir;
// directories are expanded). It uploads what is missing or different,
// manifests last. With prune, it then deletes everything else under the
// target's root.
func Sync(ctx context.Context, dir string, files []string, t Target, prune bool) (Report, error) {
	var r Report
	local, err := expand(dir, files)
	if err != nil {
		return r, err
	}
	remote, err := t.List(ctx)
	if err != nil {
		return r, fmt.Errorf("listing %s: %w", t, err)
	}

	slices.SortStableFunc(local, func(a, b string) int {
		return boolCmp(isManifest(a), isManifest(b))
	})
	for _, name := range local {
		p := filepath.Join(dir, filepath.FromSlash(name))
		st, err := os.Stat(p)
		if err != nil {
			return r, err
		}
		same, err := unchanged(p, st.Size(), remote[name])
		if err != nil {
			return r, err
		}
		if same {
			r.Skipped = append(r.Skipped, name)
			continue
		}
		slog.Info("Uploading", "target", t.String(), "name", name, "bytes", st.Size())
		if err := t.Put(ctx, name, p, st.Size(), metaFor(name)); err != nil {
			return r, fmt.Errorf("uploading %s to %s: %w", name, t, err)
		}
		r.Uploaded = append(r.Uploaded, name)
	}

	if prune {
		var stale []string
		for name := range remote {
			if !slices.Contains(local, name) {
				stale = append(stale, name)
			}
		}
		slices.Sort(stale)
		for _, name := range stale {
			slog.Info("Deleting", "target", t.String(), "name", name)
			if err := t.Delete(ctx, name); err != nil {
				return r, fmt.Errorf("deleting %s from %s: %w", name, t, err)
			}
			r.Deleted = append(r.Deleted, name)
		}
	}
	return r, nil
}

func boolCmp(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

func isManifest(name string) bool { return slices.Contains(manifests, name) }

// expand lists files under dir, recursing into directories, as
// slash-separated names.
func expand(dir string, files []string) ([]string, error) {
	var out []string
	for _, f := range files {
		root := filepath.Join(dir, filepath.FromSlash(f))
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// unchanged reports whether the remote object matches the local file.
func unchanged(localPath string, size int64, remote Object) (bool, error) {
	if remote.Size != size || (remote == Object{}) {
		return false, nil
	}
	if size > smallFile {
		return true, nil
	}
	if remote.MD5 == "" {
		return false, nil
	}
	sum, err := fileMD5(localPath)
	return sum == remote.MD5, err
}

func fileMD5(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// keyed matches names containing a build key, whose content never
// changes: "av1-2160.hdr.7c3e1a09bd.mp4", "thumbs.7b3af0c32d/tile-1.jpg".
var keyed = regexp.MustCompile(`(^|[./])[0-9a-f]{10}([./]|$)`)

func metaFor(name string) Meta {
	m := Meta{CacheControl: "no-cache"}
	if keyed.MatchString(name) {
		m.CacheControl = "public, max-age=31536000, immutable"
	}
	switch path.Ext(name) {
	case ".mpd":
		m.ContentType = "application/dash+xml"
	case ".m3u8":
		m.ContentType = "application/vnd.apple.mpegurl"
	case ".mp4":
		m.ContentType = "video/mp4"
		if strings.HasPrefix(path.Base(name), "audio.") {
			m.ContentType = "audio/mp4"
		}
	case ".jpg":
		m.ContentType = "image/jpeg"
	case ".avif":
		m.ContentType = "image/avif"
	case ".webp":
		m.ContentType = "image/webp"
	case ".json":
		m.ContentType = "application/json"
	default:
		m.ContentType = "application/octet-stream"
	}
	return m
}

// Dir is a local directory target.
type Dir struct{ Root string }

func (d Dir) String() string { return d.Root }

func (d Dir) List(ctx context.Context) (map[string]Object, error) {
	out := map[string]Object{}
	err := filepath.WalkDir(d.Root, func(p string, e os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) && p == d.Root {
			return filepath.SkipAll
		}
		if err != nil || e.IsDir() || strings.HasSuffix(p, ".partial") {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		o := Object{Size: info.Size()}
		if o.Size <= smallFile {
			if o.MD5, err = fileMD5(p); err != nil {
				return err
			}
		}
		rel, err := filepath.Rel(d.Root, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = o
		return nil
	})
	return out, err
}

// Put copies the file into place atomically.
func (d Dir) Put(ctx context.Context, name, localPath string, size int64, _ Meta) error {
	dst := filepath.Join(d.Root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".partial"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func (d Dir) Delete(ctx context.Context, name string) error {
	p := filepath.Join(d.Root, filepath.FromSlash(name))
	if err := os.Remove(p); err != nil {
		return err
	}
	// Tidy directories this leaves empty, such as an old thumbs.<key>.
	for dir := filepath.Dir(p); dir != filepath.Clean(d.Root); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			break
		}
	}
	return nil
}
