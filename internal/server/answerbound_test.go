package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// commentSafe is s without the characters that end a YAML comment or that
// YAML refuses in one.
func commentSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == '\u2028' || r == '\u2029' || r == '\u0085' {
			return -1
		}
		return r
	}, s)
}

// yamlQuoted is s as a YAML double-quoted scalar.
func yamlQuoted(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteString(`\` + string(r))
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02X`, r)
		case r == '\u2028':
			b.WriteString(`\L`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// boundStream is a manifest stream that puts name, n times, everywhere a
// stream's strings reach an answer: object names and namespaces of
// deprecated and removed APIs (the first 12 such GVKs of k, 10 objects
// each, half of them annotated), their ignore annotations and Helm
// sources, a CRD's name and its deprecated version, and an image no
// add-on claims.
func boundStream(k kb.KB, name string) string {
	q := yamlQuoted(name)
	var b strings.Builder
	gvks := 0
	for _, e := range k.APILifecycle {
		if e.Deprecated == nil && e.Removed == nil {
			continue
		}
		gv := e.Version
		if e.Group != "" {
			gv = e.Group + "/" + e.Version
		}
		fmt.Fprintf(&b, "---\n# Source: %s/templates/x.yaml\napiVersion: %s\nkind: %sList\nitems:\n", commentSafe(name), gv, e.Kind)
		for i := range 10 {
			if i%2 == 0 {
				fmt.Fprintf(&b, "- metadata: {name: %s, namespace: %s}\n", yamlQuoted(fmt.Sprintf("%d%s", i, name)), q)
				continue
			}
			fmt.Fprintf(&b, "- metadata: {name: %s, namespace: %s, annotations: {upgradescope.dev/ignore: %s, upgradescope.dev/ignore-reason: %s}}\n",
				yamlQuoted(fmt.Sprintf("%d%s", i, name)), q, q, q)
		}
		if gvks++; gvks == 12 {
			break
		}
	}
	fmt.Fprintf(&b, `---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: %s}
spec:
  group: %s
  names: {kind: Thing, plural: things}
  scope: Namespaced
  versions:
  - {name: v1beta1, served: true, storage: false, deprecated: true, deprecationWarning: %s}
  - {name: v1, served: true, storage: true}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: %s, namespace: %s}
spec:
  template:
    spec:
      containers: [{name: c, image: %s}]
`, yamlQuoted("things."+name), q, q, q, q, yamlQuoted("registry.example/"+name+":1"))
	return b.String()
}

// gateAnswerBound must hold for every format, path and context: each
// string class an escape lengthens (control characters, quotes, HTML
// characters, percent-encoded punctuation, non-ASCII, U+2028) in every
// place a stream's or a cluster's strings reach an answer, with and
// without ?path= and ?cluster=, answers within the bound.
func TestGateAnswerBoundHolds(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string]string{
		"letters":   strings.Repeat("abc", 20),
		"punct":     strings.Repeat("-. %#?/", 10),
		"html":      strings.Repeat("<>&'", 15),
		"quote":     strings.Repeat(`"\`, 30),
		"control":   strings.Repeat("\x00\x01\t\n", 15),
		"unicode":   strings.Repeat("é✓"+lineSeparator, 15),
		"realistic": "web-frontend-7d9f",
	}
	paths := []string{"", "deploy/rendered.yaml", strings.Repeat(`"/`, 100) + "a b%.yaml", strings.Repeat("é/", 100) + "x.yaml"}
	for cname, name := range classes {
		s := newTestServer(t, newFakeStore(), func(c *Config) { c.KB = k })
		s.maxGateAnswer = 1 << 40 // measure every answer
		var bound int64
		s.observeGateBound = func(b int64) { bound = b }
		inv := testInventoryWithPSP()
		// A cluster's namespaces are RFC 1123 labels (ingest refuses others);
		// its object names may hold anything but / and %.
		objName := strings.NewReplacer("/", "", "%", "").Replace(name)
		inv.APIUsage[0].Objects = []inventory.ObjectRef{{Namespace: "team-a", Name: objName, Manager: name}, {Name: "x" + objName}}
		rec := httptest.NewRecorder()
		serveIngest(s, rec, pushReqBody(t, inv), false)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: push status %d (%s)", cname, rec.Code, rec.Body)
		}
		stream := boundStream(k, name)
		for _, path := range paths {
			for _, cluster := range []string{"", "prod-eu-1"} {
				for _, format := range []string{"json", "sarif", "junit", "gitlab-codequality"} {
					q := url.Values{"target": {"1.35"}, "fail-on": {"never"}, "format": {format}}
					if path != "" {
						q.Set("path", path)
					}
					if cluster != "" {
						q.Set("cluster", cluster)
					}
					req := httptest.NewRequest(http.MethodPost, "/api/v1/gate?"+q.Encode(), strings.NewReader(stream))
					req.Header.Set("Content-Type", "application/x-yaml")
					rec := httptest.NewRecorder()
					bound = 0
					s.Handler().ServeHTTP(rec, req)
					if rec.Code != http.StatusOK || bound == 0 {
						t.Fatalf("%s, path %.20q, cluster %q, %s: status %d (%.300s)", cname, path, cluster, format, rec.Code, rec.Body)
					}
					if got := int64(rec.Body.Len()); got > bound {
						t.Errorf("%s, path %.20q, cluster %q, %s: answer %d bytes, over its bound %d", cname, path, cluster, format, got, bound)
					} else if cname == "realistic" {
						t.Logf("%s, path %.20q, cluster %q, %s: answer %d bytes, bound %d (%.1fx)", cname, path, cluster, format, got, bound, float64(bound)/float64(got))
					}
				}
			}
		}
	}
}

// An answer that could be larger than --max-gate-bytes is 413 before it
// is encoded, saying to split the stream; the same stream with short
// names is answered.
func TestGateAnswerOverTheLimitIs413(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.MaxGateBytes = 1 << 20 })
	list := func(name string) string {
		var b strings.Builder
		b.WriteString("apiVersion: policy/v1beta1\nkind: PodSecurityPolicyList\nitems:\n")
		for i := range 100 {
			fmt.Fprintf(&b, "- metadata: {name: '%d%s'}\n", i, name)
		}
		return b.String()
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	query := "?target=1.35&fail-on=never&format=sarif&path=deploy/rendered.yaml"
	if resp, raw := postGate(t, ts, query, "", list("-a"), "application/x-yaml"); resp.StatusCode != http.StatusOK {
		t.Fatalf("short names: status %d (%.300s), want 200", resp.StatusCode, raw)
	}
	resp, raw := postGate(t, ts, query, "", list(strings.Repeat("<", 2000)), "application/x-yaml")
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(raw), "split the stream") ||
		resp.Header.Get("X-Upgradescope-Verdict") != "" {
		t.Fatalf("2000-byte names: status %d, verdict %q (%.300s), want 413 saying to split the stream",
			resp.StatusCode, resp.Header.Get("X-Upgradescope-Verdict"), raw)
	}
}

