package kb

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// migrationsJSON is data/migrations.json, hand-authored: what a manifest
// needs besides a new apiVersion, for the entries whose replacement is no
// drop-in (a changed schema) or that have none at all
// (PodSecurityPolicy). tools/gen-kb and the weekly kb-refresh never write
// it, so a regeneration cannot drop a note (#330).
//
//go:embed data/migrations.json
var migrationsJSON []byte

// Migration is a hand-written note on moving off an API version, with the
// pages it comes from. The engine appends Note to the generated hint of a
// finding for the entry, or uses it alone when there is none, and
// appends Citations to the finding's.
type Migration struct {
	Note      string   `json:"note"`
	Citations []string `json:"citations"`
}

// migrationEntry is one element of data/migrations.json, keyed by the
// lifecycle entry it annotates.
type migrationEntry struct {
	Group     string   `json:"group"` // "" for core
	Version   string   `json:"version"`
	Kind      string   `json:"kind"`
	Note      string   `json:"note"`
	Citations []string `json:"citations"`
}

type migrationsFile struct {
	Migrations []migrationEntry `json:"migrations"`
}

// applyMigrations merges data/migrations.json onto entries (setting each
// annotated entry's Migration) and refuses a file that cannot be trusted:
// a key with no lifecycle entry, a duplicate key, an empty note, or a note
// without an https citation. A silently ignored note would be a silently
// missing fix.
func applyMigrations(entries []APILifecycleEntry, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var f migrationsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("kb: corrupt migrations.json: %w", err)
	}
	at := make(map[GVK]int, len(entries))
	for i, e := range entries {
		at[GVK{Group: e.Group, Version: e.Version, Kind: e.Kind}] = i
	}
	seen := map[GVK]bool{}
	for _, m := range f.Migrations {
		key := GVK{Group: m.Group, Version: m.Version, Kind: m.Kind}
		name := fmt.Sprintf("%s/%s %s", m.Group, m.Version, m.Kind)
		i, ok := at[key]
		switch {
		case !ok:
			return fmt.Errorf("kb: migrations.json has a note for %s, which is not in apilifecycle.json", name)
		case seen[key]:
			return fmt.Errorf("kb: migrations.json has two notes for %s", name)
		case strings.TrimSpace(m.Note) == "":
			return fmt.Errorf("kb: migrations.json note for %s is empty", name)
		case !hasHTTPSCitation(m.Citations):
			return fmt.Errorf("kb: migrations.json note for %s has no https citation", name)
		}
		seen[key] = true
		entries[i].Migration = &Migration{Note: m.Note, Citations: append([]string(nil), m.Citations...)}
	}
	return nil
}

func hasHTTPSCitation(cs []string) bool {
	for _, c := range cs {
		if strings.HasPrefix(c, "https://") && len(c) > len("https://") {
			return true
		}
	}
	return false
}
