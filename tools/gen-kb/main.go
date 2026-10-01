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
// list, tidies go.mod, and regenerates the dataset.
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

func (v version) MarshalJSON() ([]byte, error) {
	return json.Marshal(fmt.Sprintf("%d.%d", v.Major, v.Minor))
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

	var entries []entry
	for k, t := range scheme.AllKnownTypes() {
		if skipKind(k) {
			continue
		}
		obj := reflect.New(t).Interface()
		in, ok := obj.(introducedIface)
		if !ok {
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
		entries = append(entries, e)
	}

	// A near-empty result means the APILifecycle* type assertions stopped
	// matching (e.g. upstream renamed the generated methods) — refuse to
	// write a dataset that would make every scan silently green.
	if len(entries) < 100 {
		log.Fatalf("gen-kb: only %d entries extracted (want >= 100) — did upstream rename the APILifecycle* methods?", len(entries))
	}

	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Kind < b.Kind
	})

	apiVer := k8sAPIModuleVersion()
	doc := output{
		GeneratedFrom: "k8s.io/api " + apiVer,
		MaxKnownK8s:   maxKnownK8s(apiVer),
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

func skipKind(k schema.GroupVersionKind) bool {
	if k.Version == runtime.APIVersionInternal {
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
// tracks: "v0.36.1" → "1.36".
func maxKnownK8s(apiVersion string) string {
	parts := strings.Split(strings.TrimPrefix(apiVersion, "v"), ".")
	if len(parts) < 2 || parts[0] != "0" {
		log.Fatalf("gen-kb: unexpected k8s.io/api version %q", apiVersion)
	}
	if parts[1] == "0" {
		// A pseudo-version like v0.0.0-20260101000000-abcdef would silently
		// map to "1.0"; require a real tagged release instead.
		log.Fatalf("gen-kb: k8s.io/api version %q looks like a pseudo-version; pin a tagged release", apiVersion)
	}
	return "1." + parts[1]
}
