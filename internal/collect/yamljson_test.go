package collect

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
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

// TestTreeJSON_OracleQuestionsAreBounded: a document of many distinct
// numeric-looking scalars is declined past the bound, not put to kubectl's
// decoder scalar by scalar without end.
func TestTreeJSON_OracleQuestionsAreBounded(t *testing.T) {
	var b strings.Builder
	for i := range 3 * maxOracleQuestions {
		fmt.Fprintf(&b, "- 1.%d\n", i)
	}
	var conv treeJSONConverter
	root := treeRoot(b.String())
	if _, ok := conv.json(root, []byte(b.String())); ok {
		t.Fatal("answered a document of more distinct numeric scalars than the bound")
	}
	if conv.asked != maxOracleQuestions+1 {
		t.Errorf("asked %d times, want %d", conv.asked, maxOracleQuestions+1)
	}
	// The same scalars repeated are asked once each.
	b.Reset()
	for range 3 * maxOracleQuestions {
		b.WriteString("- 1.5\n")
	}
	if _, ok := conv.json(treeRoot(b.String()), []byte(b.String())); !ok {
		t.Error("declined a document of one repeated scalar")
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
