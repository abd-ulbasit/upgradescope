package collect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

// kubectlJSON is the JSON kubectl's decoder makes of one YAML document's
// text (kubectlDecode's first step), the reference treeJSONConverter is
// held to.
func kubectlJSON(text string) ([]byte, error) {
	var ext runtime.RawExtension
	if err := utilyaml.NewYAMLToJSONDecoder(strings.NewReader(text)).Decode(&ext); err != nil {
		return nil, err
	}
	return ext.Raw, nil
}

// treeRoot is the first node of the first document of text, as the walk
// reads it; nil when yaml.v3 cannot read it or it holds nothing.
func treeRoot(text string) *yaml.Node {
	var n yaml.Node
	if yaml.NewDecoder(strings.NewReader(text)).Decode(&n) != nil || n.Kind != yaml.DocumentNode || len(n.Content) == 0 {
		return nil
	}
	return n.Content[0]
}

// checkTreeJSON fails unless treeJSONConverter, when it answers for text,
// answers with the bytes kubectl's decoder makes of it, and reports
// whether it answered.
func checkTreeJSON(t testing.TB, conv *treeJSONConverter, text string) (answered bool) {
	t.Helper()
	root := treeRoot(text)
	if root == nil {
		return false
	}
	got, ok := conv.json(root, []byte(text))
	if !ok {
		return false
	}
	want, err := kubectlJSON(text)
	if err != nil {
		t.Fatalf("answered %s for a document kubectl's decoder fails on (%v):\n%s", got, err, text)
	}
	if len(want) == 0 && string(got) == "null" {
		return true // a null document: RawExtension keeps no bytes for it
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("JSON differs from kubectl's decoder's\n got  %s\n want %s\nfor:\n%s", got, want, text)
	}
	return true
}

// plain scalars every YAML 1.1 reader has an opinion on: the ones go-yaml
// v2 and v3 type differently, the ones only v2 types as numbers, and
// ordinary text.
var treeJSONScalars = []string{
	"", "~", "null", "Null", "NULL", "nuLL", "true", "True", "TRUE", "tRue", "false", "False", "FALSE",
	"yes", "Yes", "YES", "no", "No", "NO", "y", "Y", "n", "N", "on", "On", "ON", "off", "Off", "OFF", "oN",
	"0", "1", "-1", "+1", "-0", "00", "01", "010", "08", "0o17", "0x1F", "0X1f", "0b101", "-0b11", "1_000", "1__0",
	"9223372036854775807", "9223372036854775808", "18446744073709551615", "18446744073709551616", "-9223372036854775808",
	"123456789012345678", "1234567890123456789", "0.5", "1.0", "1.", ".5", "-.5", "+.5", "1e3", "1E3", "1e+3", "1e-3", "1.5e3",
	".inf", ".Inf", ".INF", "-.inf", "+.inf", ".nan", ".NaN", ".NAN", ".", "..", "-", "+", "1.2.3", "10.0.0.0/8", "8080/TCP",
	"80:8080", "12:30", "1:2:3", "2001-12-14", "2001-12-14t21:59:43.10-05:00", "2001-12-14 21:59:43.10", "2001-1-1", "2001-12-14T21:59:43Z",
	"--events-addr=http://x.svc.cluster.local./", "--log-level=info", "-v", "--", ".status.conditions[?(@.type==\"Ready\")].status", ".a.b", "-a b", "+a:b", "1:", "0x", "1e", "1e+", "0b", "-0b", "0b2", "1_", "_1", "1.2.3.4", "1,000", "1 000",
	"+inf", "-inf", "inf", "+Inf", "-Infinity", "+nan", "0x1p-2", "0x1.8p1", "-0x1p3", "1_0.5", ".5e3", "5.e3", "1e3_0", "0b-1", "0b+1", "-0b1", "0b1_1", "0o1_7", "0_7", "0b", "0b_", "+0x1F", "-0x1f", ".5.5", "1.5.", "6.02e23", "1e400", "-1e400", "1e-400", "0.1", "-0.0", "1.0e0", "123456789.123456789", "9007199254740993", "9007199254740993.0", "1.7976931348623157e308",
	"100m", "128Mi", "50%", "1,2", "30 seconds", "1 2", "5s", "0.1.0", "v1.2.3", "1.2.3-rc.1+build", "-1.5", "-x", "+x", ".x", "<<", "<",
	"name", "Name", "Type", "The name of the thing", "yes please", "on-prem", "no_such", "nullable", "Nothing", "trueish", "falsey", "oFF",
	"t", "T", "f", "F", "o", "O", "yEs", "~x", "é", "日本語", "a/b", "http://x", "x-y", "*", "&", "!", "%", "@",
}

