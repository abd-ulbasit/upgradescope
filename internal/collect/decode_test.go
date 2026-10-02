package collect

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// streamResult reduces parseManifestStream's output to what the corpus
// asserts: objects per "group/version/Kind", object lines in stream order,
// the problems that left part of the stream unassessed, and the API names
// those parts mention.
type streamResult struct {
	counts     map[string]int
	lines      []int
	unassessed []docError
	warnings   []docError // decoded anyway (duplicate keys)
	named      map[string]bool
}

func parseResult(t *testing.T, s string) streamResult {
	t.Helper()
	objs, bad, err := parseManifestStream(strings.NewReader(s))
	if err != nil {
		t.Fatalf("parseManifestStream: %v", err)
	}
	r := streamResult{counts: map[string]int{}, named: map[string]bool{}}
	for _, o := range objs {
		r.counts[o.group+"/"+o.version+"/"+o.kind]++
		r.lines = append(r.lines, o.ref.Line)
	}
	for _, b := range bad {
		if b.unassessed == nil {
			r.warnings = append(r.warnings, b)
			continue
		}
		r.unassessed = append(r.unassessed, b)
		for _, g := range namedAPIs(b.unassessed) {
			r.named[g.group+"/"+g.version+"/"+g.kind] = true
		}
	}
	return r
}

// The regression corpus (testdata/adversarial): each red-team input with
// what the scan counts, and the API names an unassessed part carries (a
// removed one makes the verdict at least unknown, see CollectFiles). Every
// case is also decoded by kubectl's own decoder (kubectlObjects): whatever
// kubectl apply would send must be counted, or named by a part that was
// not assessed — never silently dropped.
func TestAdversarialCorpus(t *testing.T) {
	const (
		deploy  = "apps/v1/Deployment"
		cm      = "/v1/ConfigMap"
		ingOld  = "extensions/v1beta1/Ingress"
		ingNew  = "networking.k8s.io/v1/Ingress"
		cronOld = "batch/v1beta1/CronJob"
		pdbOld  = "policy/v1beta1/PodDisruptionBudget"
	)
	cases := map[string]struct {
		counts   map[string]int
		lines    []int
		named    []string // must be named by an unassessed part
		warnings int      // decoded-anyway warnings
	}{
		"ndjson.json":                        {counts: map[string]int{deploy: 1, ingOld: 1}, lines: []int{1, 2}},
		"pretty.json":                        {counts: map[string]int{deploy: 1, ingOld: 1}, lines: []int{2, 9}},
		"adjacent.json":                      {counts: map[string]int{deploy: 1, ingOld: 1}, lines: []int{1, 1}},
		"tab-indented.json":                  {counts: map[string]int{deploy: 1, ingOld: 1}, lines: []int{2, 7}},
		"json-then-yaml.json":                {counts: map[string]int{deploy: 1, cronOld: 1}, lines: []int{1, 3}},
		"json-error-after-two.json":          {counts: map[string]int{deploy: 1, cm: 1}, named: []string{ingOld}},
		"json-in-yaml-mode.yaml":             {counts: map[string]int{deploy: 1}, named: []string{ingOld}},
		"duplicate-apiversion.yaml":          {counts: map[string]int{ingOld: 1}, lines: []int{3}, warnings: 1},
		"duplicate-apiversion.json":          {counts: map[string]int{ingOld: 1}, warnings: 1},
		"duplicate-apiversion-reversed.yaml": {counts: map[string]int{ingNew: 1}, lines: []int{3}, warnings: 1},
		"typed-list-untyped-items.yaml":      {counts: map[string]int{ingOld: 2}, lines: []int{4, 6}},
		"items-on-object.yaml":               {counts: map[string]int{ingOld: 1}},
		"crd-list-empty-items.yaml":          {counts: map[string]int{}},
		"merge-key.yaml":                     {counts: map[string]int{ingOld: 1}, lines: []int{4}},
		"merge-key-overridden.yaml":          {counts: map[string]int{ingNew: 1}, lines: []int{2}},
		"yaml-trailing-node.yaml":            {counts: map[string]int{cm: 1}, named: []string{ingOld}},
		"invalid-separator.yaml":             {counts: map[string]int{cm: 1}, named: []string{ingOld}},
		"crlf.yaml":                          {counts: map[string]int{cm: 1, ingOld: 1}, lines: []int{1, 6}},
		"bom.yaml":                           {counts: map[string]int{ingOld: 1}, lines: []int{1}},
		"flow-style.yaml":                    {counts: map[string]int{ingOld: 1}},
		"quoted-keys.yaml":                   {counts: map[string]int{ingOld: 1}},
		"aliased-items.yaml":                 {counts: map[string]int{}, named: []string{ingOld}},
		"unrendered-template.yaml":           {counts: map[string]int{}, named: []string{pdbOld}},
	}
	files, err := filepath.Glob("testdata/adversarial/*")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	if want := slices.Sorted(maps.Keys(cases)); !reflect.DeepEqual(names, want) {
		t.Fatalf("corpus files = %v\nwant one case per file: %v", names, want)
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata/adversarial", name))
			if err != nil {
				t.Fatal(err)
			}
			got := parseResult(t, string(raw))
			if !reflect.DeepEqual(got.counts, tc.counts) {
				t.Errorf("objects = %v, want %v", got.counts, tc.counts)
			}
			if tc.lines != nil && !reflect.DeepEqual(got.lines, tc.lines) {
				t.Errorf("lines = %v, want %v", got.lines, tc.lines)
			}
			for _, n := range tc.named {
				if !got.named[n] {
					t.Errorf("unassessed parts %+v name %v, want %s among them", got.unassessed, got.named, n)
				}
			}
			if tc.named == nil && len(got.unassessed) > 0 {
				t.Errorf("unassessed = %+v, want everything decoded", got.unassessed)
			}
			if len(got.warnings) != tc.warnings {
				t.Errorf("warnings = %+v, want %d", got.warnings, tc.warnings)
			}
			kubectl, _ := kubectlObjects(string(raw))
			for _, g := range kubectl {
				if got.counts[g] == 0 && !got.named[g] {
					t.Errorf("kubectl applies %s, which is neither counted nor named by an unassessed part", g)
				}
			}
		})
	}
}

