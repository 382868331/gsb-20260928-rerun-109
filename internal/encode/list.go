package encode

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

var segRe = regexp.MustCompile(`^seg\d+\.m4s$`)

// ListSegments returns absolute paths of completed fMP4 segments in order.
func ListSegments(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && segRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var paths []string
	for _, n := range names {
		paths = append(paths, filepath.Join(dir, n))
	}
	return paths, nil
}