// TestTreeJSON_MatchesKubectl: where treeJSONConverter answers, its JSON
// is kubectl's decoder's, byte for byte, for every scalar above as a value,
// a key, in a sequence and in flow collections.
func TestTreeJSON_MatchesKubectl(t *testing.T) {
	var conv treeJSONConverter
	answered, total := 0, 0
	check := func(text string) {
		t.Helper()
		total++
		if checkTreeJSON(t, &conv, text) {
			answered++
		}
	}
	for _, s := range treeJSONScalars {
		check("k: " + s + "\n")
		check("k:\n  - " + s + "\n  - x\n")
		check(s + ": v\n")
		check("a: {k: " + s + "}\n")
		check("a: [" + s + ", x]\n")
		check("{" + s + ": v}\n")
		check(s + "\n")
		check("k: '" + s + "'\n")
		check("k: \"" + s + "\"\n")
		check("k: |\n  " + s + "\n")
		check("k: >-\n  " + s + "\n")
	}
	t.Logf("%d documents, %d answered by the tree", total, answered)
	if answered == 0 {
		t.Fatal("never answers")
	}
}

// TestTreeJSON_Documents: realistic and awkward documents. answer says
// whether the converter is expected to answer (it must for what Helm
// renders, and decline what it cannot be certain of).
func TestTreeJSON_Documents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer bool
		doc    string
	}{
		{"deployment", true, `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
  labels: {app: web, tier: "1", version: 1.2.3}
  annotations:
    note: 50%
spec:
  replicas: 3
  selector:
    matchLabels: {app: web}
  template:
    spec:
      containers:
      - name: web
        image: nginx:1.27
        args: ["--port=8080", -v, "--x"]
        env:
        - {name: A, value: "yes"}
        - name: B
          value: 'no'
        resources:
          limits: {cpu: 500m, memory: 128Mi}
          requests: {cpu: 0.5, memory: 1.5Gi}
        ports:
        - containerPort: 8080
          protocol: TCP
`},
		{"comments and blank lines", true, "# head\napiVersion: v1 # c\nkind: ConfigMap\n\n# mid\nmetadata:\n  name: x\ndata:\n  a: |\n    line1\n\n    line2\n  b: >\n    folded\n    text\n"},
		{"multi-line plain scalar", true, "apiVersion: v1\nkind: ConfigMap\ndata:\n  description: the first line\n    and the second\n    and the third\n"},
		{"null values", true, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name:\n  labels: ~\ndata: null\n"},
		{"empty collections", true, "a: {}\nb: []\nc: [[], {}]\n"},
		{"list kind", true, "apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: ConfigMap\n  metadata: {name: a}\n- apiVersion: v1\n  kind: ConfigMap\n"},
		{"json-looking flow", true, `{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "a\tb"}, "data": {"k": "<tag> & \u2028"}}` + "\n"},
		{"html and unicode", true, "k: <b>&amp;</b> é 日本語 \U0001F600\nq: \"\\u00e9\\n\\t\\\"\"\n"},
		{"root scalar", true, "just a string\n"},
		{"root sequence", true, "- a\n- 1\n- true\n"},
		{"non-specific tag on a number", false, "k: ! 12\n"},
		{"non-specific tag on a boolean word", false, "k: ! yes\n"},
		{"non-specific tag on nothing", false, "k: !\n"},
		{"non-specific tag in a flow sequence", false, "k: [! 12]\n"},
		{"non-specific tag in a flow mapping", false, "{k: ! on}\n"},
		{"verbatim tag", false, "k: !<tag:yaml.org,2002:str> 12\n"},
		{"tag directive", false, "%TAG ! tag:example.com,2000:\n---\nk: !x 12\n"},
		{"flags and paths that start like numbers", true, "args:\n- --events-addr=http://n.svc.cluster.local./\n- -v=4\n- .status.conditions[?(@.type==\"Ready\")].status\n- 1.2.3.4\n- 2001-12-14 21:59:43\n- +1_000\n"},
		{"a CEL rule", true, "rule: (has(self.chart) && !has(self.chartRef)) || self.a == 'x' || !has(self.sts)\nm: |\n  a && !b\n"},
		{"tag-like text in a block scalar", false, "m: |\n  - !c\n"},
		{"tag after a sequence dash", false, "k:\n- !x 12\n"},
		{"tag on its own line", false, "k:\n  !x 12\n"},
		{"exclamation in text", true, "k: Hello! world\nl: x!y\n"},
		{"no final newline", false, "k: |\n  a"},
		{"carriage returns", false, "k: v\r\nj: |\r\n  a\r\n"},
		{"UTF-16 mark", false, "\xff\xfe00"},
		{"a block scalar", true, "k: |\n  a\n"},
		{"anchors", false, "a: &x {k: v}\nb: *x\n"},
		{"anchor without alias", false, "a: &x {k: v}\n"},
		{"merge key", false, "base: &b {k: v}\nm:\n  <<: *b\n  j: w\n"},
		{"tagged scalar", false, "k: !!str 12\n"},
		{"tagged binary", false, "k: !!binary aGVsbG8=\n"},
		{"tagged mapping", false, "k: !!map {a: b}\n"},
		{"custom tag", false, "k: !foo bar\n"},
		{"duplicate key", false, "k: 1\nk: 2\n"},
		{"integer key", false, "1: a\n"},
		{"bool key", false, "yes: a\n"},
		{"null key", false, "~: a\n"},
		{"float key", false, "1.5: a\n"},
		{"complex key", false, "? [a, b]\n: c\n"},
		{"hex int", true, "k: 0x1F\n"},
		{"nan", false, "k: .nan\n"},
		{"inf", false, "k: .inf\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var conv treeJSONConverter
			got := checkTreeJSON(t, &conv, tc.doc)
			if got != tc.answer {
				t.Errorf("answered = %v, want %v", got, tc.answer)
			}
		})
	}
}

