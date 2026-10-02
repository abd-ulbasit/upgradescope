package cli

// Tests that keep the hand-written docs (README.md, docs/, SECURITY.md)
// true to the code and data they describe (#117, #130). Each one names the
// page to fix when it fails.

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"sigs.k8s.io/yaml"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/suppress"
	"github.com/abd-ulbasit/upgradescope/registry"
)

const repoRoot = "../.."

func readDoc(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestDocsReadinessContract: ready is verdict == ready, not "no blockers"
// (#130 VS-03), and a reader of the README learns how to gate when a
// required check is missing.
func TestDocsReadinessContract(t *testing.T) {
	for _, page := range []string{"README.md", "docs/architecture.md", "docs/concepts/verdict-and-score.md"} {
		doc := readDoc(t, page)
		if strings.Contains(strings.ReplaceAll(doc, " ", ""), "blockers==0") {
			t.Errorf("%s says ready = (blockers == 0); ready is verdict == ready", page)
		}
	}
	readme := readDoc(t, "README.md")
	for _, want := range []string{"--allow-incomplete", "unknown"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.md does not mention %s", want)
		}
	}
}

// TestDocsKBHorizon: the README's and the CI gate page's example targets are
// ones the embedded knowledge base can judge, so copying an example gives a
// verdict rather than "unknown" (#130 VS-11), and the README hard-codes no
// "currently 1.NN" horizon that goes stale with the next release (#23).
func TestDocsKBHorizon(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	target := regexp.MustCompile(`(?:--target[ =]|target: "?|target=)([0-9]+\.[0-9]+)`)
	for _, page := range []string{"README.md", "docs/getting-started/ci-gate.md"} {
		doc := readDoc(t, page)
		found := target.FindAllStringSubmatch(doc, -1)
		if len(found) == 0 {
			t.Errorf("%s has no example target; this test would check nothing", page)
		}
		for _, m := range found {
			v, err := inventory.ParseTarget(m[1])
			if err != nil {
				t.Errorf("%s: example target %q: %v", page, m[1], err)
				continue
			}
			if v.Compare(k.MaxKnownK8s) > 0 {
				t.Errorf("%s: example target %s is past the knowledge base horizon %s, so it reads unknown", page, v, k.MaxKnownK8s)
			}
		}
	}
	if regexp.MustCompile(`(?i)currently \**1\.[0-9]+`).MatchString(readDoc(t, "README.md")) {
		t.Error(`README.md hard-codes the horizon ("currently 1.NN"); point to upgradescope version instead`)
	}
}

// registryStats counts the embedded registry the way the docs describe it.
type registryStats struct {
	ids                          []string
	eolSynced, k8sRanges, noData int
}

func loadRegistryStats(t *testing.T) registryStats {
	t.Helper()
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	var s registryStats
	for _, a := range addons {
		s.ids = append(s.ids, a.ID)
		if a.EndoflifeProduct != "" {
			s.eolSynced++
		}
		ranges := len(a.Compat) > 0
		for _, c := range a.Cycles {
			ranges = ranges || c.K8sMin != "" || c.K8sMax != ""
		}
		if ranges {
			s.k8sRanges++
		}
		if len(a.Cycles) == 0 && len(a.Compat) == 0 && a.Support.Status != "eol" && a.Support.EOLDate == "" {
			s.noData++
		}
	}
	sort.Strings(s.ids)
	return s
}

// TestDocsRegistryCounts: the add-on counts in the README and on the
// registry page match registry/data, and the registry page lists every
// entry (#130 KB-06).
func TestDocsRegistryCounts(t *testing.T) {
	s := loadRegistryStats(t)
	page := readDoc(t, "docs/concepts/addon-registry.md")
	for _, want := range []string{
		fmt.Sprintf("%d add-ons today", len(s.ids)),
		fmt.Sprintf("%d entries carry release-line EOL data", s.eolSynced),
		fmt.Sprintf("%d carry Kubernetes compatibility", s.k8sRanges),
		fmt.Sprintf("for the %d entries with", s.noData),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("docs/concepts/addon-registry.md: want %q (counted from registry/data)", want)
		}
	}
	listed := regexp.MustCompile("(?m)^\\| `([a-z0-9-]+)`").FindAllStringSubmatch(page, -1)
	var ids []string
	for _, m := range listed {
		ids = append(ids, m[1])
	}
	sort.Strings(ids)
	if !slices.Equal(ids, s.ids) {
		t.Errorf("docs/concepts/addon-registry.md lists %v, registry/data has %v", ids, s.ids)
	}
	if want := fmt.Sprintf("%d add-ons", len(s.ids)); !strings.Contains(readDoc(t, "README.md"), want) {
		t.Errorf("README.md: want %q (counted from registry/data)", want)
	}
}