// Concatenated JSON (#119): every object is decoded, with its own line, the
// way kubectl's YAMLOrJSONDecoder reads a stream that starts with "{".
func TestParseManifestStream_ConcatenatedJSON(t *testing.T) {
	const dep = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"w"}}`
	const ing = `{"apiVersion":"extensions/v1beta1","kind":"Ingress","metadata":{"name":"old"}}`
	for name, tc := range map[string]struct {
		stream string
		lines  []int
	}{
		"NDJSON":          {dep + "\n" + ing + "\n", []int{1, 2}},
		"pretty-printed":  {"{\n  \"apiVersion\": \"apps/v1\",\n  \"kind\": \"Deployment\"\n}\n{\n  \"apiVersion\": \"extensions/v1beta1\",\n  \"kind\": \"Ingress\"\n}\n", []int{2, 6}},
		"adjacent":        {dep + ing, []int{1, 1}},
		"leading spaces":  {"\n\n  " + dep + "\n\n" + ing, []int{3, 5}},
		"then YAML":       {dep + "\n---\napiVersion: extensions/v1beta1\nkind: Ingress\n", []int{1, 3}},
		"second is YAML ": {dep + "\napiVersion: extensions/v1beta1\nkind: Ingress\n", []int{1, 2}},
	} {
		t.Run(name, func(t *testing.T) {
			got := parseResult(t, tc.stream)
			want := map[string]int{"apps/v1/Deployment": 1, "extensions/v1beta1/Ingress": 1}
			if !reflect.DeepEqual(got.counts, want) || !reflect.DeepEqual(got.lines, tc.lines) || len(got.unassessed)+len(got.warnings) > 0 {
				t.Errorf("objects %v at lines %v, problems %+v %+v; want %v at lines %v", got.counts, got.lines, got.unassessed, got.warnings, want, tc.lines)
			}
		})
	}
}

// A node after the first in one YAML document (after a "..." end marker,
// or a second flow mapping) is ignored by kubectl's decoder; it is not
// assessed, so it is reported with its text.
func TestParseManifestStream_YAMLTrailingNode(t *testing.T) {
	got := parseResult(t, "apiVersion: v1\nkind: ConfigMap\n...\napiVersion: extensions/v1beta1\nkind: Ingress\n")
	if !reflect.DeepEqual(got.counts, map[string]int{"/v1/ConfigMap": 1}) {
		t.Errorf("objects = %v, want the first node only", got.counts)
	}
	if len(got.unassessed) != 1 || got.unassessed[0].line != 4 || !got.named["extensions/v1beta1/Ingress"] {
		t.Errorf("unassessed = %+v named %v, want the Ingress at line 4", got.unassessed, got.named)
	}
}

// Duplicate keys are decoded last-wins, as kubectl's YAML-to-JSON decoder
// does, with a warning: the apiVersion kubectl sends is the one judged.
func TestAppendObjects_DuplicateAPIVersion(t *testing.T) {
	for name, tc := range map[string]struct {
		doc  string
		want string
	}{
		"removed last":      {"apiVersion: networking.k8s.io/v1\nkind: Ingress\napiVersion: extensions/v1beta1\n", "extensions/v1beta1/Ingress"},
		"removed first":     {"apiVersion: extensions/v1beta1\nkind: Ingress\napiVersion: networking.k8s.io/v1\n", "networking.k8s.io/v1/Ingress"},
		"duplicate kind":    {"apiVersion: extensions/v1beta1\nkind: Deployment\nkind: Ingress\n", "extensions/v1beta1/Ingress"},
		"duplicate in JSON": {`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","apiVersion":"extensions/v1beta1"}`, "extensions/v1beta1/Ingress"},
	} {
		t.Run(name, func(t *testing.T) {
			got := parseResult(t, tc.doc)
			if !reflect.DeepEqual(got.counts, map[string]int{tc.want: 1}) {
				t.Errorf("objects = %v, want %s (last value wins)", got.counts, tc.want)
			}
			if len(got.warnings) != 1 || !strings.Contains(got.warnings[0].err.Error(), "duplicate key") || len(got.unassessed) != 0 {
				t.Errorf("warnings = %+v, unassessed %+v; want one duplicate-key warning", got.warnings, got.unassessed)
			}
		})
	}
}

