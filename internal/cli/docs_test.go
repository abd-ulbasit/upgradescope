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

// TestDocsAgentRBAC: the security pages name the ingressclasses list that
// add-on detection reads (#18). That the README, SECURITY.md and the
// security page grant no watch, keep the CRD writes by resourceNames and
// name the ConfigMaps read (#130 RB-01) is deploy/chart's
// TestRBACDocsMatchRole, next to the role it describes.
func TestDocsAgentRBAC(t *testing.T) {
	for _, page := range []string{"SECURITY.md", "docs/operations/security-model-and-rbac.md"} {
		doc := strings.ToLower(readDoc(t, page))
		if !strings.Contains(doc, "ingressclasses") {
			t.Errorf("%s does not mention the IngressClass list that add-on detection reads (#18)", page)
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
// unrecognized images and a live cluster's serverVersion, kubeContext and apiServer, validate against
// api/report.schema.json, and carry no field it does not list (#60).
func TestJSONReportMatchesSchema(t *testing.T) {
	sch, schemaDoc := compileReportSchema(t)

	dir := writeFiles(t, map[string]string{
		"rendered/all.yaml": removedAPIs + "---\napiVersion: policy/v1beta1\nkind: PodDisruptionBudget\nmetadata:\n  name: pdb\n  namespace: shop\n" +
			"---\napiVersion: apps/v1beta9\nkind: Deployment\nmetadata:\n  name: typo\n  namespace: shop\n" +
			"---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n  namespace: shop\nspec:\n  template:\n    spec:\n      containers:\n        - name: api\n          image: registry.example.com/shop/api:1.2.3\n",
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
	liveInv := liveInventory("v1.36.4")
	liveInv.UnrecognizedImages, liveInv.UnrecognizedImagesOmitted = []string{"registry.example.com/shop/api"}, 3
	live, _, err := execScanStderr(t, []string{"--target", "1.37", "--output", "json", "--fail-on", "never"}, evalStub(t, liveInv))
	if err != nil {
		t.Fatalf("live run: %v", err)
	}
	outs = append(outs, live)
	all := strings.Join(outs, "\n")
	for _, want := range []string{`"category": "unknown-api"`, `"serverVersion": "v1.36.4"`, `"baselineState"`, `"suppressed"`, `"notAssessed"`, `"kubeContext": "test-ctx"`, `"apiServer"`, `"unrecognizedImages"`, `"unrecognizedImagesOmitted"`} {
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

// TestDocsGitLabJob: the GitLab CI template, ci/gitlab/upgradescope.gitlab-ci.yml,
// which docs/guides/other-ci.md includes verbatim, runs as GitLab runs it.
// CI has no GitLab runner, so this checks the ways the job broke before:
// the image's entrypoint is cleared (alpine/helm's is `helm`, and the
// Docker executor runs the script through it), no line depends on
// GNU-only flags (the image's sha256sum is busybox) or on a pipeline's
// exit status (GitLab's bash runs `set -eo pipefail`), the archive is
// checked against checksums.txt, and the last line, unpiped, is the gate.
// The reports GitLab reads (#73) are the files the scans write: the
// Code Quality report by the gate itself, the JUnit report by a scan that
// cannot stop the job before it.
func TestDocsGitLabJob(t *testing.T) {
	const template = "ci/gitlab/upgradescope.gitlab-ci.yml"
	page := readDoc(t, "docs/guides/other-ci.md")
	_, section, ok := strings.Cut(page, "## GitLab CI\n")
	if !ok {
		t.Fatal(`docs/guides/other-ci.md: no "## GitLab CI" section`)
	}
	if !strings.Contains(section, "```yaml\n--8<-- \""+template+"\"\n```") {
		t.Errorf("docs/guides/other-ci.md: the GitLab section does not include %s as a yaml block", template)
	}
	var jobs map[string]struct {
		Image struct {
			Name       string   `json:"name"`
			Entrypoint []string `json:"entrypoint"`
		} `json:"image"`
		Variables    map[string]string `json:"variables"`
		BeforeScript []string          `json:"before_script"`
		Script       []string          `json:"script"`
		Artifacts    struct {
			When    string            `json:"when"`
			Paths   []string          `json:"paths"`
			Reports map[string]string `json:"reports"`
		} `json:"artifacts"`
	}
	if err := yaml.UnmarshalStrict([]byte(readDoc(t, template)), &jobs); err != nil {
		t.Fatalf("%s: the job does not parse (image must be a {name, entrypoint} mapping): %v", template, err)
	}
	job, ok := jobs["upgrade-readiness"]
	if !ok || len(job.Script) == 0 {
		t.Fatalf("%s: no upgrade-readiness job with a script: %v", template, jobs)
	}
	if job.Image.Name == "" || !reflect.DeepEqual(job.Image.Entrypoint, []string{""}) {
		t.Errorf("%s: the image is %q with entrypoint %q; clear the entrypoint ([\"\"]) so GitLab can start a shell", template, job.Image.Name, job.Image.Entrypoint)
	}
	if job.Artifacts.When != "always" {
		t.Errorf("%s: artifacts.when is %q; the report is wanted when the gate fails", template, job.Artifacts.When)
	}
	if len(job.BeforeScript) == 0 {
		t.Errorf("%s: no before_script rendering the manifests", template)
	}
	written := map[string]string{} // file → the scan line writing it
	verified := false
	for _, line := range job.Script {
		if strings.Contains(line, "--ignore-missing") {
			t.Errorf("%s: %q uses --ignore-missing, which busybox sha256sum lacks", template, line)
		}
		if strings.Contains(line, "sha256sum") && strings.Contains(line, "|") {
			t.Errorf("%s: %q pipes sha256sum; under pipefail its failures on the other archives fail the job", template, line)
		}
		verified = verified || strings.Contains(line, "sha256sum -c")
		if strings.Contains(line, "upgradescope scan") && strings.Contains(line, "|") && !strings.HasSuffix(line, "|| true") {
			t.Errorf("%s: %q pipes a scan; under pipefail a failed gate stops the job there", template, line)
		}
		if strings.HasPrefix(line, "./upgradescope scan ") {
			_, file, _ := strings.Cut(line, " > ")
			file, _, _ = strings.Cut(file, " ")
			written[file] = line
		}
	}
	if !verified {
		t.Errorf("%s: the archive is not checked against checksums.txt (sha256sum -c)", template)
	}
	last := job.Script[len(job.Script)-1]
	if !strings.HasPrefix(last, "./upgradescope scan ") || strings.Contains(last, "|") {
		t.Errorf("%s: the last script line %q should be the unpiped scan that gates the job", template, last)
	}
	for report, format := range map[string]string{"codequality": "--output gitlab-codequality", "junit": "--output junit"} {
		file := job.Artifacts.Reports[report]
		if line := written[file]; file == "" || !strings.Contains(line, format) {
			t.Errorf("%s: artifacts:reports:%s is %q, which no %s scan writes (scans: %v)", template, report, file, format, written)
		}
		if !slices.Contains(job.Artifacts.Paths, file) {
			t.Errorf("%s: %s is not in artifacts:paths, so a failed gate's report cannot be downloaded", template, file)
		}
	}
	if !strings.Contains(last, "--output gitlab-codequality") {
		t.Errorf("%s: the gate %q should write the Code Quality report", template, last)
	}
	if v := job.Variables["UPGRADESCOPE_VERSION"]; !strings.Contains(page, "VERSION="+v+"\n") {
		t.Errorf("%s pins %s; the install section of docs/guides/other-ci.md pins another release", template, v)
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

// TestDocsContractsAreReleaseAssets: every published contract in api/ (the
// JSON report and webhook schemas, the OpenAPI document) is attached to
// each release and listed in its checksums.txt, which cosign signs (#60),
// and the compatibility policy says where to download it.
// hack/release-check.sh proves the checksums on a real snapshot.
func TestDocsContractsAreReleaseAssets(t *testing.T) {
	type extraFile struct {
		Glob string `json:"glob"`
	}
	var cfg struct {
		Checksum struct {
			ExtraFiles []extraFile `json:"extra_files"`
		} `json:"checksum"`
		Release struct {
			ExtraFiles []extraFile `json:"extra_files"`
		} `json:"release"`
	}
	if err := yaml.Unmarshal([]byte(readDoc(t, ".goreleaser.yml")), &cfg); err != nil {
		t.Fatal(err)
	}
	contracts, err := filepath.Glob(filepath.Join(repoRoot, "api", "*"))
	if err != nil || len(contracts) == 0 {
		t.Fatalf("no published contracts under api/ (%v)", err)
	}
	policy := readDoc(t, "docs/compatibility-policy.md")
	for _, c := range contracts {
		if filepath.Ext(c) == ".go" {
			continue // api/embed.go is the Go package that embeds the contracts, not one of them
		}
		rel, _ := filepath.Rel(repoRoot, c)
		rel = filepath.ToSlash(rel)
		for section, files := range map[string][]extraFile{
			"checksum.extra_files (checksums.txt, signed)": cfg.Checksum.ExtraFiles,
			"release.extra_files (uploaded)":               cfg.Release.ExtraFiles,
		} {
			if !slices.ContainsFunc(files, func(f extraFile) bool {
				ok, err := filepath.Match(strings.TrimPrefix(f.Glob, "./"), rel)
				return err == nil && ok
			}) {
				t.Errorf(".goreleaser.yml %s does not include %s", section, rel)
			}
		}
		if want := "releases/latest/download/" + filepath.Base(c); !strings.Contains(policy, want) {
			t.Errorf("docs/compatibility-policy.md does not say where to download %s (want %q)", rel, want)
		}
	}
}

// TestDocsTokenCommandsParse: every `tokens create|list|revoke` command the
// hand-written docs show parses as the CLI defines it, arguments, flags and
// flag groups included (#126: a README once showed `tokens revoke <cluster>`
// without --id or --all, which the CLI refuses). A bare mention of a
// subcommand by name is not a command and is skipped.
func TestDocsTokenCommandsParse(t *testing.T) {
	pages := []string{"README.md", "SECURITY.md", "deploy/chart/README.md"}
	err := filepath.WalkDir(filepath.Join(repoRoot, "docs"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "reference" {
			return filepath.SkipDir // generated from the CLI itself
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return err
			}
			pages = append(pages, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A command runs to the end of its code span or line, before a shell
	// comment, a line continuation or a table cell's end.
	cmdRE := regexp.MustCompile("\\btokens (create|list|revoke)((?: [^`\\n#\\\\|]*)?)")
	placeholder := strings.NewReplacer("<id>", "1", "<old id>", "1")
	seen := 0
	for _, page := range pages {
		for _, m := range cmdRE.FindAllStringSubmatch(readDoc(t, page), -1) {
			args := strings.Fields(placeholder.Replace(m[2]))
			if len(args) == 0 {
				continue
			}
			seen++
			cmd, rest, err := Root().Find(append([]string{"tokens", m[1]}, args...))
			if err == nil {
				err = cmd.ParseFlags(rest)
			}
			if err == nil {
				err = cmd.ValidateArgs(cmd.Flags().Args())
			}
			if err == nil {
				err = cmd.ValidateRequiredFlags()
			}
			if err == nil {
				err = cmd.ValidateFlagGroups()
			}
			if err != nil {
				t.Errorf("%s: %q does not parse: %v", page, strings.TrimSpace(m[0]), err)
			}
		}
	}
	if seen == 0 {
		t.Fatal("found no tokens command in the docs; the pattern is stale")
	}
}
