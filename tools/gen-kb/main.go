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
// IMPORTANT — when bumping k8s.io/api: reconcile the import list below
// against `go list k8s.io/api/...`. A new group/version package that is
// missing from the imports still compiles and the CI freshness check still
// passes (the dataset just silently lacks that group); only the
// import-list drift check in CI (and this note) guard against it.
package main

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

	admissionv1 "k8s.io/api/admission/v1"
	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	admregv1 "k8s.io/api/admissionregistration/v1"
	admregv1alpha1 "k8s.io/api/admissionregistration/v1alpha1"
	admregv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
	apidiscoveryv2beta1 "k8s.io/api/apidiscovery/v2beta1"
	apiserverinternalv1alpha1 "k8s.io/api/apiserverinternal/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	appsv1beta1 "k8s.io/api/apps/v1beta1"
	appsv1beta2 "k8s.io/api/apps/v1beta2"
	authnv1 "k8s.io/api/authentication/v1"
	authnv1alpha1 "k8s.io/api/authentication/v1alpha1"
	authnv1beta1 "k8s.io/api/authentication/v1beta1"
	authzv1 "k8s.io/api/authorization/v1"
	authzv1beta1 "k8s.io/api/authorization/v1beta1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	certsv1 "k8s.io/api/certificates/v1"
	certsv1alpha1 "k8s.io/api/certificates/v1alpha1"
	certsv1beta1 "k8s.io/api/certificates/v1beta1"
	coordv1 "k8s.io/api/coordination/v1"
	coordv1alpha2 "k8s.io/api/coordination/v1alpha2"
	coordv1beta1 "k8s.io/api/coordination/v1beta1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	discoveryv1beta1 "k8s.io/api/discovery/v1beta1"
	eventsv1 "k8s.io/api/events/v1"
	eventsv1beta1 "k8s.io/api/events/v1beta1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	flowcontrolv1 "k8s.io/api/flowcontrol/v1"
	flowcontrolv1beta1 "k8s.io/api/flowcontrol/v1beta1"
	flowcontrolv1beta2 "k8s.io/api/flowcontrol/v1beta2"
	flowcontrolv1beta3 "k8s.io/api/flowcontrol/v1beta3"
	imagepolicyv1alpha1 "k8s.io/api/imagepolicy/v1alpha1"
	networkingv1 "k8s.io/api/networking/v1"
	networkingv1beta1 "k8s.io/api/networking/v1beta1"
	nodev1 "k8s.io/api/node/v1"
	nodev1alpha1 "k8s.io/api/node/v1alpha1"
	nodev1beta1 "k8s.io/api/node/v1beta1"
	policyv1 "k8s.io/api/policy/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	rbacv1 "k8s.io/api/rbac/v1"
	rbacv1alpha1 "k8s.io/api/rbac/v1alpha1"
	rbacv1beta1 "k8s.io/api/rbac/v1beta1"
	resourcev1 "k8s.io/api/resource/v1"
	resourcev1alpha3 "k8s.io/api/resource/v1alpha3"
	resourcev1beta1 "k8s.io/api/resource/v1beta1"
	resourcev1beta2 "k8s.io/api/resource/v1beta2"
	schedulingv1 "k8s.io/api/scheduling/v1"
	schedulingv1alpha2 "k8s.io/api/scheduling/v1alpha2"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	storagev1 "k8s.io/api/storage/v1"
	storagev1alpha1 "k8s.io/api/storage/v1alpha1"
	storagev1beta1 "k8s.io/api/storage/v1beta1"
	storagemigrationv1beta1 "k8s.io/api/storagemigration/v1beta1"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var addToSchemes = []func(*runtime.Scheme) error{
	admissionv1.AddToScheme, admissionv1beta1.AddToScheme,
	admregv1.AddToScheme, admregv1alpha1.AddToScheme, admregv1beta1.AddToScheme,
	apidiscoveryv2.AddToScheme, apidiscoveryv2beta1.AddToScheme,
	apiserverinternalv1alpha1.AddToScheme,
	appsv1.AddToScheme, appsv1beta1.AddToScheme, appsv1beta2.AddToScheme,
	authnv1.AddToScheme, authnv1alpha1.AddToScheme, authnv1beta1.AddToScheme,
	authzv1.AddToScheme, authzv1beta1.AddToScheme,
	autoscalingv1.AddToScheme, autoscalingv2.AddToScheme,
	batchv1.AddToScheme, batchv1beta1.AddToScheme,
	certsv1.AddToScheme, certsv1alpha1.AddToScheme, certsv1beta1.AddToScheme,
	coordv1.AddToScheme, coordv1alpha2.AddToScheme, coordv1beta1.AddToScheme,
	corev1.AddToScheme,
	discoveryv1.AddToScheme, discoveryv1beta1.AddToScheme,
	eventsv1.AddToScheme, eventsv1beta1.AddToScheme,
	extensionsv1beta1.AddToScheme,
	flowcontrolv1.AddToScheme, flowcontrolv1beta1.AddToScheme,
	flowcontrolv1beta2.AddToScheme, flowcontrolv1beta3.AddToScheme,
	imagepolicyv1alpha1.AddToScheme,
	networkingv1.AddToScheme, networkingv1beta1.AddToScheme,
	nodev1.AddToScheme, nodev1alpha1.AddToScheme, nodev1beta1.AddToScheme,
	policyv1.AddToScheme, policyv1beta1.AddToScheme,
	rbacv1.AddToScheme, rbacv1alpha1.AddToScheme, rbacv1beta1.AddToScheme,
	resourcev1.AddToScheme, resourcev1alpha3.AddToScheme,
	resourcev1beta1.AddToScheme, resourcev1beta2.AddToScheme,
	schedulingv1.AddToScheme, schedulingv1alpha2.AddToScheme, schedulingv1beta1.AddToScheme,
	storagev1.AddToScheme, storagev1alpha1.AddToScheme, storagev1beta1.AddToScheme,
	storagemigrationv1beta1.AddToScheme,
}

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
