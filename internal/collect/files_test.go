package collect

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func TestCollectFiles(t *testing.T) {
	inv, sum, err := CollectFiles("testdata/files", nil)
	if err != nil {
		t.Fatal(err)
	}

	if inv.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", inv.SchemaVersion)
	}
	if inv.ClusterID != "files" {
		t.Errorf("ClusterID = %q, want \"files\"", inv.ClusterID)
	}
	if inv.CollectedAt.IsZero() {
		t.Error("CollectedAt must be set")
	}

	if got := inv.Capabilities[inventory.CapAPIUsage]; !got.Available {
		t.Errorf("api-usage capability = %+v, want available", got)
	}
	for _, cap := range []inventory.Capability{
		inventory.CapDeprecatedCalls, inventory.CapHelm, inventory.CapAddOns, inventory.CapVersions,
	} {
		if got := inv.Capabilities[cap]; got.Available || got.Reason != "files mode" {
			t.Errorf("capability %s = %+v, want {false, \"files mode\"}", cap, got)
		}
	}

	want := []inventory.APIUsage{
		{Group: "", Version: "v1", Kind: "ConfigMap", Count: 1, Namespaces: map[string]int{"shop": 1},
			Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "cfg", File: "cm.json", Line: 1}}},
		{Group: "apps", Version: "v1", Kind: "Deployment", Count: 1, Namespaces: map[string]int{"shop": 1},
			Objects: []inventory.ObjectRef{{Namespace: "shop", Name: "web", File: "app.yaml", Line: 1}}},
		{Group: "networking.k8s.io", Version: "v1beta1", Kind: "Ingress", Count: 2, Namespaces: map[string]int{"internal": 1, "shop": 1},
			Objects: []inventory.ObjectRef{
				{Namespace: "shop", Name: "web", File: "app.yaml", Line: 7},
				{Namespace: "internal", Name: "admin", File: "app.yaml", Line: 13},
			}},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole", Count: 1, Namespaces: map[string]int{"": 1},
			Objects: []inventory.ObjectRef{{Name: "reader", File: "rbac.yml", Line: 1}}},
	}
	if !reflect.DeepEqual(inv.APIUsage, want) {
		t.Errorf("api usage = %#v\nwant     %#v", inv.APIUsage, want)
	}
	// notes.txt is walked but skipped (wrong extension).
	if want := (FilesSummary{Files: 4, Skipped: 1, Objects: 5}); !reflect.DeepEqual(sum, want) {
		t.Errorf("summary = %+v, want %+v", sum, want)
	}
}

func TestCollectFilesMissingDir(t *testing.T) {
	if _, _, err := CollectFiles("testdata/does-not-exist", nil); err == nil {
		t.Fatal("want error for missing directory")
	}
}

// writeTree creates files (slash paths → content) under a temp dir.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const ingressV1beta1 = `apiVersion: networking.k8s.io/v1beta1
kind: Ingress
metadata:
  name: web
  namespace: shop
