// Command gen-kb extracts API lifecycle data from a pinned k8s.io/api:
// it registers every standard group into a runtime.Scheme, walks all known
// types, and type-asserts each against the generated APILifecycle* method
// interfaces — the same pattern the apiserver's
// k8s.io/apiserver/pkg/endpoints/deprecation package uses.
//
// Output JSON mirrors internal/kb's lifecycleFile / APILifecycleEntry shape
// (this module cannot import internal/kb; the sanity test in internal/kb
// and the CI freshness check keep the shapes in sync).
//
// The group/version imports live in zz_generated_imports.go, written by
// internal/genimports from `go list k8s.io/api/...`. After bumping
// k8s.io/api, `go generate ./...` is the only step: it rewrites the import
// list, tidies go.mod, and regenerates the dataset. Types upstream has
// deleted become tombstones, removed in the release that deleted them:
// every run reads the k8s.io/api releases since historyFrom from source
// (see deletedTypes), and entries of the previously written dataset are
// carried forward (see carryForward), never dropped.
package main

//go:generate go run ./internal/genimports -out zz_generated_imports.go
//go:generate go mod tidy
//go:generate go run . -out ../../internal/kb/data/apilifecycle.json

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"reflect"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Private lifecycle interfaces — the generated zz_generated.prerelease-lifecycle.go
// methods on each type satisfy these (Deprecated/Removed/Replacement only
// where applicable).
type introducedIface interface{ APILifecycleIntroduced() (int, int) }
type deprecatedIface interface{ APILifecycleDeprecated() (int, int) }
type removedIface interface{ APILifecycleRemoved() (int, int) }
type replacementIface interface {
	APILifecycleReplacement() schema.GroupVersionKind
}

// JSON mirrors of internal/kb types. version intentionally mirrors
// inventory.Version's JSON form — it marshals to the same canonical string
// ("1.38") as inventory.Version.MarshalJSON. It CANNOT import
// internal/inventory: gen-kb is a separate Go module pinned to its own
// k8s.io/api release, so importing the main module would entangle the two
// dependency graphs. Drift between this mirror and the real type is caught
// by internal/kb's dataset tests (which unmarshal the generated JSON with
// inventory.Version) and the CI kb-freshness job.
type version struct{ Major, Minor int }

func (v version) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

func (v version) MarshalJSON() ([]byte, error) { return json.Marshal(v.String()) }

// before reports whether v is an earlier release than o.
func (v version) before(o version) bool {
	return v.Major < o.Major || v.Major == o.Major && v.Minor < o.Minor
}

// UnmarshalJSON reads the canonical "1.36" form back, so gen-kb can load
// the previously committed dataset (see carryForward).
func (v *version) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("version: want a \"major.minor\" string, got %s", b)
	}
	if _, err := fmt.Sscanf(s, "%d.%d", &v.Major, &v.Minor); err != nil || v.String() != s {
		return fmt.Errorf("version: invalid %q", s)
	}
	return nil
}

type gvkOut struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

type entry struct {
	Group       string   `json:"group"`
	Version     string   `json:"version"`
	Kind        string   `json:"kind"`
	Introduced  version  `json:"introduced"`
	Deprecated  *version `json:"deprecated,omitempty"`
	Removed     *version `json:"removed,omitempty"`
	Replacement *gvkOut  `json:"replacement,omitempty"`
	// RemovedInferred marks a tombstone: upstream deleted the type's
	// package without ever tagging a removal, so Removed is the k8s.io/api
	// minor in which it disappeared (see carryForward).
	RemovedInferred bool `json:"removedInferred,omitempty"`
}

func (e entry) gvk() gvkOut { return gvkOut{Group: e.Group, Version: e.Version, Kind: e.Kind} }

// less orders entries by group, version, kind: the dataset's order.
func (e entry) less(o entry) bool {
	if e.Group != o.Group {
		return e.Group < o.Group
	}
	if e.Version != o.Version {
		return e.Version < o.Version
	}
	return e.Kind < o.Kind
}

type output struct {
	GeneratedFrom string  `json:"generatedFrom"`
	MaxKnownK8s   string  `json:"maxKnownK8s"`
	Entries       []entry `json:"entries"`
}

