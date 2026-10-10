package collect

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// These tests hold a manifest document parsed once (#285: the walk's tree
// is reused for kubectl's decoder) to the same document parsed twice, as it
// was before: the reference is the same code run with reparseDocuments set
// (see reparsing), which runs kubectl's decoder on the text of every
// document. Whatever the findings are, they must not change.

// reparseGuard is held while reparsing has reparseDocuments flipped.
var reparseGuard sync.Mutex

// reparsing runs f with reparseDocuments set to reparse, then restores it.
// reparseDocuments is a package global, so two tests that flip it at once
// would race and read each other's setting: a second reparsing that finds
// one running panics instead (see TestParseOnce_NothingRunsInParallel,
// which fails on any parallel test of the package, one that only reads the
// variable through the code under test included).
func reparsing[T any](reparse bool, f func() T) T {
	if !reparseGuard.TryLock() {
		panic("reparsing called while another is running: reparseDocuments is a package global, so tests that flip it cannot run in parallel")
	}
	defer reparseGuard.Unlock()
	old := reparseDocuments
	reparseDocuments = reparse
	defer func() { reparseDocuments = old }()
	return f()
}

// A test of this package that runs in parallel would read reparseDocuments
// while another flips it. The package has none: this fails if one is added,
// in any of its test files, before it can flake.
func TestParseOnce_NothingRunsInParallel(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files found (%v)", err)
	}
	needle := "t.Para" + "llel()" // spelled so that this file does not hold it
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), needle) {
			t.Errorf("%s runs a test in parallel; reparseDocuments (see reparsing) is shared by this package's tests", f)
		}
	}
}

// reparsing cannot be entered twice at once.
func TestParseOnce_ReparsingRefusesToNest(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a nested reparsing did not panic")
		}
		if reparseDocuments {
			t.Error("reparseDocuments was left set")
		}
	}()
	reparsing(true, func() struct{} {
		reparsing(false, func() struct{} { return struct{}{} })
		return struct{}{}
	})
}

// manifestAPIsOf is manifestAPIs, parsed once or (reparse) twice.
func manifestAPIsOf(manifest string, flagged map[gvk]bool, reparse bool) ([]inventory.APIUsage, error) {
	type result struct {
		rows []inventory.APIUsage
		err  error
	}
	r := reparsing(reparse, func() result {
		rows, err := manifestAPIs(manifest, flagged)
		return result{rows, err}
	})
	return r.rows, r.err
}

// decodeHelmEntryOf is decodeHelmEntry, parsed once or (reparse) twice.
func decodeHelmEntryOf(data []byte, flagged map[gvk]bool, reparse bool) helmCacheEntry {
	return reparsing(reparse, func() helmCacheEntry { return decodeHelmEntry(data, flagged) })
}

// streamSnapshot is everything parseManifestStream returns, with its errors
// as text so two runs compare.
type streamSnapshot struct {
	Objs []manifestObject
	Ev   addOnEvidence
	Bad  []badSnapshot
}

type badSnapshot struct {
	Line       int
	Err        string
	Unassessed string
	Named      []gvk
}

func snapshotStream(t testing.TB, text string, reparse bool) streamSnapshot {
	t.Helper()
	type result struct {
		objs []manifestObject
		ev   addOnEvidence
		bad  []docError
		err  error
	}
	r := reparsing(reparse, func() result {
		objs, ev, bad, err := parseManifestStream(strings.NewReader(text))
		return result{objs, ev, bad, err}
	})
	objs, ev, bad, err := r.objs, r.ev, r.bad, r.err
	if err != nil {
		t.Fatal(err)
	}
	s := streamSnapshot{Objs: objs, Ev: ev}
	for _, b := range bad {
		s.Bad = append(s.Bad, badSnapshot{b.line, b.err.Error(), string(b.unassessed), b.named})
	}
	return s
}