// TestDocsAgentRBAC: what the docs say the agent may do matches the chart's
// role since #16 and #113: get/list only, CRD writes only on its own CRD,
// and the configmaps read that rbac.helmSecrets adds (#130 RB-01).
func TestDocsAgentRBAC(t *testing.T) {
	watch := regexp.MustCompile("`get`/`list`/`watch`|get/list/watch|`watch` on all")
	for _, page := range []string{"README.md", "SECURITY.md", "docs/operations/security-model-and-rbac.md"} {
		doc := readDoc(t, page)
		if watch.MatchString(doc) {
			t.Errorf("%s says the agent watches; it only gets and lists", page)
		}
		if strings.Contains(doc, "not restricted by `resourceNames`") || strings.Contains(doc, "not\n    restricted by `resourceNames`") {
			t.Errorf("%s says the CRD grant is not restricted by resourceNames; it is", page)
		}
	}
	for _, page := range []string{"SECURITY.md", "docs/operations/security-model-and-rbac.md"} {
		if !strings.Contains(strings.ToLower(readDoc(t, page)), "configmaps") {
			t.Errorf("%s does not mention the ConfigMaps read that rbac.helmSecrets grants", page)
		}
	}
}

// TestDocsConfigReference: docs/reference/config.md lists every field the
// config loader accepts and exactly the categories a rule can name (each
// one it lists loads, and each category the engine defines is listed and
// loads), and its example loads.
func TestDocsConfigReference(t *testing.T) {
	page := readDoc(t, "docs/reference/config.md")
	for _, typ := range []reflect.Type{reflect.TypeFor[suppress.Config](), reflect.TypeFor[suppress.Rule]()} {
		for f := range typ.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name != "" && !strings.Contains(page, "| `"+name+"` |") {
				t.Errorf("docs/reference/config.md has no row for the %s field %q", typ.Name(), name)
			}
		}
	}

	_, block, _ := strings.Cut(page, "```yaml\n")
	block, _, _ = strings.Cut(block, "```")
	dir := t.TempDir()
	write := func(content string) string {
		p := filepath.Join(dir, ".upgradescope.yaml")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := suppress.LoadConfig(write(block)); err != nil {
		t.Errorf("the example in docs/reference/config.md does not load: %v", err)
	}

	_, cats, _ := strings.Cut(page, "## Categories\n")
	cats, _, _ = strings.Cut(cats, "What each means")
	listed := regexp.MustCompile("`([a-z-]+)`").FindAllStringSubmatch(cats, -1)
	if len(listed) == 0 {
		t.Fatal("docs/reference/config.md lists no categories")
	}
	for _, m := range listed {
		if _, err := suppress.LoadConfig(write("ignore:\n  - category: " + m[1] + "\n    reason: x\n")); err != nil {
			t.Errorf("docs/reference/config.md lists category %q, which the loader rejects: %v", m[1], err)
		}
	}
	if _, err := suppress.LoadConfig(write("ignore:\n  - category: not-a-category\n    reason: x\n")); err == nil {
		t.Fatal("the loader accepted an unknown category; this test cannot tell listed categories apart")
	}
	// The other direction: the loader's categories are engine categories,
	// so every engine category must be listed, and must load.
	for _, c := range engineCategories(t) {
		if !slices.ContainsFunc(listed, func(m []string) bool { return m[1] == c }) {
			t.Errorf("docs/reference/config.md does not list category %q under Categories", c)
		}
		if _, err := suppress.LoadConfig(write("ignore:\n  - category: " + c + "\n    reason: x\n")); err != nil {
			t.Errorf("the loader rejects engine category %q (add it to suppress's categories): %v", c, err)
		}
	}
}