func main() {
	out := flag.String("out", "", "output path for apilifecycle.json (required)")
	flag.Parse()
	if *out == "" {
		log.Fatal("gen-kb: -out is required")
	}

	scheme := runtime.NewScheme()
	for _, add := range addToSchemes {
		if err := add(scheme); err != nil {
			log.Fatalf("gen-kb: AddToScheme: %v", err)
		}
	}

	entries, upstream, noLifecycle := extract(scheme)

	// A near-empty result means the APILifecycle* type assertions stopped
	// matching (e.g. upstream renamed the generated methods) — refuse to
	// write a dataset that would make every scan silently green.
	if len(entries) < 100 {
		log.Fatalf("gen-kb: only %d entries extracted (want >= 100) — did upstream rename the APILifecycle* methods?", len(entries))
	}
	sort.Strings(noLifecycle)
	for _, s := range noLifecycle {
		log.Printf("gen-kb: skipped %s (no APILifecycle* methods)", s)
	}

	apiVer := k8sAPIModuleVersion()
	maxKnown := maxKnownK8s(apiVer)

	// Types upstream deleted before any dataset recorded them, from the
	// k8s.io/api releases since historyFrom. They join the fresh entries,
	// so they are rederived on every run rather than only carried forward.
	hist, err := readHistory(historyFrom, maxKnown.Minor)
	if err != nil {
		log.Fatalf("gen-kb: reading k8s.io/api history: %v", err)
	}
	deleted := deletedTypes(hist, upstream)
	for _, e := range deleted {
		log.Printf("gen-kb: history: %s/%s %s gone upstream (removed %s, inferred=%v)",
			e.Group, e.Version, e.Kind, e.Removed, e.RemovedInferred)
	}
	entries = append(entries, deleted...)

	prev, err := readDataset(*out)
	if err != nil {
		log.Fatalf("gen-kb: reading previous dataset: %v", err)
	}
	entries, tombstoned := carryForward(prev, entries, upstream, maxKnown)
	for _, e := range tombstoned {
		log.Printf("gen-kb: carried forward %s/%s %s (gone upstream; removed %s, inferred=%v)",
			e.Group, e.Version, e.Kind, e.Removed, e.RemovedInferred)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].less(entries[j]) })

	doc := output{
		GeneratedFrom: "k8s.io/api " + apiVer,
		MaxKnownK8s:   maxKnown.String(),
		Entries:       entries,
	}
	buf, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		log.Fatalf("gen-kb: marshal: %v", err)
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(*out, buf, 0o644); err != nil {
		log.Fatalf("gen-kb: write %s: %v", *out, err)
	}
	fmt.Printf("gen-kb: wrote %d entries (k8s.io/api %s, maxKnownK8s %s) to %s\n",
		len(entries), apiVer, doc.MaxKnownK8s, *out)
}

// extract walks every type registered in scheme: entries holds the ones
// with generated lifecycle data, upstream every registered GVK, and
// noLifecycle ("group/version Kind") the registered ones without.
func extract(scheme *runtime.Scheme) (entries []entry, upstream map[gvkOut]bool, noLifecycle []string) {
	upstream = map[gvkOut]bool{}
	for k, t := range scheme.AllKnownTypes() {
		if skipKind(k) {
			continue
		}
		upstream[gvkOut{Group: k.Group, Version: k.Version, Kind: k.Kind}] = true
		obj := reflect.New(t).Interface()
		in, ok := obj.(introducedIface)
		if !ok {
			noLifecycle = append(noLifecycle, k.GroupVersion().String()+" "+k.Kind)
			continue // no generated lifecycle data for this type
		}
		maj, min := in.APILifecycleIntroduced()
		e := entry{
			Group: k.Group, Version: k.Version, Kind: k.Kind,
			Introduced: version{Major: maj, Minor: min},
		}
		if d, ok := obj.(deprecatedIface); ok {
			if maj, min := d.APILifecycleDeprecated(); maj != 0 || min != 0 {
				e.Deprecated = &version{Major: maj, Minor: min}
			}
		}
		if r, ok := obj.(removedIface); ok {
			if maj, min := r.APILifecycleRemoved(); maj != 0 || min != 0 {
				e.Removed = &version{Major: maj, Minor: min}
			}
		}
		if r, ok := obj.(replacementIface); ok {
			if g := r.APILifecycleReplacement(); !g.Empty() {
				e.Replacement = &gvkOut{Group: g.Group, Version: g.Version, Kind: g.Kind}
			}
		}
		fixReplacement(&e)
		entries = append(entries, e)
	}
	return entries, upstream, noLifecycle
}

func skipKind(k schema.GroupVersionKind) bool {
	if k.Version == runtime.APIVersionInternal {
		return true
	}
	if isNonPersisted(gvkOut{Group: k.Group, Version: k.Version, Kind: k.Kind}) {
		return true
	}
	if strings.HasSuffix(k.Kind, "List") || strings.HasSuffix(k.Kind, "Options") {
		return true
	}
	switch k.Kind { // apimachinery plumbing registered into every group
	case "WatchEvent", "Status", "APIGroup", "APIGroupList", "APIVersions", "APIResourceList":
		return true
	}
	return false
}

func k8sAPIModuleVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		log.Fatal("gen-kb: no build info (build with module support)")
	}
	for _, dep := range bi.Deps {
		if dep.Path == "k8s.io/api" {
			return dep.Version
		}
	}
	log.Fatal("gen-kb: k8s.io/api not found in build info")
	return ""
}

// maxKnownK8s maps a k8s.io/api module version to the Kubernetes minor it
// tracks: "v0.36.1" → 1.36.
func maxKnownK8s(apiVersion string) version {
	parts := strings.Split(strings.TrimPrefix(apiVersion, "v"), ".")
	if len(parts) < 2 || parts[0] != "0" {
		log.Fatalf("gen-kb: unexpected k8s.io/api version %q", apiVersion)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		log.Fatalf("gen-kb: unexpected k8s.io/api version %q", apiVersion)
	}
	if minor == 0 {
		// A pseudo-version like v0.0.0-20260101000000-abcdef would silently
		// map to "1.0"; require a real tagged release instead.
		log.Fatalf("gen-kb: k8s.io/api version %q looks like a pseudo-version; pin a tagged release", apiVersion)
	}
	return version{Major: 1, Minor: minor}
}