// TestTreeJSON_RandomDocuments builds documents from the scalars above in
// random shapes and holds the converter to kubectl's decoder on all of them.
func TestTreeJSON_RandomDocuments(t *testing.T) {
	rng := rand.New(rand.NewPCG(285, 1))
	var conv treeJSONConverter
	var gen func(depth int) string
	scalar := func() string {
		s := treeJSONScalars[rng.IntN(len(treeJSONScalars))]
		switch rng.IntN(8) {
		case 0:
			return "'" + strings.ReplaceAll(s, "'", "''") + "'"
		case 1:
			return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
		}
		return s
	}
	gen = func(depth int) string {
		switch n := rng.IntN(6); {
		case depth > 3 || n < 2:
			return scalar()
		case n == 2:
			var b strings.Builder
			b.WriteString("{")
			for i := range rng.IntN(4) {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "k%d: %s", i, gen(depth+1))
			}
			b.WriteString("}")
			return b.String()
		case n == 3:
			var b strings.Builder
			b.WriteString("[")
			for i := range rng.IntN(4) {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(gen(depth + 1))
			}
			b.WriteString("]")
			return b.String()
		case n == 4:
			return "{" + scalar() + ": " + gen(depth+1) + "}"
		}
		return "[" + gen(depth+1) + ", " + gen(depth+1) + "]"
	}
	answered := 0
	const docs = 20000
	for range docs {
		if checkTreeJSON(t, &conv, "doc: "+gen(0)+"\n") {
			answered++
		}
	}
	t.Logf("%d documents, %d answered by the tree", docs, answered)
	if answered < docs/10 {
		t.Errorf("answered only %d of %d", answered, docs)
	}
}