`

// Non-manifest YAML/JSON in a repo (lists, scalars, kustomize patches,
// values/Chart files, workflows) is skipped silently; syntactically invalid
// files (unrendered chart templates, JSONC) become warnings, never a hard
// failure; a sibling manifest is still scanned.
func TestCollectFilesSkipsNonManifests(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"rendered.yaml":                  ingressV1beta1,
		"list.yaml":                      "- a\n- b\n",
		"array.json":                     `[{"apiVersion": "v1", "kind": "ConfigMap"}]`,
		"scalar.yaml":                    "just a string\n",
		"patch.yaml":                     "- op: replace\n  path: /spec/replicas\n  value: 3\n",
		"playbook.yml":                   "- hosts: all\n  tasks: []\n",
		"chart/Chart.yaml":               "apiVersion: v2\nname: demo\nversion: 0.1.0\n",
		"chart/values.yaml":              "replicaCount: 1\n",
		".github/workflows/ci.yml":       "on: push\njobs: {}\n",
		"chart/templates/configmap.yaml": "{{- if .Values.enabled }}\napiVersion: v1\nkind: ConfigMap\n{{- end }}\n",
		"tsconfig.json":                  "/* compiler options */\n{\"compilerOptions\": {}}\n",
	})

	inv, sum, err := CollectFiles(dir, nil)
	if err != nil {
		t.Fatalf("CollectFiles must not fail on non-manifest input: %v", err)
	}
	if len(inv.APIUsage) != 1 || inv.APIUsage[0].Kind != "Ingress" || inv.APIUsage[0].Count != 1 {
		t.Fatalf("api usage = %+v, want only the rendered Ingress", inv.APIUsage)
	}
	if sum.Files != 11 || sum.Skipped != 10 || sum.Objects != 1 {
		t.Errorf("summary = %+v, want 11 files, 10 skipped, 1 object", sum)
	}
	var warned []string
	for _, w := range sum.Warnings {
		warned = append(warned, w.File)
		if w.Err == nil || w.Line < 1 {
			t.Errorf("warning %+v: want an error and a line", w)
		}
	}
	if want := []string{"chart/templates/configmap.yaml", "tsconfig.json"}; !reflect.DeepEqual(warned, want) {
		t.Errorf("warned files = %v, want %v", warned, want)
	}
	if s := sum.Warnings[0].String(); !strings.HasPrefix(s, "chart/templates/configmap.yaml:1: ") {
		t.Errorf("warning string = %q, want file:line: error", s)
	}
}

// An unrendered chart template can be valid YAML (`name: {{ include ... }}`
// parses as a flow mapping), but an object whose name or namespace is not a
// string is not a Kubernetes object: it is warned about like any other
// unparseable document, not counted next to its rendered copy.
func TestCollectFilesUnrenderedTemplateDoc(t *testing.T) {
	dir := writeTree(t, map[string]string{"chart/templates/pdb.yaml": `apiVersion: policy/v1beta1
kind: PodDisruptionBudget
metadata:
  name: {{ include "chart.fullname" . }}
---
apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: ConfigMap
  metadata:
    name: ok
    namespace: {{ .Release.Namespace }}