// manyRefsStream names per objects of every deprecated or removed GVK in
// k (the KB's 136 at 100, inventory.MaxObjectRefs, is the most refs an
// answer lists), each named name plus its index.
func manyRefsStream(k kb.KB, per int, name string) string {
	q := yamlQuoted(name)
	var b strings.Builder
	for _, e := range k.APILifecycle {
		if e.Deprecated == nil && e.Removed == nil {
			continue
		}
		gv := e.Version
		if e.Group != "" {
			gv = e.Group + "/" + e.Version
		}
		fmt.Fprintf(&b, "---\napiVersion: %s\nkind: %sList\nitems:\n", gv, e.Kind)
		for i := range per {
			fmt.Fprintf(&b, "- metadata: {name: %s, namespace: n%d}\n", q[:len(q)-1]+fmt.Sprint(i)+`"`, i%10)
		}
	}
	return b.String()
}

// The answers that cost the most to encode are the largest ones within
// the answer limit: as many objects as an answer lists, with names as
// long as fit, in every format, plain and escape-heavy, with a path. Each
// is answered within maxGateDecodeHeap, its answer within the limit.
func TestGateAnswerHeapIsBounded(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("encodes answers of up to 10 MiB; heap figures under the race detector mean nothing")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // as in TestGateDecodeHeapIsBounded
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	const path = "deploy/overlays/production/rendered.yaml"
	for _, format := range []string{"json", "sarif", "junit", "gitlab-codequality"} {
		for _, unit := range []string{"x", `"`} {
			t.Run(format+" "+unit, func(t *testing.T) {
				s := newTestServer(t, newFakeStore(), func(c *Config) { c.KB = k })
				var bound int64
				s.observeGateBound = func(b int64) { bound = b }
				// JSON lists 100 objects of each GVK with names of n bytes;
				// the others, which write a result per object, list n/10
				// objects with 100-byte names.
				shape := func(n int) (per, name int) {
					if format == "json" {
						return 100, n
					}
					return max(n/10, 1), 100
				}
				post := func(n int) *httptest.ResponseRecorder {
					q := url.Values{"target": {"1.35"}, "fail-on": {"never"}, "format": {format}, "path": {path}}
					per, name := shape(n)
					req := httptest.NewRequest(http.MethodPost, "/api/v1/gate?"+q.Encode(),
						strings.NewReader(manyRefsStream(k, per, strings.Repeat(unit, name))))
					req.Header.Set("Content-Type", "application/x-yaml")
					rec := httptest.NewRecorder()
					s.Handler().ServeHTTP(rec, req)
					return rec
				}
				// The most objects, or the longest names, whose answer is
				// within the limit.
				lo, hi := 0, 1000
				if rec := post(lo); rec.Code != http.StatusOK {
					t.Skipf("no names fit: status %d (%.200s)", rec.Code, rec.Body)
				}
				for hi-lo > 4 {
					if mid := (lo + hi) / 2; post(mid).Code == http.StatusOK {
						lo = mid
					} else {
						hi = mid
					}
				}
				var rec *httptest.ResponseRecorder
				grew := heapPeak(func() { rec = post(lo) })
				if rec.Code != http.StatusOK || int64(rec.Body.Len()) > s.gateAnswerLimit() || grew > maxGateDecodeHeap {
					per, name := shape(lo)
					t.Fatalf("%d objects per GVK, %d-byte names: status %d, answer %d bytes (bound %d), heap grew %d MiB; want 200 within %d bytes and %d MiB",
						per, name, rec.Code, rec.Body.Len(), bound, grew>>20, s.gateAnswerLimit(), maxGateDecodeHeap>>20)
				}
				per, name := shape(lo)
				t.Logf("%d objects per GVK, %d-byte names: answer %d bytes (bound %d), heap grew %d MiB", per, name, rec.Body.Len(), bound, grew>>20)
			})
		}
	}
}