func requireSameStream(t testing.TB, name, text string) {
	t.Helper()
	once, twice := snapshotStream(t, text, false), snapshotStream(t, text, true)
	if !reflect.DeepEqual(once, twice) {
		t.Fatalf("%s: parsed once and parsed twice differ\n once  %+v\n twice %+v\nfor:\n%.2000s", name, once, twice, text)
	}
}

// corpusTexts are the manifest files of the repository's tests, the
// adversarial ones among them.
func corpusTexts(t testing.TB) map[string]string {
	t.Helper()
	texts := map[string]string{}
	for _, dir := range []string{"testdata/adversarial", "testdata/files", "testdata/crds"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(path)
			texts[path] = string(b)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return texts
}

// helmDocs are documents as Helm renders and stores them, and the ways a
// stored manifest can go wrong, as pieces for manifests below.
var helmDocs = []string{
	"# Source: app/templates/deploy.yaml\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: shop\n  labels:\n    app.kubernetes.io/name: web\n    app.kubernetes.io/version: \"1.2.3\"\n    helm.sh/chart: app-0.1.0\nspec:\n  replicas: 3\n  selector:\n    matchLabels:\n      app: web\n  template:\n    metadata:\n      labels:\n        app: web\n    spec:\n      containers:\n      - name: web\n        image: \"registry.k8s.io/ingress-nginx/controller:v1.9.0\"\n        args: [\"--port=8080\", \"--v=2\"]\n        env:\n        - name: DEBUG\n          value: \"yes\"\n        - name: ON\n          value: on\n        - name: PORT\n          value: \"8080\"\n        resources:\n          limits: {cpu: 500m, memory: 128Mi}\n          requests: {cpu: 0.5, memory: 1.5Gi}\n",
	"# Source: app/templates/ingress.yaml\napiVersion: extensions/v1beta1\nkind: Ingress\nmetadata:\n  name: old\n  namespace: shop\n  annotations:\n    kubernetes.io/ingress.class: nginx\nspec:\n  rules:\n  - host: shop.example.com\n    http:\n      paths:\n      - path: /\n        backend: {serviceName: web, servicePort: 80}\n",
	"apiVersion: networking.k8s.io/v1beta1\nkind: Ingress\nmetadata:\n  name: older\n  annotations:\n    upgradescope.basit.engineer/ignore: \"true\"\n    upgradescope.basit.engineer/ignore-reason: planned\nspec:\n  rules: []\n",
	"apiVersion: policy/v1beta1\nkind: PodSecurityPolicy\nmetadata: {name: restricted}\nspec:\n  privileged: false\n  seLinux: {rule: RunAsAny}\n  runAsUser: {rule: MustRunAsNonRoot}\n",
	"apiVersion: batch/v1beta1\nkind: CronJob\nmetadata: {name: nightly, namespace: shop}\nspec:\n  schedule: \"0 3 * * *\"\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers: [{name: j, image: busybox}]\n          restartPolicy: Never\n",
	"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\ndata:\n  a: |\n    line1\n\n    line2\n  b: >\n    folded\n    text\n  c: 0x1F\n  d: 1_000\n  e: 010\n  f: 2001-12-14\n  g: .inf\n  h: no\n  i: ~\n  j:\n  k: 1e3\n  l: 1.2.3\n  m: ! 12\n",
	"apiVersion: v1\nkind: List\nitems:\n- apiVersion: extensions/v1beta1\n  kind: Ingress\n  metadata: {name: in-list}\n- apiVersion: v1\n  kind: ConfigMap\n  metadata: {name: also}\n",
	"apiVersion: extensions/v1beta1\nkind: IngressList\nitems:\n- metadata: {name: untyped}\n",
	"apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.example.com\n  annotations:\n    controller-gen.kubebuilder.io/version: v0.14.0\nspec:\n  group: example.com\n  names: {kind: Widget, listKind: WidgetList, plural: widgets, singular: widget}\n  scope: Namespaced\n  versions:\n  - name: v1\n    served: true\n    storage: true\n    schema:\n      openAPIV3Schema:\n        type: object\n        properties:\n          spec:\n            type: object\n            properties:\n              size: {type: integer, default: 3, minimum: 0, description: \"How many; 0 means none. Default: yes.\"}\n              mode: {type: string, enum: [on, off, auto]}\n",
	"apiVersion: v1\nkind: ConfigMap\nmetadata: &m {name: anchored}\ndata: {k: v}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: *m\n",
	"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: dup\n  name: dup2\ndata: {}\n",
	"base: &b\n  apiVersion: v1\n  kind: ConfigMap\nmerged:\n  <<: *b\n  metadata: {name: m}\n",
	"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: !!str 12\n",
	"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: tagged}\ndata:\n  b: !!binary aGVsbG8=\n  c: !custom x\n",
	"{\"apiVersion\": \"extensions/v1beta1\", \"kind\": \"Ingress\", \"metadata\": {\"name\": \"json-doc\"}}\n",
	"# only a comment\n",
	"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Values.name }}\n",
	"apiVersion: v1\nkind: Secret\nmetadata: {name: s}\ndata:\n  tls.crt: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0t\nstringData:\n  note: \"it's <b>bold</b> & \\u00e9\"\n",
	"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: 'quoted ''name'''}\ndata:\n  \"key with spaces\": \"v\"\n  'k2': 'v2'\n  ? complex\n  : value\n",
	"apiVersion: v1\nkind: ConfigMap\ndata: [not, a, mapping]\n",
	"- apiVersion: v1\n  kind: ConfigMap\n  metadata: {name: in-sequence}\n",
	"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: after-end}\n...\nsecond: node\n",
	"apiVersion: v1\nkind: ConfigMap\nmetadata:\n\tname: tab\n",
	"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: bad\n",
	"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: no-final-newline}",
	"apiVersion: v1\r\nkind: ConfigMap\r\nmetadata: {name: crlf}\r\n",
}

// manifestOf joins documents the way Helm stores them: each preceded by a
// "---" separator.
func manifestOf(docs ...string) string {
	var b strings.Builder
	for _, d := range docs {
		b.WriteString("---\n")
		b.WriteString(d)
		if !strings.HasSuffix(d, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// flaggedAll flags every API a stream holds, with the real KB's flagged
// ones: every object the stream finds is then counted by manifestAPIs.
func flaggedAll(t testing.TB, texts ...string) map[gvk]bool {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	flagged := map[gvk]bool{}
	for _, e := range k.APILifecycle {
		if e.Deprecated != nil || e.Removed != nil {
			flagged[gvk{e.Group, e.Version, e.Kind}] = true
		}
	}
	for _, text := range texts {
		for _, o := range snapshotStream(t, text, true).Objs {
			flagged[gvk{o.group, o.version, o.kind}] = true
		}
	}
	return flagged
}

func requireSameAPIs(t testing.TB, name, manifest string, flagged map[gvk]bool) {
	t.Helper()
	onceRows, onceErr := manifestAPIsOf(manifest, flagged, false)
	twiceRows, twiceErr := manifestAPIsOf(manifest, flagged, true)
	if !reflect.DeepEqual(onceRows, twiceRows) || fmt.Sprint(onceErr) != fmt.Sprint(twiceErr) {
		t.Fatalf("%s: the findings differ between one parse and two\n once  %+v (%v)\n twice %+v (%v)\nfor:\n%.2000s", name, onceRows, onceErr, twiceRows, twiceErr, manifest)
	}
}

func TestParseOnce_CorpusStreamsMatchTwoParses(t *testing.T) {
	for name, text := range corpusTexts(t) {
		requireSameStream(t, name, text)
		requireSameAPIs(t, name, text, flaggedAll(t, text))
	}
}

// TestParseOnce_UnicodeLineBreaksMatchTwoParses: go-yaml reads NEL, LS and
// PS as line breaks, so a bare "!" after one starts a node that v3 types and
// v2 keeps a string. A converter that missed them gave other JSON, and
// problems the second parse did not (review of #285).
func TestParseOnce_UnicodeLineBreaksMatchTwoParses(t *testing.T) {
	for _, br := range []string{"\u0085", "\u2028", "\u2029"} {
		for i, text := range []string{
			"k:" + br + "  ! 12\n",
			"apiVersion: v1\nkind:" + br + "  ! 12\nmetadata: {name: a}\n",
			"apiVersion:" + br + "  ! 1\nkind: X\nmetadata: {name: a}\n",
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name:" + br + "    ! 0x1F\n",
			"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\ndata:\n  k: a" + br + "b\n  l: |\n    x" + br + "y\n",
		} {
			name := fmt.Sprintf("break %U case %d", []rune(br)[0], i)
			requireSameStream(t, name, text)
			requireSameStream(t, name+" in a manifest", manifestOf(helmDocs[0], text, helmDocs[1]))
		}
	}
}

func TestParseOnce_EveryHelmDocumentMatchesTwoParses(t *testing.T) {
	for i, d := range helmDocs {
		name := fmt.Sprintf("document %d", i)
		requireSameStream(t, name, d)
		m := manifestOf(d)
		requireSameStream(t, name+" in a manifest", m)
		requireSameAPIs(t, name, m, flaggedAll(t, m))
	}
	all := manifestOf(helmDocs...)
	requireSameStream(t, "all documents", all)
	requireSameAPIs(t, "all documents", all, flaggedAll(t, all))
}

// A table of releases: multi-document manifests as Helm stores them, whose
// findings are the same parsed once or twice, and are what the release
// holds.
func TestParseOnce_ReleasesHaveTheSameFindings(t *testing.T) {
	flagged := flaggedAll(t, manifestOf(helmDocs...))
	for _, tc := range []struct {
		name     string
		manifest string
		want     map[string]int // "group/version/Kind" counts
	}{
		{"nothing flagged", manifestOf(helmDocs[0], helmDocs[5]), map[string]int{"apps/v1/Deployment": 1, "/v1/ConfigMap": 1}},
		{"ingress and a list", manifestOf(helmDocs[1], helmDocs[6], helmDocs[3]), map[string]int{"extensions/v1beta1/Ingress": 2, "policy/v1beta1/PodSecurityPolicy": 1, "/v1/ConfigMap": 1}},
		{"a CRD beside old APIs", manifestOf(helmDocs[8], helmDocs[2], helmDocs[4]), map[string]int{"apiextensions.k8s.io/v1/CustomResourceDefinition": 1, "networking.k8s.io/v1beta1/Ingress": 1, "batch/v1beta1/CronJob": 1}},
		{"documents the walk and kubectl read differently", manifestOf(helmDocs[9], helmDocs[10], helmDocs[11], helmDocs[12], helmDocs[13]), nil},
		{"json, comments, templates", manifestOf(helmDocs[14], helmDocs[15], helmDocs[16], helmDocs[1]), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireSameAPIs(t, tc.name, tc.manifest, flagged)
			rows, _ := manifestAPIsOf(tc.manifest, flagged, false)
			if tc.want == nil {
				return
			}
			got := map[string]int{}
			for _, r := range rows {
				got[r.Group+"/"+r.Version+"/"+r.Kind] = r.Count
			}
			for k, n := range tc.want {
				if got[k] != n {
					t.Errorf("%s: %d objects, want %d (rows %+v)", k, got[k], n, rows)
				}
			}
		})
	}
}

// Random manifests: documents drawn from the corpus and the pieces above,
// cut into several runs by padding.
func TestParseOnce_RandomManifestsMatchTwoParses(t *testing.T) {
	corpus := corpusTexts(t)
	pieces := slices.Clone(helmDocs)
	for _, name := range slices.Sorted(maps.Keys(corpus)) {
		pieces = append(pieces, corpus[name])
	}
	pad := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: pad}\ndata:\n  blob: |\n" + strings.Repeat("    "+strings.Repeat("x", 76)+"\n", 6000) // about 0.5 MiB
	rng := rand.New(rand.NewPCG(285, 2))
	rounds := 300
	switch {
	case raceEnabled:
		// The race detector makes the rounds slow (60 of them took 257 s,
		// the slowest test of the package, against CI's 25-minute limit
		// for `go test -race`); the 300 rounds without it hold the
		// equivalence, and these run the same code for races.
		rounds = 10
	case testing.Short():
		rounds = 60
	}
	for i := range rounds {
		var docs []string
		for range 1 + rng.IntN(10) {
			switch n := rng.IntN(12); {
			case n == 0:
				docs = append(docs, pad)
			default:
				docs = append(docs, pieces[rng.IntN(len(pieces))])
			}
		}
		m := manifestOf(docs...)
		name := fmt.Sprintf("random manifest %d", i)
		requireSameAPIs(t, name, m, flaggedAll(t, m))
	}
}

// Documents past the bounds are reported, not parsed, and the rest are read
// the same.
func TestParseOnce_BoundsAreUnchanged(t *testing.T) {
	flagged := flaggedAll(t, helmDocs[1])
	big := "apiVersion: v1\nkind: ConfigMap\ndata:\n  blob: |\n" + strings.Repeat("    "+strings.Repeat("y", 76)+"\n", 28000) // over 2 MiB
	nodes := "apiVersion: v1\nkind: ConfigMap\ndata:\n" + strings.Repeat(" k: v\n", maxManifestNodes)
	for name, m := range map[string]string{
		"over the size":  manifestOf(helmDocs[1], big, helmDocs[1]),
		"over the nodes": manifestOf(helmDocs[1], nodes, helmDocs[1]),
	} {
		requireSameAPIs(t, name, m, flagged)
		rows, err := manifestAPIsOf(m, flagged, false)
		if err == nil || len(rows) != 1 || rows[0].Count != 2 {
			t.Errorf("%s: rows %+v, err %v; want the two Ingresses and the error", name, rows, err)
		}
	}
}

// answerRate says how many of the documents of the YAML streams the
// converter made JSON of, and how many it left to kubectl's decoder.
func answerRate(streams ...string) (answered, declined int) {
	for _, text := range streams {
		p := readStream([]byte(text))
		answered += p.toJSON.answered
		declined += p.toJSON.declined
	}
	return answered, declined
}

// With UPGRADESCOPE_HELM_PAYLOADS (see TestHelmDecodeCostByRelease), every
// release of the file decodes to the same entry parsed once or twice, and
// the test says how many of the manifests' documents the shortcut answered.
func TestParseOnce_PayloadsDecodeToTheSameEntries(t *testing.T) {
	path := os.Getenv("UPGRADESCOPE_HELM_PAYLOADS")
	if path == "" {
		t.Skip("diagnostic: set UPGRADESCOPE_HELM_PAYLOADS to a file of namespace, name and base64 payload lines (see TestHelmDecodeCostByRelease)")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	flagged := map[gvk]bool{}
	for _, e := range k.APILifecycle {
		if e.Deprecated != nil || e.Removed != nil {
			flagged[gvk{e.Group, e.Version, e.Kind}] = true
		}
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	n, withAPIs, answered, declined := 0, 0, 0, 0
	for sc.Scan() {
		p := strings.Split(sc.Text(), "\t")
		if len(p) != 3 {
			t.Fatalf("line %q: want namespace, name and payload separated by tabs", sc.Text())
		}
		b, err := base64.StdEncoding.DecodeString(p[2])
		if err != nil {
			t.Fatalf("%s/%s: %v", p[0], p[1], err)
		}
		once, twice := decodeHelmEntryOf(b, flagged, false), decodeHelmEntryOf(b, flagged, true)
		if !reflect.DeepEqual(once, twice) {
			t.Fatalf("%s/%s: entries differ\n once  %+v\n twice %+v", p[0], p[1], once, twice)
		}
		n++
		if len(once.apis) > 0 {
			withAPIs++
		}
		doc, err := decodeHelmRelease(b)
		if err != nil {
			continue
		}
		splitManifest(doc.Manifest, func(text string, _ int, whole bool) {
			if whole {
				a, d := answerRate(text)
				answered, declined = answered+a, declined+d
			}
		})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d releases decode to the same entries, %d of them with flagged APIs; of %d manifest documents the shortcut answered %d and left %d to kubectl's decoder", n, withAPIs, answered+declined, answered, declined)
}

// parseCPUReps is how many times parseCPU reads a stream.
const parseCPUReps = 3

// parseCPU is the mean CPU one reading of data takes, as a whole stream.
func parseCPU(data []byte, reparse bool) time.Duration {
	before := processCPU()
	reparsing(reparse, func() struct{} {
		for range parseCPUReps {
			readStream(data)
		}
		return struct{}{}
	})
	return (processCPU() - before) / parseCPUReps
}

// With UPGRADESCOPE_PARSE_ONCE_FILES, a list of manifest files (separated
// as a PATH is), each parses to the same objects and problems once or
// twice, and the test says how many of its YAML documents the shortcut
// answered: a way to read the rate on manifests of real charts.
func TestParseOnce_FilesMatchTwoParses(t *testing.T) {
	list := os.Getenv("UPGRADESCOPE_PARSE_ONCE_FILES")
	if list == "" {
		t.Skip("diagnostic: set UPGRADESCOPE_PARSE_ONCE_FILES to manifest files separated like a PATH")
	}
	var answered, declined int
	var onceTotal, twiceTotal time.Duration
	for _, path := range filepath.SplitList(list) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		requireSameStream(t, path, string(b))
		a, d := answerRate(string(b))
		once, twice := parseCPU(b, false), parseCPU(b, true)
		t.Logf("%s: %d KiB, %d documents answered, %d left to kubectl's decoder; CPU to parse it %v once, %v twice", path, len(b)>>10, a, d, once.Round(time.Millisecond), twice.Round(time.Millisecond))
		answered, declined = answered+a, declined+d
		onceTotal, twiceTotal = onceTotal+once, twiceTotal+twice
	}
	t.Logf("in all: %d documents answered, %d left to kubectl's decoder; CPU to parse them %v once, %v twice (the mean of %d runs each)", answered, declined, onceTotal.Round(time.Millisecond), twiceTotal.Round(time.Millisecond), parseCPUReps)
}

// The shortcut is wired in: a stream of the documents Helm renders is
// answered from the walk's tree, none left to kubectl's decoder, and with
// the reference path it is not used. (Without this a refactor could drop
// the saving and every equivalence test above would still pass, as they
// pass for a stream parsed twice.)
func TestParseOnce_ShortcutIsWired(t *testing.T) {
	for _, i := range []int{0, 1, 2, 3, 4, 8} {
		text := helmDocs[i]
		p := readStream([]byte(text))
		if p.toJSON.answered != 1 || p.toJSON.declined != 0 {
			t.Errorf("document %d: answered %d, left %d to kubectl's decoder; want 1 and 0", i, p.toJSON.answered, p.toJSON.declined)
		}
		ref := reparsing(true, func() *streamParser { return readStream([]byte(text)) })
		if ref.toJSON.answered != 0 || ref.toJSON.declined != 0 {
			t.Errorf("document %d: the reference path used the shortcut (answered %d, declined %d)", i, ref.toJSON.answered, ref.toJSON.declined)
		}
	}
	// A whole manifest of them, as a release stores it.
	a, d := answerRate(manifestOf(helmDocs[0], helmDocs[1], helmDocs[2], helmDocs[3], helmDocs[4], helmDocs[8]))
	if a != 6 || d != 0 {
		t.Errorf("a manifest of six rendered documents: answered %d, left %d; want 6 and 0", a, d)
	}
}

// numberDenseStream is a stream of about size bytes whose documents are
// full of distinct plain scalars that start like numbers (decimals, dotted
// versions, hex and underscored integers, exponents), the worst case for a
// converter that has to type each one: the cost #285's review measured as
// 1.5 to 2.6 times the old path when each was put to kubectl's decoder.
func numberDenseStream(size int) []byte {
	var b strings.Builder
	n := 0
	for b.Len() < size {
		b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: dense\ndata:\n")
		for range 300 {
			n++
			switch n % 6 {
			case 0:
				fmt.Fprintf(&b, "  k%d: %d.%d\n", n, n%97, n)
			case 1:
				fmt.Fprintf(&b, "  k%d: %d.%d.%d\n", n, n%9, n%97, n)
			case 2:
				fmt.Fprintf(&b, "  k%d: 0x%X\n", n, n)
			case 3:
				fmt.Fprintf(&b, "  k%d: %d_%03d\n", n, n, n%1000)
			case 4:
				fmt.Fprintf(&b, "  k%d: %de3\n", n, n)
			default:
				fmt.Fprintf(&b, "  k%d: -0.%d\n", n, n)
			}
		}
		b.WriteString("---\n")
	}
	return []byte(b.String())
}

// A stream dense in distinct number-like scalars costs less parsed once than
// twice, and none of it is left to kubectl's decoder. CPU time is noisy on a
// loaded machine, so the bar is generous: the saving measured is about half.
// A CPU ratio can flake beside the other packages' tests, so it runs with
// the timing and heap proofs in hack/test-heap.sh (UPGRADESCOPE_HEAP=1, no
// -race: it reads raceEnabled, which is how the script finds it), not in a
// plain `go test ./...`.
func TestParseOnce_NumberDenseStreamIsNotSlower(t *testing.T) {
	if testing.Short() || !heapRun {
		t.Skip("a CPU ratio, run by hack/test-heap.sh (UPGRADESCOPE_HEAP=1), not beside every other package's tests")
	}
	if raceEnabled || processCPU() == 0 {
		t.Skip("a CPU timing test (the race detector's overhead is not the code's)")
	}
	data := numberDenseStream(256 << 10)
	if a, d := answerRate(string(data)); a == 0 || d != 0 {
		t.Fatalf("the number-dense stream: answered %d, left %d to kubectl's decoder; want all answered", a, d)
	}
	var once, twice time.Duration
	for range 3 { // alternated, so a change in load hits both
		once += parseCPU(data, false)
		twice += parseCPU(data, true)
	}
	t.Logf("number-dense stream, %d KiB: CPU %v parsed once, %v twice (sums of 3 alternating means)", len(data)>>10, once.Round(time.Millisecond), twice.Round(time.Millisecond))
	if once*10 > twice*8 {
		t.Errorf("parsed once took %v, parsed twice %v: not under 0.8 of it", once, twice)
	}
}

// numberDocs builds a manifest of at least size bytes whose documents are
// each doc(n) for a counter n, so that the scalars differ from document to
// document, as they do in real releases.
func numberDocs(size int, doc func(n int) string) string {
	var docs []string
	total := 0
	for n := 0; total < size; n++ {
		d := doc(n)
		docs = append(docs, d)
		total += len(d) + 4
	}
	return manifestOf(docs...)
}

// ingressWith is an old Ingress whose spec holds the scalars that count
// yields, one per line: the documents of the worst-case measurement below.
func ingressWith(n, count int, scalar func(n, i int) string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: extensions/v1beta1\nkind: Ingress\nmetadata:\n  name: dense%d\nspec:\n  extra:\n", n)
	for i := range count {
		fmt.Fprintf(&b, "    k%d: %s\n", i, scalar(n, i))
	}
	return b.String()
}

// With UPGRADESCOPE_PARSE_ONCE_WORST set, the CPU of one 1 MiB manifest
// (through manifestAPIs, as a Helm release's is) parsed once and parsed
// twice, for manifests dense in distinct scalars that start like numbers
// (#285's review): the worst cases of the shortcut, the figures of the
// scale guide. The two are alternated, three runs each.
func TestParseOnce_WorstCaseCost(t *testing.T) {
	if os.Getenv("UPGRADESCOPE_PARSE_ONCE_WORST") == "" {
		t.Skip("diagnostic: set UPGRADESCOPE_PARSE_ONCE_WORST=1 (CPU figures for scale.md)")
	}
	float := func(n, i int) string { return fmt.Sprintf("%d.%d", n%1000+1, i+1) }
	version := func(n, i int) string { return fmt.Sprintf("%d.%d.%d", n%9+1, i+1, n) }
	exotic := func(n, i int) string {
		switch i % 4 {
		case 0:
			return fmt.Sprintf("0x%X", n*1000+i)
		case 1:
			return fmt.Sprintf("%d_%03d", n+1, i)
		case 2:
			return fmt.Sprintf("%de3", n*1000+i)
		}
		return fmt.Sprintf("-0.%d%d", n, i)
	}
	// deployment is a rendered Deployment (helmDocs[0]) with count dotted
	// versions among its annotations.
	deployment := func(count int) func(int) string {
		return func(n int) string {
			var b strings.Builder
			b.WriteString("  annotations:\n")
			for i := range count {
				fmt.Fprintf(&b, "    v%d: %s\n", i, version(n, i))
			}
			doc := strings.Replace(helmDocs[0], "spec:\n  replicas: 3", b.String()+"spec:\n  replicas: 3", 1)
			return strings.Replace(doc, "name: web\n  namespace", fmt.Sprintf("name: web%d\n  namespace", n), 1)
		}
	}
	for _, tc := range []struct {
		name string
		doc  func(n int) string
	}{
		{"300 distinct floats per document", func(n int) string { return ingressWith(n, 300, float) }},
		{"250 distinct floats per document", func(n int) string { return ingressWith(n, 250, float) }},
		{"16 distinct floats per document", func(n int) string { return ingressWith(n, 16, float) }},
		{"300 distinct dotted versions per document", func(n int) string { return ingressWith(n, 300, version) }},
		{"300 hex, underscored, exponent and negative scalars per document", func(n int) string { return ingressWith(n, 300, exotic) }},
		// Declined late: the whole tree is walked, then the last value (.inf,
		// which JSON cannot hold) leaves the document to kubectl's decoder.
		{"300 distinct floats and a final .inf per document (declined)", func(n int) string { return ingressWith(n, 300, float) + "    z: .inf\n" }},
		{"a Deployment and 0 distinct dotted versions", deployment(0)},
		{"a Deployment and 16 distinct dotted versions", deployment(16)},
		{"a Deployment and 64 distinct dotted versions", deployment(64)},
	} {
		manifest := numberDocs(1<<20, tc.doc)
		flagged := flaggedAll(t, manifest)
		requireSameAPIs(t, tc.name, manifest, flagged)
		var once, twice time.Duration
		const reps = 3
		for range reps {
			start := processCPU()
			manifestAPIsOf(manifest, flagged, false)
			once += processCPU() - start
			start = processCPU()
			manifestAPIsOf(manifest, flagged, true)
			twice += processCPU() - start
		}
		a, d := answerRate(manifest)
		t.Logf("%-70s %4d KiB: CPU %4d ms once, %4d ms twice (mean of %d); %d documents answered, %d left", tc.name, len(manifest)>>10, (once / reps).Milliseconds(), (twice / reps).Milliseconds(), reps, a, d)
	}
}

// FuzzParseOnceMatchesTwoParses: for any text, a stream parsed once yields
// the objects, evidence and problems it yields parsed twice.
func FuzzParseOnceMatchesTwoParses(f *testing.F) {
	for _, d := range helmDocs {
		f.Add(d)
	}
	f.Add(manifestOf(helmDocs[:6]...))
	f.Add("apiVersion: v1\nkind:\u2028  ! 12\nmetadata: {name: a}\n")
	f.Add("apiVersion:\u2029  ! 1\nkind: X\nmetadata: {name: a}\n")
	f.Add("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name:\u0085    ! 0x1F\n")
	f.Fuzz(func(t *testing.T, text string) {
		requireSameStream(t, "fuzz", text)
	})
}