`})
	inv, sum, err := CollectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.APIUsage) != 0 || sum.Objects != 0 || sum.Skipped != 1 {
		t.Errorf("api usage = %+v, summary %+v; want nothing counted", inv.APIUsage, sum)
	}
	if len(sum.Warnings) != 2 || sum.Warnings[0].Line != 1 || sum.Warnings[1].Line != 6 {
		t.Errorf("warnings = %+v, want one per document (lines 1 and 6)", sum.Warnings)
	}
}

// A document that could not be decoded but names an API the knowledge base
// lists as removed may hide a blocker: api-usage is then not assessed (a
// required gap, so the verdict is at least unknown), naming each such
// document. Other undecodable documents (JSONC, a template that names no
// removed API) stay warnings.
func TestCollectFilesUnassessedRemovedAPI(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	clean := map[string]string{
		"app.yaml":                  "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\n",
		"tsconfig.json":             "/* compiler options */\n{\"compilerOptions\": {}}\n",
		"chart/templates/svc.yaml":  "apiVersion: v1\nkind: Service\nmetadata:\n  name: {{ .Release.Name }}\n",
		"chart/templates/hpa.yaml":  "apiVersion: {{ include \"hpa.apiVersion\" . }}\nkind: HorizontalPodAutoscaler\n",
		"rendered/configmaps.json":  `{"apiVersion": "v1", "kind": "ConfigMap"}` + "\n{",
		"chart/templates/notes.txt": "apiVersion: policy/v1beta1\nkind: PodDisruptionBudget\n",
	}
	inv, sum, err := CollectFiles(writeTree(t, clean), k.APILifecycle)
	if err != nil {
		t.Fatal(err)
	}
	if st := inv.Capabilities[inventory.CapAPIUsage]; !st.Available || len(sum.Warnings) != 3 {
		t.Fatalf("api-usage = %+v with warnings %v; want available: no undecodable document names a removed API", st, sum.Warnings)
	}

	clean["chart/templates/pdb.yaml"] = "apiVersion: policy/v1beta1\nkind: PodDisruptionBudget\nmetadata:\n  name: {{ include \"chart.fullname\" . }}\n"
	clean["rendered/all.json"] = `{"apiVersion": "v1", "kind": "ConfigMap"}` + "\n" + `{"apiVersion": "v1", "kind": "ConfigMap"}` + "\n" + `{"apiVersion": "batch/v1beta1", "kind": "CronJob",`
	inv, _, err = CollectFiles(writeTree(t, clean), k.APILifecycle)
	if err != nil {
		t.Fatal(err)
	}
	st := inv.Capabilities[inventory.CapAPIUsage]
	if st.Available {
		t.Fatalf("api-usage = %+v, want not assessed", st)
	}
	for _, want := range []string{"2 document(s)", "chart/templates/pdb.yaml:1 (policy/v1beta1 PodDisruptionBudget)", "rendered/all.json:3 (batch/v1beta1 CronJob)"} {
		if !strings.Contains(st.Reason, want) {
			t.Errorf("reason %q lacks %q", st.Reason, want)
		}
	}
	if len(inv.APIUsage) == 0 {
		t.Error("the objects that were decoded must still be counted")
	}
}

// An invalid separator ("----", "---foo") makes the rest of the stream
// unsplittable, but the document before it is complete and still counted:
// dropping it would let a removed API slip out of the gate while the
// warning names only the separator line.
func TestCollectFilesInvalidSeparatorKeepsPrecedingDoc(t *testing.T) {
	dir := writeTree(t, map[string]string{"rendered.yaml": ingressV1beta1 + "----\napiVersion: v1\nkind: ConfigMap\n"})
	inv, sum, err := CollectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.APIUsage) != 1 || inv.APIUsage[0].Kind != "Ingress" || sum.Objects != 1 {
		t.Fatalf("api usage = %+v, summary %+v; want the Ingress before the separator", inv.APIUsage, sum)
	}
	if got := inv.APIUsage[0].Objects[0].Line; got != 1 {
		t.Errorf("ingress line = %d, want 1", got)
	}
	if len(sum.Warnings) != 1 || sum.Warnings[0].Line != 6 || !strings.Contains(sum.Warnings[0].Err.Error(), "separator") {
		t.Errorf("warnings = %+v, want one for the separator on line 6", sum.Warnings)
	}
}

// kubectl accepts an object with a duplicated identity key and sends the
// last value (#119): the object is counted that way, with a warning on the
// duplicate's line.
func TestCollectFilesDuplicateKeys(t *testing.T) {
	dir := writeTree(t, map[string]string{"dup.yaml": `apiVersion: networking.k8s.io/v1
kind: Ingress
apiVersion: extensions/v1beta1
metadata: {name: a}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: b
  name: c
---
` + ingressV1beta1})
	inv, sum, err := CollectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"extensions/v1beta1/Ingress": 1, "/v1/ConfigMap": 1, "networking.k8s.io/v1beta1/Ingress": 1}
	if got := usageCounts(inv); !reflect.DeepEqual(got, want) {
		t.Errorf("usage = %v, want %v (last values win)", got, want)
	}
	for _, u := range inv.APIUsage {
		if u.Kind == "ConfigMap" && u.Objects[0].Name != "c" {
			t.Errorf("ConfigMap = %+v, want the last name, c", u.Objects[0])
		}
	}
	if len(sum.Warnings) != 2 || sum.Warnings[0].Line != 3 || sum.Warnings[1].Line != 10 {
		t.Fatalf("warnings = %+v, want lines 3 and 10 (the duplicates)", sum.Warnings)
	}
	for _, w := range sum.Warnings {
		if !strings.Contains(w.Err.Error(), "duplicate") || w.Unassessed {
			t.Errorf("warning %v: want a duplicate-key warning on a counted object", w)
		}
	}
}

// `--files .` in a repository must not descend into VCS metadata or
// dependency trees: Go module vendor directories and node packages ship
// test manifests that are not the repository's, and every file in them
// would inflate the skipped count. A vendor/ without Go's modules.txt is
// kept: GitOps repositories vendor upstream manifests they deploy. Naming
// a skipped directory explicitly scans it.
func TestCollectFilesSkipsVCSAndDependencyDirs(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"rendered.yaml":                       ingressV1beta1,
		".git/HEAD":                           "ref: refs/heads/main\n",
		".git/hooks/x.yaml":                   ingressV1beta1,
		"node_modules/pkg/fixture.yaml":       ingressV1beta1,
		"vendor/modules.txt":                  "# k8s.io/api v0.30.0\n",
		"vendor/k8s.io/api/testdata/ing.yaml": ingressV1beta1,
		"deploy/vendor/upstream/ing.yml":      ingressV1beta1,
	})
	inv, sum, err := CollectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, u := range inv.APIUsage {
		for _, o := range u.Objects {
			files = append(files, o.File)
		}
	}
	if want := []string{"deploy/vendor/upstream/ing.yml", "rendered.yaml"}; !reflect.DeepEqual(files, want) || sum.Files != 2 || sum.Skipped != 0 {
		t.Errorf("objects in %v, summary %+v; want %v walked", files, sum, want)
	}
	inv, _, err = CollectFiles(filepath.Join(dir, "vendor"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.APIUsage) != 1 {
		t.Errorf("api usage = %+v, want the vendored manifest when vendor/ is the root", inv.APIUsage)
	}
}

// Skipped dependency trees are warned about, so a scan never silently
// covers less than the user pointed it at; VCS metadata stays silent.
func TestCollectFiles_SkippedDirsReported(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"rendered.yaml":                       ingressV1beta1,
		".git/hooks/x.yaml":                   ingressV1beta1,
		"node_modules/pkg/fixture.yaml":       ingressV1beta1,
		"vendor/modules.txt":                  "# k8s.io/api v0.30.0\n",
		"vendor/k8s.io/api/testdata/ing.yaml": ingressV1beta1,
	})
	_, sum, err := CollectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range sum.Warnings {
		got = append(got, w.String())
		if w.Unassessed || w.Line != 0 {
			t.Errorf("warning %+v: want a directory warning (no line)", w)
		}
	}
	if len(got) != 2 || !strings.HasPrefix(got[0], "node_modules: not walked") || !strings.HasPrefix(got[1], "vendor: not walked") {
		t.Errorf("warnings = %q, want node_modules and vendor, in walk order", got)
	}
}

// Symlinked directories below the root are not followed, as kubectl apply
// -R does not follow them (and following could leave the repository or
// loop); each is a warning instead of a silent gap. Symlinked files are
// read, as kubectl reads them.
func TestCollectFiles_SymlinkedDirWarns(t *testing.T) {
	dir := writeTree(t, map[string]string{"real/ing.yaml": ingressV1beta1, "outside/cron.yaml": "apiVersion: batch/v1beta1\nkind: CronJob\n"})
	if err := os.Symlink(filepath.Join(dir, "outside"), filepath.Join(dir, "real", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "real", "loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "outside", "cron.yaml"), filepath.Join(dir, "real", "cron-link.yaml")); err != nil {
		t.Fatal(err)
	}
	inv, sum, err := CollectFiles(filepath.Join(dir, "real"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := usageCounts(inv); !reflect.DeepEqual(got, map[string]int{"networking.k8s.io/v1beta1/Ingress": 1, "batch/v1beta1/CronJob": 1}) {
		t.Errorf("usage = %v, want the Ingress and the symlinked file's CronJob, once each", got)
	}
	var got []string
	for _, w := range sum.Warnings {
		got = append(got, w.String())
	}
	if len(got) != 2 || !strings.HasPrefix(got[0], "linked: symlinked directory not walked") || !strings.HasPrefix(got[1], "loop: symlinked directory not walked") {
		t.Errorf("warnings = %q, want one per symlinked directory", got)
	}
}

// A symlink named as the root is resolved and walked: `--files rendered`
// where rendered links to the output directory.
func TestCollectFiles_SymlinkRoot(t *testing.T) {
	dir := writeTree(t, map[string]string{"out/sub/ing.yaml": ingressV1beta1})
	link := filepath.Join(dir, "rendered")
	if err := os.Symlink(filepath.Join(dir, "out"), link); err != nil {
		t.Fatal(err)
	}
	inv, sum, err := CollectFiles(link, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.APIUsage) != 1 || inv.APIUsage[0].Objects[0].File != "sub/ing.yaml" || len(sum.Warnings) != 0 {
		t.Errorf("usage = %+v, warnings %v; want sub/ing.yaml, relative to the root", inv.APIUsage, sum.Warnings)
	}
}

// A file named explicitly is parsed whatever its extension
// (`kustomize build overlays/prod > rendered`).
func TestCollectFilesSingleFileAnyExtension(t *testing.T) {
	dir := writeTree(t, map[string]string{"rendered": ingressV1beta1})
	inv, sum, err := CollectFiles(filepath.Join(dir, "rendered"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.APIUsage) != 1 || sum.Objects != 1 {
		t.Fatalf("api usage = %+v, summary %+v; want the Ingress", inv.APIUsage, sum)
	}
	if got := inv.APIUsage[0].Objects[0].File; got != "rendered" {
		t.Errorf("file = %q, want the file's base name", got)
	}
}

// helm template output carries "# Source:" comments naming the template
// each document was rendered from.
func TestCollectFilesHelmSource(t *testing.T) {
	dir := writeTree(t, map[string]string{"out/all.yaml": `---
# Source: demo/templates/ingress.yaml
apiVersion: networking.k8s.io/v1beta1
kind: Ingress
metadata:
  name: web
---
# Source: demo/templates/cronjob.yaml
apiVersion: batch/v1beta1
kind: CronJob
metadata:
  name: nightly
`})
	inv, _, err := CollectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]inventory.ObjectRef{}
	for _, u := range inv.APIUsage {
		got[u.Kind] = u.Objects[0]
	}
	want := map[string]inventory.ObjectRef{
		"CronJob": {Name: "nightly", File: "out/all.yaml", Line: 9, RenderedFrom: "demo/templates/cronjob.yaml"},
		"Ingress": {Name: "web", File: "out/all.yaml", Line: 3, RenderedFrom: "demo/templates/ingress.yaml"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("objects = %+v\nwant      %+v", got, want)
	}
}

// Object refs are capped per GVK; the overflow is counted, not dropped
// silently.
func TestCollectFilesCapsObjectRefs(t *testing.T) {
	var b strings.Builder
	n := inventory.MaxObjectRefs + 5
	for i := range n {
		fmt.Fprintf(&b, "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm-%d\n", i)
	}
	dir := writeTree(t, map[string]string{"many.yaml": b.String()})
	inv, _, err := CollectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := inv.APIUsage[0]
	if u.Count != n || len(u.Objects) != inventory.MaxObjectRefs || u.ObjectsOmitted != 5 {
		t.Fatalf("count=%d objects=%d omitted=%d, want %d/%d/5", u.Count, len(u.Objects), u.ObjectsOmitted, n, inventory.MaxObjectRefs)
	}
	if u.Objects[0].Name != "cm-0" || u.Objects[0].Line != 2 {
		t.Errorf("first ref = %+v, want cm-0 at line 2 (collection order)", u.Objects[0])
	}
}

// usageCounts reduces an inventory to GVK → count for comparisons that
// should not depend on lines.
func usageCounts(inv inventory.Inventory) map[string]int {
	m := map[string]int{}
	for _, u := range inv.APIUsage {
		m[u.Group+"/"+u.Version+"/"+u.Kind] = u.Count
	}
	return m
}

// kind: List (and typed *List) wrappers are expanded: the items are what
// the apiserver would store, so they are what gets assessed.
func TestCollectFilesExpandsLists(t *testing.T) {
	stream := `apiVersion: networking.k8s.io/v1beta1
kind: Ingress
metadata:
  name: web
  namespace: shop
---
apiVersion: policy/v1beta1
kind: PodSecurityPolicy
metadata:
  name: restricted
`
	list := `apiVersion: v1
kind: List
items:
- apiVersion: networking.k8s.io/v1beta1
  kind: Ingress
  metadata:
    name: web
    namespace: shop
- apiVersion: policy/v1beta1
  kind: PodSecurityPolicy
  metadata:
    name: restricted
`
	dir := writeTree(t, map[string]string{"stream/s.yaml": stream, "list/l.yaml": list})
	want, _, err := CollectFiles(filepath.Join(dir, "stream"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := CollectFiles(filepath.Join(dir, "list"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(usageCounts(got), usageCounts(want)) {
		t.Errorf("List usage = %v, want the multi-doc stream's %v", usageCounts(got), usageCounts(want))
	}
	ing := got.APIUsage[0]
	if want := (inventory.ObjectRef{Namespace: "shop", Name: "web", File: "l.yaml", Line: 4}); !reflect.DeepEqual(ing.Objects, []inventory.ObjectRef{want}) {
		t.Errorf("list item ref = %+v, want %+v (the item's own apiVersion line)", ing.Objects, want)
	}
}

func TestCollectFilesListVariants(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want map[string]int
	}{
		{"typed IngressList", `apiVersion: networking.k8s.io/v1beta1
kind: IngressList
items:
- apiVersion: networking.k8s.io/v1beta1
  kind: Ingress
  metadata: {name: a}
- apiVersion: networking.k8s.io/v1beta1
  kind: Ingress
  metadata: {name: b}
`, map[string]int{"networking.k8s.io/v1beta1/Ingress": 2}},
		{"nested List", `apiVersion: v1
kind: List
items:
- apiVersion: v1
  kind: List
  items:
  - apiVersion: batch/v1beta1
    kind: CronJob
    metadata: {name: nightly}
- apiVersion: apps/v1
  kind: Deployment
  metadata: {name: web}
`, map[string]int{"batch/v1beta1/CronJob": 1, "apps/v1/Deployment": 1}},
		{"JSON List", `{"apiVersion": "v1", "kind": "List", "items": [
  {"apiVersion": "policy/v1beta1", "kind": "PodSecurityPolicy", "metadata": {"name": "p"}}
]}`, map[string]int{"policy/v1beta1/PodSecurityPolicy": 1}},
		// kubectl decodes any object with items as a list (#119; the
		// trade-off of #13): a custom resource whose kind ends in List
		// is expanded too, its untyped items inferred.
		{"CRD-like *List with untyped items is a list, as kubectl decodes it", `apiVersion: example.com/v1
kind: AllowList
metadata: {name: office}
items:
- cidr: 10.0.0.0/8
`, map[string]int{"example.com/v1/Allow": 1}},
		{"untyped List items are skipped", `apiVersion: v1
kind: List
items:
- just: data
`, map[string]int{}},
		{"List with null items is an empty wrapper", "apiVersion: v1\nkind: List\nitems:\n", map[string]int{}},
		{"List without items is an empty wrapper", "apiVersion: v1\nkind: List\nmetadata: {}\n", map[string]int{}},
		{"CRD-like *List with empty items is an empty list", `apiVersion: example.com/v1
kind: AllowList
metadata: {name: none}
items: []
`, map[string]int{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := CollectManifests(strings.NewReader(tc.doc))
			if err != nil {
				t.Fatal(err)
			}
			if got := usageCounts(inv); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("usage = %v, want %v", got, tc.want)
			}
		})
	}
}

// apiName matches "group/version/Kind" values a Kubernetes API can have.
var apiName = regexp.MustCompile(`^([a-z0-9][-a-z0-9.]*)?/[a-z0-9]+/[A-Za-z][A-Za-z0-9]*$`)

// FuzzScanManifestStream: arbitrary input never panics, every object found
// carries a positive line, and every object kubectl's own decoder would
// apply (kubectlObjects) is counted or named by a part reported as not
// assessed — the scan never silently drops what kubectl sends.
func FuzzScanManifestStream(f *testing.F) {
	corpus, err := filepath.Glob("testdata/adversarial/*")
	if err != nil {
		f.Fatal(err)
	}
	for _, name := range corpus {
		raw, err := os.ReadFile(name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(string(raw))
	}
	for _, seed := range []string{
		ingressV1beta1,
		"- a\n- b\n",
		"[1, 2]",
		"scalar",
		"- op: replace\n  path: /x\n",
		"apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: ConfigMap\n",
		"--- # c\napiVersion: v1\nkind: [broken\n",
		"---x\n",
		"a: &a {apiVersion: v1, kind: ConfigMap}\nb: *a\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		objs, bad, err := parseManifestStream(strings.NewReader(s))
		if err != nil {
			t.Fatalf("in-memory reader cannot fail, got %v", err)
		}
		seen := map[string]bool{}
		for _, o := range objs {
			if o.ref.Line < 1 || o.kind == "" || o.version == "" {
				t.Fatalf("object %+v: want kind, version and a positive line", o)
			}
			seen[o.group+"/"+o.version+"/"+o.kind] = true
		}
		for _, b := range bad {
			for _, g := range namedAPIs(b.unassessed) {
				seen[g.group+"/"+g.version+"/"+g.kind] = true
			}
		}
		kubectl, _ := kubectlObjects(s)
		for _, g := range kubectl {
			// Only names an API can have: the knowledge base flags no
			// other, and text that did not decode is read for such names.
			if !seen[g] && apiName.MatchString(g) {
				t.Fatalf("kubectl applies %s, which is neither counted nor named by an unassessed part (bad %+v)", g, bad)
			}
		}
	})
}