// engineCategories reads the finding categories the engine defines, its
// constants of type Category, from internal/engine's source, so a new
// category is seen without a second list to keep in step.
func engineCategories(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoRoot, "internal", "engine", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var cats []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				if typ, ok := vs.Type.(*ast.Ident); !ok || typ.Name != "Category" {
					continue
				}
				for _, v := range vs.Values {
					if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						c, _ := strconv.Unquote(lit.Value)
						cats = append(cats, c)
					}
				}
			}
		}
	}
	sort.Strings(cats)
	if !slices.Contains(cats, "removed-api") || !slices.Contains(cats, "unknown-api") {
		t.Fatalf("read categories %v from internal/engine; this test no longer finds them", cats)
	}
	return cats
}

// TestDocsListEveryCategory: every category the engine defines is named in
// the published schemas and on the pages that explain categories, and the
// schemas do not close the list: within schemaVersion 1 a category may be
// added (docs/compatibility-policy.md), so a report with one the schema
// has not seen yet must still validate.
func TestDocsListEveryCategory(t *testing.T) {
	cats := engineCategories(t)
	sch, schemaDoc := compileReportSchema(t)
	defs, _ := schemaDoc.(map[string]any)["$defs"].(map[string]any)
	fields, _ := defs["findingFields"].(map[string]any)
	props, _ := fields["properties"].(map[string]any)
	category, _ := props["category"].(map[string]any)
	y, err := os.ReadFile(filepath.Join(repoRoot, "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var openapi struct {
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := yaml.Unmarshal(y, &openapi); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]map[string]any{
		"api/report.schema.json $defs.findingFields.category": category,
		"api/openapi.yaml components.schemas.Category":        openapi.Components.Schemas["Category"],
	} {
		if s == nil {
			t.Errorf("%s: not found", name)
			continue
		}
		if _, ok := s["enum"]; ok {
			t.Errorf("%s is an enum: a category added in a release would fail consumers that validate with it", name)
		}
		desc, _ := s["description"].(string)
		for _, c := range cats {
			if !strings.Contains(desc, "`"+c+"`") {
				t.Errorf("%s: the description does not list category %q", name, c)
			}
		}
	}
	for _, page := range []string{"docs/concepts/verdict-and-score.md", "docs/architecture.md"} {
		doc := readDoc(t, page)
		for _, c := range cats {
			if !strings.Contains(doc, "| `"+c+"` |") {
				t.Errorf("%s has no table row for category %q", page, c)
			}
		}
	}
	future := `{"schemaVersion":1,"toolVersion":"9.9.9","clusterId":"files","target":"1.99","kbVersion":"x","score":100,"ready":true,"verdict":"ready",` +
		`"findings":[{"category":"a-future-category","severity":"info","key":"a-future-category/x","title":"t","detail":"d"}]}`
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(future))
	if err != nil {
		t.Fatal(err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Errorf("a report with a category added in a later release does not validate against api/report.schema.json: %v", err)
	}
}

// compileReportSchema compiles api/report.schema.json and returns it with
// its parsed document.
func compileReportSchema(t *testing.T) (*jsonschema.Schema, any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, "api", "report.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("report.schema.json", schemaDoc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile("report.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return sch, schemaDoc
}

// TestJSONReportMatchesSchema: real `scan --output json` reports, with
// findings (an unknown-api one included), suppressions, a baseline, gaps,
// and a live cluster's serverVersion, kubeContext and apiServer, validate against
// api/report.schema.json, and carry no field it does not list (#60).
func TestJSONReportMatchesSchema(t *testing.T) {
	sch, schemaDoc := compileReportSchema(t)

	dir := writeFiles(t, map[string]string{
		"rendered/all.yaml": removedAPIs + "---\napiVersion: policy/v1beta1\nkind: PodDisruptionBudget\nmetadata:\n  name: pdb\n  namespace: shop\n" +
			"---\napiVersion: apps/v1beta9\nkind: Deployment\nmetadata:\n  name: typo\n  namespace: shop\n",
		".upgradescope.yaml": "ignore:\n  - key: removed-api/batch/v1beta1/CronJob\n    reason: deleted next sprint\n    expires: 2099-01-01\n",
	})
	files := filepath.Join(dir, "rendered")
	baseline := filepath.Join(dir, "baseline.json")
	runs := [][]string{
		{"--files", files, "--config", filepath.Join(dir, ".upgradescope.yaml"), "--output", "json", "--fail-on", "never", "--write-baseline", baseline},
		{"--files", files, "--output", "json", "--fail-on", "never", "--baseline", baseline},
		{"--files", files, "--output", "json", "--fail-on", "never", "--target", targetAboveKBHorizon(t)},
	}
	page := readDoc(t, "docs/reference/json-report.md")
	_, example, _ := strings.Cut(page, "```json\n")
	example, _, _ = strings.Cut(example, "```")
	if inst, err := jsonschema.UnmarshalJSON(strings.NewReader(example)); err != nil {
		t.Errorf("docs/reference/json-report.md: the example is not JSON: %v", err)
	} else if err := sch.Validate(inst); err != nil {
		t.Errorf("docs/reference/json-report.md: the example does not validate: %v", err)
	} else if extra := unlistedFields(schemaDoc, schemaDoc, inst, ""); len(extra) > 0 {
		t.Errorf("docs/reference/json-report.md: the example has unlisted fields %v", extra)
	}
	outs := make([]string, 0, len(runs)+1)
	for i, args := range runs {
		out, _, err := execScanFiles(t, args...)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		outs = append(outs, out)
	}
	// A real live scan (before any stub replaces runScan), against a fake
	// API server that answers only /metrics, names the cluster it read
	// (kubeContext, apiServer).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			w.Header().Set("Content-Type", "text/plain")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	named, err := execRealScan(t, []string{"--target", "1.37", "--kubeconfig", writeServerKubeconfig(t, srv.URL), "--output", "json", "--fail-on", "never"})
	if err != nil {
		t.Fatalf("live run against a fake API server: %v", err)
	}
	outs = append(outs, named)
	live, _, err := execScanStderr(t, []string{"--target", "1.37", "--output", "json", "--fail-on", "never"}, evalStub(t, liveInventory("v1.36.4")))
	if err != nil {
		t.Fatalf("live run: %v", err)
	}
	outs = append(outs, live)
	all := strings.Join(outs, "\n")
	for _, want := range []string{`"category": "unknown-api"`, `"serverVersion": "v1.36.4"`, `"baselineState"`, `"suppressed"`, `"notAssessed"`, `"kubeContext": "test-ctx"`, `"apiServer"`} {
		if !strings.Contains(all, want) {
			t.Errorf("no run produced %s; this test no longer covers it", want)
		}
	}
	for i, out := range outs {
		inst, err := jsonschema.UnmarshalJSON(strings.NewReader(out))
		if err != nil {
			t.Fatalf("run %d: not JSON: %v", i, err)
		}
		if err := sch.Validate(inst); err != nil {
			t.Errorf("run %d does not validate against api/report.schema.json: %v\n%s", i, err, out)
		}
		if extra := unlistedFields(schemaDoc, schemaDoc, inst, ""); len(extra) > 0 {
			t.Errorf("run %d has fields api/report.schema.json does not list: %v", i, extra)
		}
	}
}

// unlistedFields returns the paths of object fields in v that schema does
// not list under properties, following local $refs and allOf.
func unlistedFields(root, schema, v any, path string) []string {
	props, items, extra := schemaParts(root, schema)
	var out []string
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			if p, ok := props[k]; ok {
				out = append(out, unlistedFields(root, p, child, path+"/"+k)...)
			} else if extra != nil {
				out = append(out, unlistedFields(root, extra, child, path+"/"+k)...)
			} else {
				out = append(out, path+"/"+k)
			}
		}
	case []any:
		for i, child := range v {
			out = append(out, unlistedFields(root, items, child, fmt.Sprintf("%s/%d", path, i))...)
		}
	}
	sort.Strings(out)
	return out
}

