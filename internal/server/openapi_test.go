package server

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"sigs.k8s.io/yaml"
)

// The OpenAPI document (api/openapi.yaml) is the API reference integrators
// build on (#53, #117). These tests keep it from drifting: every route the
// server registers is an operation in it and the other way round, and real
// handler responses validate against the schemas it gives for their status
// and content type. Its response schemas are open, as the compatibility
// policy needs, so unlistedFields is what makes a field added to a
// response without the document fail here.

const openapiPath = "../../api/openapi.yaml"

// openapiDoc is the part of the document the tests read.
type openapiDoc struct {
	Paths map[string]map[string]json.RawMessage `json:"paths"`
}

type openapiOperation struct {
	Responses map[string]struct {
		Ref     string                     `json:"$ref"`
		Content map[string]json.RawMessage `json:"content"`
	} `json:"responses"`
}

func loadOpenAPI(t *testing.T) (openapiDoc, []byte) {
	t.Helper()
	y, err := os.ReadFile(openapiPath)
	if err != nil {
		t.Fatal(err)
	}
	j, err := yaml.YAMLToJSON(y)
	if err != nil {
		t.Fatalf("api/openapi.yaml: %v", err)
	}
	var doc openapiDoc
	if err := json.Unmarshal(j, &doc); err != nil {
		t.Fatal(err)
	}
	return doc, j
}

// registeredRoutes reads the route patterns the package registers on its
// mux (s.mux.HandleFunc / s.mux.Handle with a literal pattern) from the
// source, so a route added anywhere in the package is seen.
func registeredRoutes(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var routes []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
				return true
			}
			if recv, ok := sel.X.(*ast.SelectorExpr); !ok || recv.Sel.Name != "mux" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s: a mux route with a non-literal pattern; this test cannot check it", fset.Position(call.Pos()))
				return true
			}
			p, _ := strconv.Unquote(lit.Value)
			routes = append(routes, p)
			return true
		})
	}
	sort.Strings(routes)
	return routes
}

// TestOpenAPICoversEveryRoute: the document and the mux list the same
// operations, so a new route cannot go undocumented and a removed one
// cannot stay documented.
func TestOpenAPICoversEveryRoute(t *testing.T) {
	doc, _ := loadOpenAPI(t)
	routes := registeredRoutes(t)
	if len(routes) < 17 {
		t.Fatalf("found %d routes in the package source, want at least 17: %v", len(routes), routes)
	}
	registered := map[string]bool{}
	for _, r := range routes {
		method, path, ok := strings.Cut(r, " ")
		if !ok {
			t.Errorf("route %q has no method; document it or register it with one", r)
			continue
		}
		registered[method+" "+path] = true
		if _, ok := doc.Paths[path][strings.ToLower(method)]; !ok {
			t.Errorf("route %q is not in api/openapi.yaml", r)
		}
	}
	for path, item := range doc.Paths {
		for method := range item {
			if method == "parameters" {
				continue
			}
			if op := strings.ToUpper(method) + " " + path; !registered[op] {
				t.Errorf("api/openapi.yaml documents %q, which the server does not register", op)
			}
		}
	}
}

// specValidator compiles response schemas out of the document.
type specValidator struct {
	t    *testing.T
	doc  openapiDoc
	root any // the parsed document, for unlistedFields
	comp *jsonschema.Compiler
}

const openapiURL = "https://upgradescope.dev/api/openapi.json"

func newSpecValidator(t *testing.T) *specValidator {
	t.Helper()
	doc, raw := loadOpenAPI(t)
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource(openapiURL, parsed); err != nil {
		t.Fatal(err)
	}
	return &specValidator{t: t, doc: doc, root: parsed, comp: c}
}

