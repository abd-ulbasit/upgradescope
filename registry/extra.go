// registry/extra.go
package registry

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// LoadExtra parses and validates operator-supplied registry entries: one
// <id>.yaml file, or every *.yaml file of a directory, each under the same
// rules as an embedded entry (schema, citations, id = file name). An error
// names the file it is about. A path that yields no entry is an error, not
// "nothing extra": an unmounted ConfigMap would otherwise leave the
// operator's add-ons silently unjudged.
func LoadExtra(path string) ([]AddOn, error) {
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	parent, base := filepath.Dir(path), filepath.Base(path)
	var addons []AddOn
	if info.IsDir() {
		addons, err = loadFS(os.DirFS(parent), base)
	} else {
		if !strings.HasSuffix(base, ".yaml") {
			return nil, fmt.Errorf("registry: %s: registry entries must use the .yaml extension", path)
		}
		addons, err = loadFS(onlyFile{os.DirFS(parent), base}, ".")
	}
	if err != nil {
		return nil, err
	}
	if len(addons) == 0 {
		return nil, fmt.Errorf("registry: no *.yaml registry entries in %s", path)
	}
	return addons, nil
}

// onlyFile is an fs.FS whose root directory lists a single file.
type onlyFile struct {
	fs.FS
	name string
}

func (f onlyFile) ReadDir(dir string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(f.FS, dir)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(entries, func(e fs.DirEntry) bool { return e.Name() != f.name }), nil
}

// Merge returns base with extra applied, sorted by ID: an extra entry
// replaces the base entry with its ID outright (no field is inherited), and
// one with a new ID is added. base is not modified.
func Merge(base, extra []AddOn) []AddOn {
	out := slices.DeleteFunc(slices.Clone(base), func(a AddOn) bool {
		return slices.ContainsFunc(extra, func(e AddOn) bool { return e.ID == a.ID })
	})
	out = append(out, extra...)
	slices.SortFunc(out, func(a, b AddOn) int { return strings.Compare(a.ID, b.ID) })
	return out
}
