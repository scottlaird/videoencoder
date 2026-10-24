package pack

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func copyFixtures(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"dash.mpd", "hls.m3u8"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestAddThumbnails(t *testing.T) {
	dir := copyFixtures(t)
	track := Thumbnails{
		Dir: "thumbs.abc", Tiles: 2, Columns: 5, Rows: 5,
		ThumbWidth: 320, ThumbHeight: 180, Interval: 5, Duration: 170, Bytes: 170000,
	}
	files, err := AddThumbnails(dir, track)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != thumbnailPlaylist {
		t.Errorf("files = %q", files)
	}

	mpd, err := os.ReadFile(filepath.Join(dir, "dash.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	// The result must still be well-formed, with the new set last in the
	// one Period.
	var doc struct {
		Period struct {
			Sets []struct {
				ID          string `xml:"id,attr"`
				ContentType string `xml:"contentType,attr"`
				Template    struct {
					Media    string `xml:"media,attr"`
					Duration string `xml:"duration,attr"`
				} `xml:"SegmentTemplate"`
				Reps []struct {
					Width     string `xml:"width,attr"`
					Height    string `xml:"height,attr"`
					Bandwidth string `xml:"bandwidth,attr"`
					Prop      struct {
						Scheme string `xml:"schemeIdUri,attr"`
						Value  string `xml:"value,attr"`
					} `xml:"EssentialProperty"`
				} `xml:"Representation"`
			} `xml:"AdaptationSet"`
		} `xml:"Period"`
	}
	if err := xml.Unmarshal(mpd, &doc); err != nil {
		t.Fatalf("dash.mpd no longer parses: %v", err)
	}
	sets := doc.Period.Sets
	last := sets[len(sets)-1]
	got := []string{last.ID, last.ContentType, last.Template.Media, last.Template.Duration,
		last.Reps[0].Width, last.Reps[0].Height, last.Reps[0].Bandwidth, last.Reps[0].Prop.Scheme, last.Reps[0].Prop.Value}
	want := []string{"4", "image", "thumbs.abc/tile-$Number$.jpg", "125",
		"1600", "900", "8000", "http://dashif.org/thumbnail_tile", "5x5"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("thumbnail AdaptationSet =\n %q\nwant\n %q", got, want)
	}
	if len(sets) != 5 {
		t.Errorf("%d AdaptationSets, want the fixture's 4 plus thumbnails", len(sets))
	}

	master, err := os.ReadFile(filepath.Join(dir, "hls.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(master), "\n#EXT-X-IMAGE-STREAM-INF:BANDWIDTH=8000,RESOLUTION=1600x900,CODECS=\"jpeg\",URI=\"thumbnails.m3u8\"\n") {
		t.Errorf("hls.m3u8 doesn't end with the image stream:\n%s", master)
	}

	playlist, err := os.ReadFile(filepath.Join(dir, thumbnailPlaylist))
	if err != nil {
		t.Fatal(err)
	}
	wantPlaylist := `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:125
#EXT-X-MEDIA-SEQUENCE:1
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-IMAGES-ONLY
#EXTINF:125.000,
#EXT-X-TILES:RESOLUTION=320x180,LAYOUT=5x5,DURATION=5.000
thumbs.abc/tile-1.jpg
#EXTINF:45.000,
#EXT-X-TILES:RESOLUTION=320x180,LAYOUT=5x5,DURATION=5.000
thumbs.abc/tile-2.jpg
#EXT-X-ENDLIST
`
	if string(playlist) != wantPlaylist {
		t.Errorf("image playlist =\n%s\nwant\n%s", playlist, wantPlaylist)
	}
}

func TestAddThumbnailsErrors(t *testing.T) {
	dir := copyFixtures(t)
	if _, err := AddThumbnails(dir, Thumbnails{Dir: "x"}); err == nil {
		t.Error("AddThumbnails with no tiles: nil error")
	}
	if err := os.WriteFile(filepath.Join(dir, "dash.mpd"), []byte("<MPD><Period></Period><Period></Period></MPD>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AddThumbnails(dir, Thumbnails{Dir: "x", Tiles: 1, Columns: 1, Rows: 1, Interval: 1}); err == nil {
		t.Error("AddThumbnails with two Periods: nil error")
	}
}