// A typed *List's items without apiVersion and kind take the List's
// group/version and its kind minus "List", as apimachinery's decodeToList
// sets them.
func TestAppendObjects_TypedListUntypedItems(t *testing.T) {
	got := parseResult(t, `apiVersion: extensions/v1beta1
kind: IngressList
items:
- metadata: {name: a, namespace: shop}
- apiVersion: networking.k8s.io/v1
  kind: Ingress
  metadata: {name: b}
- kind: Ingress
`)
	if want := map[string]int{"extensions/v1beta1/Ingress": 1, "networking.k8s.io/v1/Ingress": 1}; !reflect.DeepEqual(got.counts, want) {
		t.Errorf("objects = %v, want %v (an item with only one of apiVersion/kind is not inferred)", got.counts, want)
	}
	if want := []int{4, 5}; !reflect.DeepEqual(got.lines, want) {
		t.Errorf("lines = %v, want %v (an inferred item's own line)", got.lines, want)
	}
}

// Merge keys are resolved as kubectl's YAML decoder resolves them, through
// aliases, and in linear time however the merges nest.
func TestParseManifestStream_MergeKeys(t *testing.T) {
	got := parseResult(t, `base: &b {apiVersion: extensions/v1beta1}
more: &m {<<: *b, kind: Ingress}
<<: [*m, {kind: Deployment}]
metadata: {name: merged}
`)
	if !reflect.DeepEqual(got.counts, map[string]int{"extensions/v1beta1/Ingress": 1}) {
		t.Errorf("objects = %v, want the merged Ingress (earlier mapping in a merge list wins)", got.counts)
	}

	// Each level merges the previous one twice: naive resolution is 2^40.
	var b strings.Builder
	b.WriteString("l0: &l0 {apiVersion: v1, kind: ConfigMap}\n")
	for i := 1; i <= 40; i++ {
		b.WriteString("l" + strconv.Itoa(i) + ": &l" + strconv.Itoa(i) + " {<<: [*l" + strconv.Itoa(i-1) + ", *l" + strconv.Itoa(i-1) + "]}\n")
	}
	b.WriteString("<<: *l40\n")
	got = parseResult(t, b.String())
	if !reflect.DeepEqual(got.counts, map[string]int{"/v1/ConfigMap": 1}) {
		t.Errorf("objects = %v, want one ConfigMap", got.counts)
	}

	// A merge into itself is a cycle, not a hang.
	parseResult(t, "a: &a {<<: *a, apiVersion: v1}\n<<: *a\nkind: ConfigMap\n")
}