// pointerEscape escapes one JSON pointer token.
func pointerEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// check validates one real response against the document: its status must
// be listed for the operation, its media type under that status, and a
// JSON body must match that media type's schema.
func (v *specValidator) check(name, path, method string, resp *http.Response, body []byte) {
	t := v.t
	t.Helper()
	rawOp, ok := v.doc.Paths[path][method]
	if !ok {
		t.Errorf("%s: %s %s is not in api/openapi.yaml", name, method, path)
		return
	}
	var op openapiOperation
	if err := json.Unmarshal(rawOp, &op); err != nil {
		t.Fatal(err)
	}
	code := strconv.Itoa(resp.StatusCode)
	r, ok := op.Responses[code]
	if !ok {
		t.Errorf("%s: %s %s answered %s, which api/openapi.yaml does not list", name, method, path, code)
		return
	}
	ptr := "#/paths/" + pointerEscape(path) + "/" + method + "/responses/" + code
	content := r.Content
	if r.Ref != "" { // a shared response under components/responses
		ptr = r.Ref
		content = map[string]json.RawMessage{"application/json": nil}
	}
	if len(content) == 0 {
		if len(body) != 0 {
			t.Errorf("%s: %s %s %s has a body, the document says none", name, method, path, code)
		}
		return
	}
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		t.Errorf("%s: Content-Type %q: %v", name, resp.Header.Get("Content-Type"), err)
		return
	}
	if _, ok := content[mt]; !ok {
		t.Errorf("%s: %s %s %s is %s, the document lists %v", name, method, path, code, mt, keys(content))
		return
	}
	if mt != "application/json" {
		return
	}
	sch, err := v.comp.Compile(openapiURL + ptr + "/content/" + pointerEscape(mt) + "/schema")
	if err != nil {
		t.Fatalf("%s: compiling the schema: %v", name, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Errorf("%s: body is not JSON: %v\n%s", name, err, body)
		return
	}
	if err := sch.Validate(inst); err != nil {
		t.Errorf("%s: %s %s %s does not match api/openapi.yaml:\n%v\nbody: %s", name, method, path, code, err, body)
	}
	// The schemas are open (TestOpenAPIResponseSchemasAreOpen), so this is
	// what fails a field the document does not list.
	schema := resolveRef(v.root, ptr+"/content/"+pointerEscape(mt)+"/schema")
	if extra := unlistedFields(v.root, schema, inst, ""); len(extra) > 0 {
		t.Errorf("%s: %s %s %s has fields api/openapi.yaml does not list: %v", name, method, path, code, extra)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestOpenAPIResponsesMatchSpec drives every operation through the real
// handlers, successes and the common errors, and validates each response.
func TestOpenAPIResponsesMatchSpec(t *testing.T) {
	v := newSpecValidator(t)
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) {
		c.ReadToken = "read-tok"
		c.AdminToken = "admin-tok"
		c.ExtraTargets = []string{"1.36"}
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	type call struct {
		name, method, route, url, token, contentType string
		body                                         []byte
		gzip                                         bool
		status                                       int // what the call is there to exercise
	}
	inv := testInventoryWithPSP()
	// The report responses then carry the unrecognized-image fields too.
	inv.UnrecognizedImages, inv.UnrecognizedImagesOmitted = []string{"registry.example.com/shop/api"}, 3
	pushBody := pushReqBody(t, inv)
	calls := []call{
		{name: "push accepted", method: "post", route: "/api/v1/snapshots", url: "/api/v1/snapshots", token: "ingest-tok", contentType: "application/json", body: pushBody, gzip: true, status: 202},
		{name: "push duplicate", method: "post", route: "/api/v1/snapshots", url: "/api/v1/snapshots", token: "ingest-tok", contentType: "application/json", body: pushBody, status: 200},
		{name: "push unauthorized", method: "post", route: "/api/v1/snapshots", url: "/api/v1/snapshots", contentType: "application/json", body: pushBody, status: 401},
		{name: "push invalid", method: "post", route: "/api/v1/snapshots", url: "/api/v1/snapshots", token: "ingest-tok", contentType: "application/json", body: []byte(`{"schemaVersion":2}`), status: 422},
		{name: "healthz", method: "get", route: "/healthz", url: "/healthz", status: 200},
		{name: "readyz", method: "get", route: "/readyz", url: "/readyz", status: 200},
		{name: "metrics", method: "get", route: "/metrics", url: "/metrics", token: "read-tok", status: 200},
		{name: "metrics unauthorized", method: "get", route: "/metrics", url: "/metrics", status: 401},
		{name: "clusters", method: "get", route: "/api/v1/clusters", url: "/api/v1/clusters", token: "read-tok", status: 200},
		{name: "clusters with admin token", method: "get", route: "/api/v1/clusters", url: "/api/v1/clusters", token: "admin-tok", status: 200},
		{name: "cluster", method: "get", route: "/api/v1/clusters/{id}", url: "/api/v1/clusters/1", token: "read-tok", status: 200},
		{name: "cluster not found", method: "get", route: "/api/v1/clusters/{id}", url: "/api/v1/clusters/99", token: "read-tok", status: 404},
		{name: "cluster bad id", method: "get", route: "/api/v1/clusters/{id}", url: "/api/v1/clusters/x", token: "read-tok", status: 400},
		{name: "report", method: "get", route: "/api/v1/clusters/{id}/report", url: "/api/v1/clusters/1/report", token: "read-tok", status: 200},
		{name: "report what-if", method: "get", route: "/api/v1/clusters/{id}/report", url: "/api/v1/clusters/1/report?target=1.40", token: "read-tok", status: 200},
		{name: "report bad target", method: "get", route: "/api/v1/clusters/{id}/report", url: "/api/v1/clusters/1/report?target=two", token: "read-tok", status: 422},
		{name: "findings", method: "get", route: "/api/v1/clusters/{id}/findings", url: "/api/v1/clusters/1/findings?severity=blocker", token: "read-tok", status: 200},
		{name: "history", method: "get", route: "/api/v1/clusters/{id}/history", url: "/api/v1/clusters/1/history?limit=5", token: "read-tok", status: 200},
		{name: "history bad limit", method: "get", route: "/api/v1/clusters/{id}/history", url: "/api/v1/clusters/1/history?limit=0", token: "read-tok", status: 422},
		{name: "teams", method: "get", route: "/api/v1/clusters/{id}/teams", url: "/api/v1/clusters/1/teams", token: "read-tok", status: 200},
		{name: "export csv", method: "get", route: "/api/v1/clusters/{id}/export", url: "/api/v1/clusters/1/export?format=csv", token: "read-tok", status: 200},
		{name: "export html", method: "get", route: "/api/v1/clusters/{id}/export", url: "/api/v1/clusters/1/export?format=html", token: "read-tok", status: 200},
		{name: "export bad format", method: "get", route: "/api/v1/clusters/{id}/export", url: "/api/v1/clusters/1/export?format=pdf", token: "read-tok", status: 422},
		{name: "export not stored", method: "get", route: "/api/v1/clusters/{id}/export", url: "/api/v1/clusters/1/export?format=csv&target=1.40", token: "read-tok", status: 404},
		{name: "fleet", method: "get", route: "/api/v1/fleet", url: "/api/v1/fleet?targets=1.34,1.35,1.40", token: "read-tok", status: 200},
		{name: "fleet default targets", method: "get", route: "/api/v1/fleet", url: "/api/v1/fleet", token: "read-tok", status: 200},
		{name: "fleet bad targets", method: "get", route: "/api/v1/fleet", url: "/api/v1/fleet?targets=x", token: "read-tok", status: 422},
		{name: "fleet teams", method: "get", route: "/api/v1/fleet/teams", url: "/api/v1/fleet/teams?target=1.35", token: "read-tok", status: 200},
		{name: "fleet teams no target", method: "get", route: "/api/v1/fleet/teams", url: "/api/v1/fleet/teams", token: "read-tok", status: 422},
		{name: "registry", method: "get", route: "/api/v1/registry", url: "/api/v1/registry", token: "read-tok", status: 200},
		{name: "registry unauthorized", method: "get", route: "/api/v1/registry", url: "/api/v1/registry", status: 401},
		{name: "gate pass", method: "post", route: "/api/v1/gate", url: "/api/v1/gate?target=1.35", token: "read-tok", contentType: "application/x-yaml", body: []byte(deploymentManifest), status: 200},
		{name: "gate fail in cluster", method: "post", route: "/api/v1/gate", url: "/api/v1/gate?target=1.35&cluster=prod-eu-1", token: "read-tok", contentType: "application/x-yaml", body: []byte(pspManifest), status: 422},
		{name: "gate sarif", method: "post", route: "/api/v1/gate", url: "/api/v1/gate?target=1.35&format=sarif&path=deploy/rendered.yaml", token: "read-tok", contentType: "application/x-yaml", body: []byte(pspManifest), status: 422},
		{name: "gate suppressed", method: "post", route: "/api/v1/gate", url: "/api/v1/gate?target=1.35" + withConfig(pspRule), token: "read-tok", contentType: "application/x-yaml", body: []byte(pspManifest), status: 200},
		{name: "gate expired rule", method: "post", route: "/api/v1/gate", url: "/api/v1/gate?target=1.35" + withConfig(pspRule+"    expires: 2026-01-01\n"), token: "read-tok", contentType: "application/x-yaml", body: []byte(pspManifest), status: 422},
		{name: "gate invalid config", method: "post", route: "/api/v1/gate", url: "/api/v1/gate?target=1.35" + withConfig("ignore: [\n"), token: "read-tok", contentType: "application/x-yaml", body: []byte(pspManifest), status: 422},
		{name: "gate no target", method: "post", route: "/api/v1/gate", url: "/api/v1/gate", token: "read-tok", contentType: "application/x-yaml", body: []byte(deploymentManifest), status: 422},
		{name: "gate unknown cluster", method: "post", route: "/api/v1/gate", url: "/api/v1/gate?target=1.35&cluster=nope", token: "read-tok", contentType: "application/x-yaml", body: []byte(deploymentManifest), status: 404},
		{name: "gate media type", method: "post", route: "/api/v1/gate", url: "/api/v1/gate?target=1.35", token: "read-tok", contentType: "text/plain", body: []byte(deploymentManifest), status: 415},
		{name: "rename read token", method: "patch", route: "/api/v1/clusters/{id}", url: "/api/v1/clusters/1", token: "read-tok", contentType: "application/json", body: []byte(`{"name":"prod-eu-2"}`), status: 403},
		{name: "rename invalid", method: "patch", route: "/api/v1/clusters/{id}", url: "/api/v1/clusters/1", token: "admin-tok", contentType: "application/json", body: []byte(`{"nom":"x"}`), status: 422},
		{name: "rename", method: "patch", route: "/api/v1/clusters/{id}", url: "/api/v1/clusters/1", token: "admin-tok", contentType: "application/json", body: []byte(`{"name":"prod-eu-2"}`), status: 200},
		{name: "delete unauthenticated", method: "delete", route: "/api/v1/clusters/{id}", url: "/api/v1/clusters/1", status: 401},
		{name: "delete", method: "delete", route: "/api/v1/clusters/{id}", url: "/api/v1/clusters/1", token: "admin-tok", status: 204},
		{name: "delete gone", method: "delete", route: "/api/v1/clusters/{id}", url: "/api/v1/clusters/1", token: "admin-tok", status: 404},
	}
	covered := map[string]bool{}
	for _, c := range calls {
		body := c.body
		if c.gzip {
			body = gzipBytes(t, body)
		}
		req, err := http.NewRequest(strings.ToUpper(c.method), ts.URL+c.url, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if c.contentType != "" {
			req.Header.Set("Content-Type", c.contentType)
		}
		if c.gzip {
			req.Header.Set("Content-Encoding", "gzip")
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != c.status {
			t.Errorf("%s: %s %s = %d, want %d: %s", c.name, strings.ToUpper(c.method), c.url, resp.StatusCode, c.status, got)
		}
		v.check(c.name, c.route, c.method, resp, got)
		covered[strings.ToUpper(c.method)+" "+c.route] = true
	}
	// Every documented operation was exercised at least once.
	for _, r := range registeredRoutes(t) {
		if !covered[r] {
			t.Errorf("no call exercises %q; add one so its responses are checked", r)
		}
	}
}

// TestOpenAPIUnknownPathIsJSONError: the error envelope the document
// promises for an unregistered /api path.
func TestOpenAPIUnknownPathIsJSONError(t *testing.T) {
	v := newSpecValidator(t)
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/v1/nope")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /api/v1/nope = %d, want 404", resp.StatusCode)
	}
	sch, err := v.comp.Compile(openapiURL + "#/components/schemas/Error")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("404 body is not JSON: %q", body)
	}
	if err := sch.Validate(inst); err != nil {
		t.Errorf("404 body %s is not the Error schema: %v", body, err)
	}
	if extra := unlistedFields(v.root, resolveRef(v.root, "#/components/schemas/Error"), inst, ""); len(extra) > 0 {
		t.Errorf("404 body %s has fields the Error schema does not list: %v", body, extra)
	}
}

// TestOpenAPIResponseSchemasAreOpen: no schema a response uses closes
// itself with additionalProperties or unevaluatedProperties false. The
// compatibility policy lets a release add response fields within /api/v1,
// and a closed schema would make every client that validates against the
// document, or is generated from it, reject that addition. Undocumented
// fields are caught by unlistedFields in check instead.
func TestOpenAPIResponseSchemasAreOpen(t *testing.T) {
	_, raw := loadOpenAPI(t)
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	var responses []string // JSON pointers to response objects
	paths, _ := root["paths"].(map[string]any)
	for p, item := range paths {
		ops, _ := item.(map[string]any)
		for m, op := range ops {
			o, _ := op.(map[string]any)
			rs, _ := o["responses"].(map[string]any)
			for code := range rs {
				responses = append(responses, "#/paths/"+pointerEscape(p)+"/"+m+"/responses/"+code)
			}
		}
	}
	comps, _ := root["components"].(map[string]any)
	shared, _ := comps["responses"].(map[string]any)
	for name := range shared {
		responses = append(responses, "#/components/responses/"+name)
	}
	if len(responses) < 40 {
		t.Fatalf("found %d responses in api/openapi.yaml; the walk is broken", len(responses))
	}
	seen := map[string]bool{}
	var closed []string
	var walk func(node any, at string)
	walk = func(node any, at string) {
		switch n := node.(type) {
		case map[string]any:
			for _, kw := range []string{"additionalProperties", "unevaluatedProperties"} {
				if n[kw] == false {
					closed = append(closed, at+" ("+kw+": false)")
				}
			}
			if ref, ok := n["$ref"].(string); ok && !seen[ref] {
				seen[ref] = true
				walk(resolveRef(root, ref), ref)
			}
			for k, child := range n {
				walk(child, at+"/"+pointerEscape(k))
			}
		case []any:
			for i, child := range n {
				walk(child, at+"/"+strconv.Itoa(i))
			}
		}
	}
	for _, r := range responses {
		walk(resolveRef(root, r), r)
	}
	if !seen["#/components/schemas/Finding"] {
		t.Fatal("the walk never reached the Finding schema; it is broken")
	}
	sort.Strings(closed)
	for _, c := range closed {
		t.Errorf("a response schema is closed at %s; responses may gain fields within /api/v1, so leave it open (the tests reject undocumented fields)", c)
	}
}
