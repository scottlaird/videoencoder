package pack

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Thumbnails describes a seek-thumbnail track: tiled JPEGs named
// "<Dir>/tile-<n>.jpg", n from 1, each a Columns x Rows grid of
// ThumbWidth x ThumbHeight frames taken every Interval seconds.
type Thumbnails struct {
	Dir                     string // relative to the stream directory
	Tiles                   int
	Columns, Rows           int
	ThumbWidth, ThumbHeight int
	Interval                float64 // seconds per thumbnail
	Duration                float64 // seconds of video
	Bytes                   int64   // total size of the tiles
}

// HLS image playlist name for the thumbnail track.
const thumbnailPlaylist = "thumbnails.m3u8"

func (t Thumbnails) tileSeconds() float64 { return t.Interval * float64(t.Columns*t.Rows) }

func (t Thumbnails) bandwidth() int64 {
	if t.Duration <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(t.Bytes) * 8 / t.Duration))
}

// AddThumbnails adds t to the DASH and HLS manifests packager wrote in
// streamdir, and writes the HLS image playlist. Shaka Packager can't
// do this itself. It returns the names of files it created.
//
// DASH gets the DASH-IF IOP form (an image AdaptationSet with a
// thumbnail_tile EssentialProperty); HLS gets an EXT-X-IMAGE-STREAM-INF
// pointing at an EXT-X-IMAGES-ONLY playlist with EXT-X-TILES, the form
// Roku and Shaka Player read. Both are read by Shaka Player.
func AddThumbnails(streamdir string, t Thumbnails) ([]string, error) {
	if t.Tiles == 0 {
		return nil, fmt.Errorf("thumbnail track %s has no tiles", t.Dir)
	}
	mpdPath := filepath.Join(streamdir, "dash.mpd")
	mpd, err := os.ReadFile(mpdPath)
	if err != nil {
		return nil, err
	}
	if mpd, err = addDASHThumbnails(mpd, t); err != nil {
		return nil, fmt.Errorf("%s: %w", mpdPath, err)
	}
	masterPath := filepath.Join(streamdir, "hls.m3u8")
	master, err := os.ReadFile(masterPath)
	if err != nil {
		return nil, err
	}
	for _, w := range []struct {
		path string
		data []byte
	}{
		{mpdPath, mpd},
		{masterPath, addHLSThumbnails(master, t)},
		{filepath.Join(streamdir, thumbnailPlaylist), hlsImagePlaylist(t)},
	} {
		if err := writeFile(w.path, w.data); err != nil {
			return nil, err
		}
	}
	return []string{thumbnailPlaylist}, nil
}

var adaptationSetID = regexp.MustCompile(`<AdaptationSet id="(\d+)"`)

func addDASHThumbnails(mpd []byte, t Thumbnails) ([]byte, error) {
	if n := bytes.Count(mpd, []byte("</Period>")); n != 1 {
		return nil, fmt.Errorf("want exactly one Period, found %d", n)
	}
	id := 0
	for _, m := range adaptationSetID.FindAllSubmatch(mpd, -1) {
		if n, _ := strconv.Atoi(string(m[1])); n >= id {
			id = n + 1
		}
	}
	set := fmt.Sprintf(`    <AdaptationSet id="%d" contentType="image" mimeType="image/jpeg">
      <SegmentTemplate media="%s/tile-$Number$.jpg" duration="%s" startNumber="1" timescale="1"/>
      <Representation id="thumbnails" bandwidth="%d" width="%d" height="%d">
        <EssentialProperty schemeIdUri="http://dashif.org/thumbnail_tile" value="%dx%d"/>
      </Representation>
    </AdaptationSet>
  </Period>`,
		id, t.Dir, ftoa(t.tileSeconds()), t.bandwidth(),
		t.Columns*t.ThumbWidth, t.Rows*t.ThumbHeight, t.Columns, t.Rows)
	return bytes.Replace(mpd, []byte("  </Period>"), []byte(set), 1), nil
}

func addHLSThumbnails(master []byte, t Thumbnails) []byte {
	line := fmt.Sprintf("#EXT-X-IMAGE-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=\"jpeg\",URI=\"%s\"\n",
		t.bandwidth(), t.Columns*t.ThumbWidth, t.Rows*t.ThumbHeight, thumbnailPlaylist)
	if !bytes.HasSuffix(master, []byte("\n")) {
		master = append(master, '\n')
	}
	return append(master, []byte("\n"+line)...)
}

func hlsImagePlaylist(t Thumbnails) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(t.tileSeconds())))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:1\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-IMAGES-ONLY\n")
	for n := 1; n <= t.Tiles; n++ {
		d := min(t.tileSeconds(), t.Duration-float64(n-1)*t.tileSeconds())
		if n == t.Tiles && d <= 0 {
			d = t.tileSeconds()
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", d)
		fmt.Fprintf(&b, "#EXT-X-TILES:RESOLUTION=%dx%d,LAYOUT=%dx%d,DURATION=%.3f\n",
			t.ThumbWidth, t.ThumbHeight, t.Columns, t.Rows, t.Interval)
		fmt.Fprintf(&b, "%s/tile-%d.jpg\n", t.Dir, n)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String())
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func writeFile(path string, data []byte) error {
	tmp := path + ".partial"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
