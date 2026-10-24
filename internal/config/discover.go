package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Discover finds every video defined under root, either by a
// "<name>.video.yaml" sidecar or by an entry under "videos:" in a
// videoencoder.yaml. A video defined both ways is returned once. Hidden
// directories are skipped. If root is a sidecar file, it returns just
// that video.
func Discover(root string) ([]Video, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		name, ok := strings.CutSuffix(filepath.Base(root), SidecarSuffix)
		if !ok {
			return nil, fmt.Errorf("%s is not a %s file or a directory", root, SidecarSuffix)
		}
		return []Video{{Name: name, Dir: filepath.Dir(root)}}, nil
	}

	seen := map[Video]bool{}
	var videos []Video
	add := func(v Video) {
		if !seen[v] {
			seen[v] = true
			videos = append(videos, v)
		}
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		dir := filepath.Dir(path)
		switch {
		case name == ProjectFile:
			n, err := loadLayerFile(path)
			if err != nil {
				return err
			}
			videos := mappingValue(n, "videos")
			if videos == nil {
				return nil
			}
			for i := 0; i+1 < len(videos.Content); i += 2 {
				add(Video{Name: videos.Content[i].Value, Dir: dir})
			}
		case strings.HasSuffix(name, SidecarSuffix):
			add(Video{Name: strings.TrimSuffix(name, SidecarSuffix), Dir: dir})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(videos, func(a, b Video) int {
		return strings.Compare(filepath.Join(a.Dir, a.Name), filepath.Join(b.Dir, b.Name))
	})
	return videos, nil
}

// AdHoc describes a video given only as source files on the command
// line. It is named after the first file and lives in its directory;
// the files become sources "0", "1", ... in that order.
func AdHoc(files []string) (Video, map[string]any, error) {
	if len(files) == 0 {
		return Video{}, nil, fmt.Errorf("no source files")
	}
	if len(files) > 10 {
		return Video{}, nil, fmt.Errorf("at most 10 source files")
	}
	sources := map[string]any{}
	for i, f := range files {
		abs, err := filepath.Abs(f)
		if err != nil {
			return Video{}, nil, err
		}
		sources[fmt.Sprint(i)] = abs
	}
	first := sources["0"].(string)
	v := Video{
		Name: strings.TrimSuffix(filepath.Base(first), filepath.Ext(first)),
		Dir:  filepath.Dir(first),
	}
	return v, map[string]any{"sources": sources}, nil
}
