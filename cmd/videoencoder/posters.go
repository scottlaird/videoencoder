package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/scottlaird/videoencoder/internal/config"
	"github.com/scottlaird/videoencoder/internal/pack"
	"github.com/scottlaird/videoencoder/internal/plan"
	"github.com/scottlaird/videoencoder/internal/probe"
)

// posters picks a source for each tone and returns the poster jobs: SDR
// from the first SDR source, or tone-mapped from the first HDR one if
// there is no SDR source; HDR from the first HDR source.
func posters(p config.Poster, sources []plan.Source, vds []probe.VideoData, outdir string) []plan.Poster {
	sdr, hdr := -1, -1
	for i, vd := range vds {
		if vd.Tone() == probe.HDR {
			if hdr < 0 {
				hdr = i
			}
		} else if sdr < 0 {
			sdr = i
		}
	}
	if sdr < 0 {
		sdr = hdr
	}
	out := plan.Posters(p, sources[sdr], vds[sdr], probe.SDR, outdir)
	if hdr >= 0 {
		out = append(out, plan.Posters(p, sources[hdr], vds[hdr], probe.HDR, outdir)...)
	}
	return out
}

// posterIndex is poster.json: what a page needs to build <picture>
// and srcset markup.
type posterIndex struct {
	Offset string        `json:"offset"`
	Images []posterImage `json:"images"`
}

type posterImage struct {
	Src    string `json:"src"`
	Tone   string `json:"tone"`
	Format string `json:"format"`
	Width  uint64 `json:"width"`
	Height uint64 `json:"height"`
}

// legacyPosterWidth caps the width of thumbnail.jpg, the single poster
// earlier versions wrote, kept for pages that still use it.
const legacyPosterWidth = 1920

// publishPosters copies the posters into streamdir and writes
// poster.json and thumbnail.jpg there. It returns the names it wrote.
func publishPosters(streamdir, offset string, ps []plan.Poster) ([]string, error) {
	idx := posterIndex{Offset: offset}
	var files []string
	var legacy *plan.Poster
	for i, p := range ps {
		name := filepath.Base(p.Job.Output)
		if err := copySmallFile(p.Job.Output, filepath.Join(streamdir, name)); err != nil {
			return nil, err
		}
		files = append(files, name)
		idx.Images = append(idx.Images, posterImage{
			Src: name, Tone: string(p.Tone), Format: p.Format, Width: p.Width, Height: p.Height,
		})
		if p.Tone == probe.SDR && p.Format == "jpg" && p.Width <= legacyPosterWidth &&
			(legacy == nil || p.Width > legacy.Width) {
			legacy = &ps[i]
		}
	}

	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Join(streamdir, "poster.json"), append(data, '\n')); err != nil {
		return nil, err
	}
	files = append(files, "poster.json")

	if legacy != nil {
		if err := copySmallFile(legacy.Job.Output, filepath.Join(streamdir, "thumbnail.jpg")); err != nil {
			return nil, err
		}
		files = append(files, "thumbnail.jpg")
	}
	return files, nil
}

// copySmallFile copies by reading the whole file into memory, so it is
// only for small files such as posters.
func copySmallFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("copying %q: %w", src, err)
	}
	return writeAtomic(dst, b)
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".partial"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// publishThumbnails copies the seek-thumbnail tiles into the stream
// directory and adds them to its manifests. It returns the names it
// wrote.
func publishThumbnails(r *config.Resolved, b *built) ([]string, error) {
	src := b.thumbs.Result()
	dirName := filepath.Base(src)
	dst := filepath.Join(r.Streamdir, dirName)
	tiles, err := filepath.Glob(filepath.Join(src, "tile-*.jpg"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, err
	}
	var bytes int64
	for _, tile := range tiles {
		st, err := os.Stat(tile)
		if err != nil {
			return nil, err
		}
		bytes += st.Size()
		if err := copySmallFile(tile, filepath.Join(dst, filepath.Base(tile))); err != nil {
			return nil, err
		}
	}
	t := r.Thumbnails
	files, err := pack.AddThumbnails(r.Streamdir, pack.Thumbnails{
		Dir:         dirName,
		Tiles:       len(tiles),
		Columns:     int(t.Columns),
		Rows:        int(t.Rows),
		ThumbWidth:  int(plan.ThumbWidth(t, b.vd)),
		ThumbHeight: int(t.Height),
		Interval:    t.Interval,
		Duration:    b.vd.Duration.Seconds(),
		Bytes:       bytes,
	})
	if err != nil {
		return nil, err
	}
	return append(files, dirName), nil
}
