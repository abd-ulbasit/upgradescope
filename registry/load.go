// registry/load.go
package registry

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

//go:embed data/*.yaml
var dataFS embed.FS

// Load parses and validates every embedded registry entry.
// Entries are returned sorted by ID; any parse/validation error or
// duplicate ID fails the whole load.
func Load() ([]AddOn, error) {
	return loadFS(dataFS, "data")
}

func loadFS(fsys fs.FS, dir string) ([]AddOn, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("registry: read dir %s: %w", dir, err)
	}
	var (
		addons []AddOn
		errs   []error
		seen   = map[string]string{} // id → file that defined it
	)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Convention: registry entries are .yaml-only, matching the
		// data/*.yaml embed glob. A .yml file in the source tree would
		// never reach the embedded FS and so be silently invisible —
		// reject the extension loudly instead of ignoring it. (The embed
		// directive cannot carry a data/*.yml guard glob: go:embed fails
		// the build when a glob matches nothing.)
		if strings.HasSuffix(e.Name(), ".yml") {
			errs = append(errs, fmt.Errorf("registry: %s/%s: registry entries must use the .yaml extension (rename to .yaml)", dir, e.Name()))
			continue
		}
		if !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		name := dir + "/" + e.Name()
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("registry: read %s: %w", name, err)
		}
		var a AddOn
		if err := yaml.UnmarshalStrict(raw, &a); err != nil {
			errs = append(errs, fmt.Errorf("registry: parse %s: %w", name, err))
			continue
		}
		if prev, dup := seen[a.ID]; dup {
			errs = append(errs, fmt.Errorf("registry: duplicate id %q in %s (already defined in %s)", a.ID, name, prev))
			continue
		}
		seen[a.ID] = name
		for _, verr := range Validate(a) {
			errs = append(errs, fmt.Errorf("registry: %s: %w", name, verr))
		}
		addons = append(addons, a)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	sort.Slice(addons, func(i, j int) bool { return addons[i].ID < addons[j].ID })
	return addons, nil
}