// schemaParts resolves a schema's properties, items and object-valued
// additionalProperties, merging allOf parts and following "#/$defs/" refs.
func schemaParts(root, schema any) (props map[string]any, items, extra any) {
	s, _ := schema.(map[string]any)
	props = map[string]any{}
	if ref, ok := s["$ref"].(string); ok {
		defs, _ := root.(map[string]any)["$defs"].(map[string]any)
		p, i, e := schemaParts(root, defs[strings.TrimPrefix(ref, "#/$defs/")])
		for k, v := range p {
			props[k] = v
		}
		items, extra = i, e
	}
	if all, ok := s["allOf"].([]any); ok {
		for _, part := range all {
			p, i, e := schemaParts(root, part)
			for k, v := range p {
				props[k] = v
			}
			if i != nil {
				items = i
			}
			if e != nil {
				extra = e
			}
		}
	}
	if p, ok := s["properties"].(map[string]any); ok {
		for k, v := range p {
			props[k] = v
		}
	}
	if i, ok := s["items"]; ok {
		items = i
	}
	if e, ok := s["additionalProperties"].(map[string]any); ok {
		extra = e
	}
	return props, items, extra
}

// TestDocsGitLabJob: the GitLab CI example in docs/guides/other-ci.md runs
// as GitLab runs it. CI has no GitLab runner, so this checks the ways the
// job broke before: the image's entrypoint is cleared (alpine/helm's is
// `helm`, and the Docker executor runs the script through it), no line
// depends on GNU-only flags (the image's sha256sum is busybox) or on a
// pipeline's exit status (GitLab's bash runs `set -eo pipefail`), and the
// last line, unpiped, is the gate.
func TestDocsGitLabJob(t *testing.T) {
	page := readDoc(t, "docs/guides/other-ci.md")
	_, section, ok := strings.Cut(page, "## GitLab CI\n")
	if !ok {
		t.Fatal(`docs/guides/other-ci.md: no "## GitLab CI" section`)
	}
	_, block, ok := strings.Cut(section, "```yaml\n")
	if !ok {
		t.Fatal("docs/guides/other-ci.md: the GitLab section has no yaml block")
	}
	block, _, _ = strings.Cut(block, "```")
	var jobs map[string]struct {
		Image struct {
			Name       string   `json:"name"`
			Entrypoint []string `json:"entrypoint"`
		} `json:"image"`
		Script    []string `json:"script"`
		Artifacts struct {
			When string `json:"when"`
		} `json:"artifacts"`
	}
	if err := yaml.Unmarshal([]byte(block), &jobs); err != nil {
		t.Fatalf("docs/guides/other-ci.md: the GitLab job does not parse (image must be a {name, entrypoint} mapping): %v", err)
	}
	job, ok := jobs["upgrade-readiness"]
	if !ok || len(job.Script) == 0 {
		t.Fatalf("docs/guides/other-ci.md: no upgrade-readiness job with a script: %v", jobs)
	}
	if job.Image.Name == "" || !reflect.DeepEqual(job.Image.Entrypoint, []string{""}) {
		t.Errorf("docs/guides/other-ci.md: the image is %q with entrypoint %q; clear the entrypoint ([\"\"]) so GitLab can start a shell", job.Image.Name, job.Image.Entrypoint)
	}
	if job.Artifacts.When != "always" {
		t.Errorf("docs/guides/other-ci.md: artifacts.when is %q; the report is wanted when the gate fails", job.Artifacts.When)
	}
	for _, line := range job.Script {
		if strings.Contains(line, "--ignore-missing") {
			t.Errorf("docs/guides/other-ci.md: %q uses --ignore-missing, which busybox sha256sum lacks", line)
		}
		if strings.Contains(line, "sha256sum") && strings.Contains(line, "|") {
			t.Errorf("docs/guides/other-ci.md: %q pipes sha256sum; under pipefail its failures on the other archives fail the job", line)
		}
		if strings.Contains(line, "upgradescope scan") && strings.Contains(line, "|") && !strings.HasSuffix(line, "|| true") {
			t.Errorf("docs/guides/other-ci.md: %q pipes a scan; under pipefail a failed gate stops the job there", line)
		}
	}
	last := job.Script[len(job.Script)-1]
	if !strings.HasPrefix(last, "./upgradescope scan ") || strings.Contains(last, "|") {
		t.Errorf("docs/guides/other-ci.md: the last script line %q should be the unpiped scan that gates the job", last)
	}
}

// TestDocsHelmCapabilities: the page that tells readers to render with
// `helm template` says what that renders (#119 FS-05): Helm's built-in
// Capabilities, not the cluster's or the target's, so a chart that picks
// API versions from them can render something other than what is
// deployed. It names the flags that pin them and where the installed
// manifests are.
func TestDocsHelmCapabilities(t *testing.T) {
	page := readDoc(t, "docs/getting-started/cli.md")
	for _, want := range []string{"helm template", ".Capabilities", "--kube-version", "--api-versions", "helm get manifest"} {
		if !strings.Contains(page, want) {
			t.Errorf("docs/getting-started/cli.md does not mention %s", want)
		}
	}
}
