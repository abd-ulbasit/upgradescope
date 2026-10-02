package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

// decodedCost is what yaml.v3 really builds for a document: every node
// (aliases not expanded) and every sequence entry. ok is false when
// yaml.v3 cannot read it.
func decodedCost(doc []byte) (c yamlCost, ok bool) {
	dec := yaml.NewDecoder(bytes.NewReader(doc))
	for {
		var n yaml.Node
		if err := dec.Decode(&n); errors.Is(err, io.EOF) {
			return c, true
		} else if err != nil {
			return c, false
		}
		for stack := []*yaml.Node{&n}; len(stack) > 0; {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if n.Kind != yaml.DocumentNode {
				c.nodes++
			}
			if n.Kind == yaml.SequenceNode {
				c.entries += len(n.Content)
			}
			stack = append(stack, n.Content...)
		}
	}
}

// kubectlNodes is how many values kubectl's YAML-to-JSON decoder (go-yaml
// v2) builds for a document's first node; ok is false when it cannot read
// it. Duplicate keys collapse in its maps, so this may under-count it.
func kubectlNodes(doc []byte) (n int, ok bool) {
	j, err := sigsyaml.YAMLToJSON(doc)
	if err != nil {
		return 0, false
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	for {
		tok, err := dec.Token()
		if err != nil {
			return n, true
		}
		if d, isDelim := tok.(json.Delim); !isDelim || d == '{' || d == '[' {
			n++
		}
	}
}

// checkNoUndercount fails when measureYAML counts fewer nodes or entries
// than yaml.v3 builds for doc, or fewer nodes than kubectl's decoder does
// (aliases aside, which only it expands, within its excessive-aliasing
// limit), or measures it differently when it arrives in small chunks.
func checkNoUndercount(t *testing.T, name string, doc []byte) {
	t.Helper()
	got := measureYAML(doc)
	if want, ok := decodedCost(doc); ok && (got.nodes < want.nodes || got.entries < want.entries) {
		t.Errorf("%s: measured %+v, yaml.v3 builds %+v\n%q", name, got, want, doc)
	}
	if !bytes.ContainsAny(doc, "*") {
		if want, ok := kubectlNodes(doc); ok && got.nodes < want {
			t.Errorf("%s: measured %+v, kubectl's decoder builds %d nodes\n%q", name, got, want, doc)
		}
	}
	var chunks [][]byte
	for p := doc; len(p) > 0; {
		n := min(len(p), 7)
		chunks = append(chunks, p[:n])
		p = p[n:]
	}
	if inChunks := measureYAMLRange(newByteSource(chunks), 0, len(doc)); inChunks != got {
		t.Errorf("%s in chunks: measured %+v, whole %+v", name, inChunks, got)
	}
}

var measureSeeds = []string{
	"a: 1\n",
	"a:\n",
	"a:\nb:\n",
	"- \n- \n",
	"-\n-\n",
	"- - - a\n",
	"- a: 1\n  b: 2\n",
	"- {}\n- []\n",
	"[]\n",
	"[[], [[]], {}]\n",
	"{a, b, c}\n",
	"{a: , b: }\n",
	"{a: 1, b: [1, 2], c: {d: e}}\n",
	"[a: 1, b: 2]\n",
	"[1,1,1,1]\n",
	`{"a":"x,y,z","b":[1,2,{"c":null}],"d":{}}` + "\n",
	"a: &x [1, 2]\nb: *x\nc: [*x, *x]\n",
	"a: &x\n  b: 1\nc:\n  <<: *x\n",
	"a: !!str 1\nb: !!map\n  c: d\n",
	"a: |\n  x: [1,2,3]\n  - y\nb: >-\n  z, z\n",
	"- |\n  x\n- >\n  y\n",
	"- k: |2\n     x\n   [1, 1]\n",
	"a: 'it''s, [x]'\nb: \"q\\\"uote, {x}\"\n",
	"a: \"multi\n  line, [x]\"\nb: 1\n",
	"? a\n: b\n? [c]\n: d\n",
	"a: b # c: d\n#- e\n",
	"a:\n# |\n  b: [1, 1, 1]\n",
	"key: value: with colon\n",
	"url: http://x:80/a,b\n",
	"a:\n- b\n- c:\n  - d\n",
	"apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: ConfigMap\n  metadata: {name: a}\n- {apiVersion: v1, kind: Secret}\n",
	"a: [\n  1,\n  2\n]\nb: {\n  c: d\n}\n",
	"a: b\n  c\n  d\n",
	"- a\n  b\n- 'c'\n",
	"a: &anchor\nb: *anchor\n",
	"!!set {a, b}\n",
	"[a, [b, c], {d: e, f}, ]\n",
	"{a: [b, c], [d]: e}\n",
	"- &a - b\n",
	"a: -1\nb: - c\n",
	"&0:\n",
	"- ! : \n",
	"{0:\", {0}} ",
	// A plain scalar's continuation line that starts with a quote: text,
	// not a quoted scalar swallowing what follows.
	"k: x\n \"\nb: [1, 1, 1]\nc: \"\"\n",
	"k:\n  x\n  \"\nb: [1, 1, 1]\nc: \"\"\n",
	"a: 1\r\nb:\r\n- 2\r\n",
	"a: 1\u2028b: [1, 1]\n",
}

func TestMeasureYAMLNeverUndercounts(t *testing.T) {
	for _, s := range measureSeeds {
		checkNoUndercount(t, "seed", []byte(s))
	}
	for name, shape := range gateHeapShapes() {
		checkNoUndercount(t, name, []byte(shape(64<<10)))
	}
	// Every YAML file in the repository, document by document.
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git") {
			return filepath.SkipDir
		}
		if ext := filepath.Ext(path); d.IsDir() || ext != ".yaml" && ext != ".yml" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		docs := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
		for {
			doc, err := docs.Read()
			if err != nil {
				return nil
			}
			checkNoUndercount(t, path, doc)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Over-counting refuses legitimate manifests early. On realistic ones the
// count is what yaml.v3 builds.
func TestMeasureYAMLIsExactOnRealisticManifests(t *testing.T) {
	for _, doc := range []string{deploymentList(20), bigConfigMap(64 << 10), pspManifest, smallConfigMaps(4 << 10)} {
		for _, d := range strings.Split(doc, "---\n") {
			if d == "" {
				continue
			}
			want, _ := decodedCost([]byte(d))
			if got := measureYAML([]byte(d)); got != want {
				t.Fatalf("measured %+v, yaml.v3 builds %+v\n%.200s", got, want, d)
			}
		}
	}
}

func FuzzMeasureYAMLNeverUndercounts(f *testing.F) {
	for _, s := range measureSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		checkNoUndercount(t, "fuzz", []byte(s))
	})
}