// checkV2Scalar fails unless v2Scalar, when it answers for the plain scalar
// s, makes the JSON go-yaml v2 and sigs.k8s.io/yaml make of it; it reports
// whether it answered. s must read as one plain scalar in a block mapping.
func checkV2Scalar(t testing.TB, s string) (answered bool) {
	t.Helper()
	var conv treeJSONConverter
	v, ok := conv.v2Scalar(s)
	want, err := sigsyaml.YAMLToJSON([]byte("k: " + s + "\n"))
	if !ok {
		if err == nil {
			t.Fatalf("declined %q, which kubectl's decoder makes %s of", s, want)
		}
		return false
	}
	if err != nil {
		if treeRoot("k: "+s+"\n") == nil {
			return false // not a plain scalar there ("-" starts a sequence), for either reader
		}
		t.Fatalf("answered %#v for %q, which kubectl's decoder fails on: %v", v, s, err)
	}
	got, err := json.Marshal(map[string]any{"k": v})
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%q: got %s (%v), kubectl's decoder makes %s", s, got, err, want)
	}
	return true
}

// TestV2Number_MatchesKubectl: every scalar of up to four bytes from the
// alphabet of the number syntaxes, and random longer ones, and dates and
// versions, are typed as go-yaml v2 types them (v2Number is a copy of what
// its resolve does).
func TestV2Number_MatchesKubectl(t *testing.T) {
	const alphabet = "0179.-+_exboapE"
	total, answered := 0, 0
	check := func(s string) {
		total++
		if checkV2Scalar(t, s) {
			answered++
		}
	}
	var gen func(prefix string, n int)
	gen = func(prefix string, n int) {
		if prefix != "" {
			check(prefix)
		}
		if n == 0 {
			return
		}
		for i := 0; i < len(alphabet); i++ {
			gen(prefix+alphabet[i:i+1], n-1)
		}
	}
	gen("", 4)
	for _, s := range treeJSONScalars {
		if !strings.ContainsAny(s, "\n\"'#&*!|>%@`{}[],:? ") {
			check(s)
		}
	}
	rng := rand.New(rand.NewPCG(285, 3))
	random := 100000
	if testing.Short() {
		random = 10000
	}
	dated := []string{"2001", "2001-", "12", "1", "-", "T", "t", ".", "14", "21", "10", "Z", "0", "+", "5", "_", "e", "x", "b"}
	for range random {
		var b strings.Builder
		if rng.IntN(2) == 0 {
			for range 5 + rng.IntN(16) {
				b.WriteByte(alphabet[rng.IntN(len(alphabet))])
			}
		} else {
			for range 1 + rng.IntN(8) {
				b.WriteString(dated[rng.IntN(len(dated))])
			}
		}
		check(b.String())
	}
	t.Logf("%d scalars, %d answered", total, answered)
}

// go-yaml v2's resolve is copied into v2Number, so a new version of it must
// be read again before the copy is trusted: this fails when the version the
// module selects (go.mod) is not the one checked.
func TestV2Number_TracksV2Version(t *testing.T) {
	const checked = "v2.4.4"
	mod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*go\.yaml\.in/yaml/v2 (\S+)`).FindSubmatch(mod)
	if m == nil {
		t.Fatal("go.mod does not select go.yaml.in/yaml/v2; is the module still in the build?")
	}
	if string(m[1]) != checked {
		t.Fatalf("go.mod selects go.yaml.in/yaml/v2 %s, v2Number was checked against %s: read resolve.go of the new version, update v2Number if it changed, run TestV2Number_MatchesKubectl, then change this version", m[1], checked)
	}
}

// FuzzTreeJSONMatchesKubectl: for any text, an answer from the tree is
// kubectl's decoder's own.
func FuzzTreeJSONMatchesKubectl(f *testing.F) {
	for _, s := range treeJSONScalars {
		f.Add("k: " + s + "\n")
		f.Add(s + ": " + s + "\n")
	}
	for _, s := range []string{
		"apiVersion: v1\nkind: ConfigMap\ndata: {a: 1, b: [x, 2.5, true]}\n",
		"a: &x [1]\nb: *x\n", "<<: {a: 1}\n", "? a\n: b\n", "- - - 1\n", "k: !!int '5'\n", "a: 1\n...\nb: 2\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		var conv treeJSONConverter
		checkTreeJSON(t, &conv, text)
	})
}
