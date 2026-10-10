package collect

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	yaml "go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/abd-ulbasit/upgradescope/internal/crd/apigroup"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// manifestObject is one Kubernetes object found in a manifest stream.
// ref.File is left for the caller (CollectFiles) to fill in.
type manifestObject struct {
	group, version, kind string
	ref                  inventory.ObjectRef
	// Add-on evidence, set only on the objects kubectl's decoder found
	// (kubectlDecode): a workload's pod template, an IngressClass's
	// controller.
	template          *podTemplate
	ingressController string
	// crd is what a CustomResourceDefinition (apiextensions.k8s.io/v1)
	// defines, set on every such object counted (see attachCRDs); an
	// unreadable one is empty.
	crd *inventory.CRD
}

// podTemplate is what add-on detection reads of a pod template: its
// labels, and its init-container and container images.
type podTemplate struct {
	labels map[string]string
	images []string
}

// docError is a problem with part of a manifest stream. unassessed is the
// text that could not be decoded — a document, a JSON value, or the rest
// of the stream after an error that ends it — so nothing in it was
// counted (names reads it). named holds what kubectl's own decoder found
// in that text, when it found anything. A document decoded the way
// kubectl decodes it, with only a warning (a duplicate key), has no
// unassessed text.
type docError struct {
	line       int // 1-based line where the problem starts
	err        error
	unassessed []byte
	named      []gvk
}

// gvk keys the per-GVK residency accumulator shared by CollectFiles and
// CollectManifests.
type gvk struct{ group, version, kind string }

// jsonPeek is how far into a stream kubectl looks for a leading "{" to
// treat it as JSON (cli-runtime's NewYAMLOrJSONDecoder buffer size).
const jsonPeek = 4096

// parseManifestStream decodes one YAML/JSON stream as kubectl apply -f
// does (apimachinery's YAMLOrJSONDecoder, then the unstructured JSON
// scheme) and returns the Kubernetes objects in it, in stream order, with
// their lines, and the add-on evidence of the objects counted (see
// kubectlDecode).
//
// A stream whose first non-space byte (in the first 4 KiB) is "{" is JSON,
// and every value in it is decoded: NDJSON, pretty-printed (`jq
// '.items[]'`) and adjacent objects alike. When the first or second value
// fails to decode, the stream continues as YAML from there (from the next
// line), as kubectl falls back; after two values a JSON error ends the
// stream. A YAML stream is split like kubectl's YAMLReader: a line starting
// with "---" ends a document, and only spaces or a comment may follow it,
// so "--- # comment" works where a plain `^---$` split would corrupt. Each
// document and JSON value is parsed into yaml.v3 nodes, which carry line
// numbers (JSON is a YAML subset), and lines are counted across the stream
// so every object can be located. Each is also decoded by kubectl's own
// decoder, which the yaml.v3 reading is checked against (see decoded):
// what kubectl would send is never silently dropped. A YAML document is
// parsed once, though: kubectl's YAML-to-JSON step is made from the nodes
// yaml.v3 read when that gives the JSON kubectl's own would (see
// treeJSONConverter), and only otherwise from the text again.
//
// A document that is not a mapping with apiVersion and kind is not a
// Kubernetes object (values.yaml, Chart.yaml, workflows, kustomize patches,
// Ansible lists, scalars) and is skipped; lists are expanded (see
// readObjects). Problems are returned in bad, in stream order: what could
// not be decoded carries its text (docError), a duplicate key is only a
// warning. An invalid separator makes the rest of the stream unsplittable,
// so it ends parsing with one entry holding the rest (the document before
// it is still counted, although kubectl drops it with the rest). err is
// only ever a read error from r.
func parseManifestStream(r io.Reader) (objs []manifestObject, ev addOnEvidence, bad []docError, err error) {
	return parseManifestStreamWith(r, false)
}

// parseManifestStreamWith is parseManifestStream, optionally with kubectl's
// decoder run on every document's text, as it was before a document was
// parsed once (#285): the reference the tests compare the shortcut with.
func parseManifestStreamWith(r io.Reader, reparse bool) (objs []manifestObject, ev addOnEvidence, bad []docError, err error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, addOnEvidence{}, nil, err
	}
	p := &streamParser{data: data, reparse: reparse}
	yamlFrom := 0
	if utilyaml.IsJSONBuffer(data[:min(len(data), jsonPeek)]) {
		yamlFrom = p.jsonStream()
	}
	if yamlFrom >= 0 {
		p.yamlStream(yamlFrom)
	}
	return p.objs, p.ev, p.bad, nil
}

// streamParser holds one stream being decoded and what was found in it.
type streamParser struct {
	data []byte
	// newlines counts the newlines in data before lineOff, the offset line
	// was last asked for.
	lineOff, newlines int
	objs              []manifestObject
	ev                addOnEvidence
	bad               []docError
	// reparse makes kubectl's decoder read every document's text itself
	// (see kubectlFor); unset, it is given the JSON made of the tree the
	// walk has already read, where that is certain to be the same.
	reparse bool
	toJSON  treeJSONConverter
}

// addEvidence adds the add-on evidence of objects kubectl's decoder
// found in a document that is counted.
func (p *streamParser) addEvidence(kubectl []manifestObject) {
	for _, o := range kubectl {
		if o.template != nil {
			p.ev.addPod(o.ref.Namespace, o.template.labels, o.template.images)
		}
		if o.ingressController != "" {
			p.ev.ingressControllers = append(p.ev.ingressControllers, o.ingressController)
		}
	}
}

// line returns the 1-based line of byte offset off. Offsets are asked for
// in stream order, so it counts the newlines since the last one asked for
// rather than indexing every newline up front, which took 8 bytes of heap
// per newline: a Helm manifest of newlines grew eightfold (#168).
func (p *streamParser) line(off int) int {
	if off < p.lineOff {
		p.lineOff, p.newlines = 0, 0
	}
	p.newlines += bytes.Count(p.data[p.lineOff:off], []byte{'\n'})
	p.lineOff = off
	return 1 + p.newlines
}

// jsonStream decodes the JSON values the stream starts with, as
// YAMLOrJSONDecoder's JSON mode does, and returns the offset where the
// stream continues as YAML, or -1 when it is done.
func (p *streamParser) jsonStream() int {
	dec := json.NewDecoder(bytes.NewReader(p.data))
	end := 0 // offset after the last decoded value
	for decoded := 0; ; decoded++ {
		var v json.RawMessage
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			return -1
		}
		start := end + len(p.data[end:]) - len(bytes.TrimLeft(p.data[end:], " \t\r\n"))
		if err != nil {
			if decoded > 1 { // an unambiguous JSON stream: kubectl stops here
				p.bad = append(p.bad, docError{line: p.line(start), err: fmt.Errorf("invalid JSON: %w", err), unassessed: p.data[start:]})
				return -1
			}
			return end + spaceThroughNewline(p.data[end:])
		}
		end = int(dec.InputOffset())
		p.document(start, end, true)
	}
}

// spaceThroughNewline is how many bytes of leading white space kubectl
// skips before decoding the rest of a stream as YAML: up to and including
// the first newline.
func spaceThroughNewline(b []byte) int {
	n := 0
	for n < len(b) {
		r, size := utf8.DecodeRune(b[n:])
		if !unicode.IsSpace(r) {
			break
		}
		n += size
		if r == '\n' {
			break
		}
	}
	return n
}

// yamlStream splits the stream from byte offset from into YAML documents,
// as kubectl's YAMLReader does, and decodes each.
func (p *streamParser) yamlStream(from int) {
	docStart := from
	for off := from; off < len(p.data); {
		end := len(p.data)
		if i := bytes.IndexByte(p.data[off:], '\n'); i >= 0 {
			end = off + i + 1
		}
		if rest, ok := bytes.CutPrefix(p.data[off:end], []byte("---")); ok {
			p.yamlDocument(docStart, off) // the document before it is complete
			if t := bytes.TrimSpace(rest); len(t) > 0 && t[0] != '#' {
				p.bad = append(p.bad, docError{line: p.line(off), err: fmt.Errorf("invalid YAML document separator: %s", t), unassessed: p.data[off:]})
				return
			}
			docStart = end
		}
		off = end
	}
	p.yamlDocument(docStart, len(p.data))
}

// yamlDocument decodes the YAML document between two byte offsets, unless
// it is blank.
func (p *streamParser) yamlDocument(start, end int) {
	if len(bytes.TrimSpace(p.data[start:end])) > 0 {
		p.document(start, end, false)
	}
}

// document decodes one YAML document or JSON value: the stream's bytes
// between two offsets.
func (p *streamParser) document(start, end int, isJSON bool) {
	text := p.data[start:end]
	first := p.line(start)
	src, renderedFrom := text, ""
	if isJSON {
		// Valid JSON holds tabs only as white space, which YAML does not
		// accept everywhere: blank them, keeping every offset.
		src = bytes.ReplaceAll(text, []byte("\t"), []byte(" "))
	} else {
		renderedFrom = helmSource(text)
	}
	dec := yaml.NewDecoder(bytes.NewReader(src))
	last := 0 // last line of the document's first node, once decoded
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			return
		}
		if last > 0 {
			// kubectl's YAML decoder reads a document's first node and
			// drops what follows it silently: a node after a "..." end
			// marker, or a second flow mapping (`{...}` lines in a stream
			// that is not JSON).
			msg := "content after the document's first node, which is all kubectl decodes"
			if err != nil {
				msg += fmt.Sprintf(" (%v)", err)
			}
			from, rest := last+1, linesFrom(text, last+1)
			for len(rest) > 0 { // from the first line with content
				line, after, _ := bytes.Cut(rest, []byte("\n"))
				if t := bytes.TrimSpace(line); len(t) > 0 && t[0] != '#' && string(t) != "..." {
					break
				}
				from, rest = from+1, after
			}
			p.bad = append(p.bad, docError{line: first + from - 1, err: errors.New(msg), unassessed: rest})
			return
		}
		var root *yaml.Node
		if err == nil {
			if n.Kind != yaml.DocumentNode || len(n.Content) == 0 {
				continue // comment-only document
			}
			root = n.Content[0]
			last = lastLine(root)
		}
		p.decodedFrom(root, err, text, first, isJSON, renderedFrom, true)
		if err != nil {
			return
		}
	}
}

// decoded settles what one document or JSON value holds. root is its
// first node as yaml.v3 read it, or nil with the error that stopped it.
// The yaml.v3 walk (readObjects) gives every object its own line;
// kubectl's own decoder (kubectlDecode) is the truth it is checked
// against, so YAML quirks the walk misreads cannot hide what kubectl
// sends:
//
//   - When the walk finds all that kubectl does, its objects are counted;
//     anything it counts beyond that is a warning (kubectl would reject or
//     skip it, as it does a list nested in a list).
//   - When it misses something, or yaml.v3 cannot read text that
//     kubectl's decoder can (valid JSON with a "\/" escape or a newline
//     before a key's colon), kubectl's objects are counted instead, at the
//     document's first line, with a warning.
//   - When the walk refuses the document (an unrendered template, items
//     through an alias), or neither can read it, it is not assessed; what
//     kubectl's decoder found in it is named with its text.
func (p *streamParser) decoded(root *yaml.Node, yerr error, text []byte, first int, isJSON bool, renderedFrom string) {
	p.decodedFrom(root, yerr, text, first, isJSON, renderedFrom, false)
}

// decodedFrom is decoded; sameDocument says root is the first node of text
// itself, so kubectl's decoder can be given the JSON made of root rather
// than parse text again (see kubectlFor).
func (p *streamParser) decodedFrom(root *yaml.Node, yerr error, text []byte, first int, isJSON bool, renderedFrom string, sameDocument bool) {
	var kubectl []manifestObject
	var kerr error
	if sameDocument {
		kubectl, kerr = p.kubectlFor(root, text, isJSON)
	} else {
		kubectl, kerr = kubectlDecode(text, isJSON)
	}
	var named []gvk
	for _, o := range kubectl {
		named = append(named, gvk{o.group, o.version, o.kind})
	}
	var objs []manifestObject
	var warnings []docError
	err := yerr
	if root != nil {
		objs, warnings, err = readObjects(root, first-1, renderedFrom)
	}
	switch {
	case root != nil && err != nil: // the walk refuses it
		p.bad = append(p.bad, warnings...)
		p.bad = append(p.bad, docError{line: first, err: err, unassessed: text, named: named})
		return
	case root == nil && kerr != nil: // neither reads it
		p.bad = append(p.bad, docError{line: first, err: yerr, unassessed: text})
		return
	case root == nil: // only kubectl's decoder reads it
		p.bad = append(p.bad, docError{line: first, err: fmt.Errorf(
			"%w: what kubectl's decoder reads here is counted, located at the first line", yerr)})
		p.useKubectl(kubectl, first, renderedFrom)
		return
	case kerr != nil: // kubectl would fail here; counting errs safe
		if len(objs) > 0 {
			p.bad = append(p.bad, docError{line: first, err: fmt.Errorf(
				"kubectl's decoder cannot read this document (%v), so kubectl would fail here; its objects are counted as read", kerr)})
		}
		p.bad = append(p.bad, warnings...)
		p.objs = append(p.objs, objs...)
		return
	}
	if missing, extra := gvkDiff(named, objs); len(missing) > 0 {
		p.bad = append(p.bad, docError{line: first, err: fmt.Errorf(
			"kubectl's decoder finds %s here, which reading the YAML did not: counting what kubectl sends, at the first line", gvkList(missing))})
		p.useKubectl(kubectl, first, renderedFrom)
		return
	} else if len(extra) > 0 {
		p.bad = append(p.bad, docError{line: first, err: fmt.Errorf(
			"counted %s, which kubectl's decoder does not send from here (it would reject or skip it)", gvkList(extra))})
	}
	p.bad = append(p.bad, warnings...)
	attachCRDs(objs, kubectl)
	p.objs = append(p.objs, objs...)
	p.addEvidence(kubectl)
}

// useKubectl counts objects kubectl's decoder found, at line first.
func (p *streamParser) useKubectl(objs []manifestObject, first int, renderedFrom string) {
	for _, o := range objs {
		o.ref.Line, o.ref.RenderedFrom = first, renderedFrom
		p.objs = append(p.objs, o)
	}
	p.addEvidence(objs)
}

// kubectlDecode returns every object kubectl apply -f sends for one YAML
// document or JSON value, without lines: decoded by apimachinery's
// YAML-to-JSON decoder (go-yaml v2, which reads only the document's first
// node) and the unstructured JSON scheme, lists flattened as
// FlattenListVisitor does. A value that is not an object, or an object
// without kind or version, yields nothing, as kubectl sends nothing for
// it; err is set only when the YAML could not be decoded at all. Objects
// carry their add-on evidence (podTemplateOf, an IngressClass's
// spec.controller): read from what kubectl sends, a document's evidence
// counts only when the document does, and never from text the YAML walk
// alone reads.
func kubectlDecode(text []byte, isJSON bool) ([]manifestObject, error) {
	raw := text
	if !isJSON {
		var ext runtime.RawExtension
		if err := utilyaml.NewYAMLToJSONDecoder(bytes.NewReader(text)).Decode(&ext); err != nil {
			return nil, err
		}
		raw = ext.Raw
	}
	return kubectlSends(raw)
}

// kubectlFor is kubectlDecode for a document whose first node root the
// walk has read: a YAML document is parsed once (#285), not by yaml.v3 for
// the walk and again by go-yaml v2 inside kubectl's decoder. The JSON that
// decoder makes of the text is made from root instead (treeJSONConverter),
// when that is certain to be the same JSON; otherwise, and for a JSON
// value, which needs no conversion, kubectl's decoder reads the text.
func (p *streamParser) kubectlFor(root *yaml.Node, text []byte, isJSON bool) ([]manifestObject, error) {
	if !isJSON && !p.reparse && root != nil {
		if raw, ok := p.toJSON.json(root, text); ok {
			return kubectlSends(raw)
		}
	}
	return kubectlDecode(text, isJSON)
}

// kubectlSends is the rest of kubectlDecode: the objects kubectl sends for
// the JSON of one document.
func kubectlSends(raw []byte) ([]manifestObject, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	obj, _, err := unstructured.UnstructuredJSONScheme.Decode(raw, nil, nil)
	if err != nil {
		return nil, nil // kubectl reports it and goes on
	}
	var out []manifestObject
	for queue := []runtime.Object{obj}; len(queue) > 0; queue = queue[1:] {
		o := queue[0]
		if meta.IsListType(o) {
			items, err := meta.ExtractList(o)
			if err != nil {
				return nil, nil // a list nested in a list: kubectl fails the whole list
			}
			queue = append(queue, items...)
			continue
		}
		k := o.GetObjectKind().GroupVersionKind()
		if k.Kind == "" || k.Version == "" || k.Kind == "List" {
			continue // no resource to map it to: kubectl reports it
		}
		mo := manifestObject{group: k.Group, version: k.Version, kind: k.Kind}
		if m, err := meta.Accessor(o); err == nil {
			mo.ref.Name, mo.ref.Namespace = m.GetName(), m.GetNamespace()
			mo.ref = withIgnore(mo.ref, m.GetAnnotations())
		}
		if u, ok := o.(*unstructured.Unstructured); ok {
			mo.template = podTemplateOf(k.GroupKind(), u.Object)
			if k.GroupKind() == (schema.GroupKind{Group: "networking.k8s.io", Kind: "IngressClass"}) {
				mo.ingressController, _, _ = unstructured.NestedString(u.Object, "spec", "controller")
			}
			if k == crdGVK {
				mo.crd = manifestCRD(u)
			}
		}
		out = append(out, mo)
	}
	return out, nil
}

// podTemplatePaths are where the workload kinds keep their pod template;
// a Pod is its own.
var podTemplatePaths = map[schema.GroupKind][]string{
	{Kind: "Pod"}:                             nil,
	{Group: "apps", Kind: "Deployment"}:       {"spec", "template"},
	{Group: "apps", Kind: "DaemonSet"}:        {"spec", "template"},
	{Group: "apps", Kind: "StatefulSet"}:      {"spec", "template"},
	{Group: "apps", Kind: "ReplicaSet"}:       {"spec", "template"},
	{Group: "extensions", Kind: "Deployment"}: {"spec", "template"},
	{Group: "extensions", Kind: "DaemonSet"}:  {"spec", "template"},
	{Group: "extensions", Kind: "ReplicaSet"}: {"spec", "template"},
	{Group: "batch", Kind: "Job"}:             {"spec", "template"},
	{Group: "batch", Kind: "CronJob"}:         {"spec", "jobTemplate", "spec", "template"},
}

// podTemplateOf returns the labels and images (init containers first) of
// the pod template of obj, an object of kind gk; nil when gk keeps none.
// Values that are not strings (an unrendered template) and empty images
// are skipped. Images injected at admission (a mesh sidecar) are not in
// a manifest, so files mode cannot see them.
func podTemplateOf(gk schema.GroupKind, obj map[string]any) *podTemplate {
	path, ok := podTemplatePaths[gk]
	if !ok {
		return nil
	}
	tmpl, _, _ := unstructured.NestedFieldNoCopy(obj, path...)
	pod, _ := tmpl.(map[string]any)
	t := &podTemplate{}
	labels, _, _ := unstructured.NestedFieldNoCopy(pod, "metadata", "labels")
	if m, ok := labels.(map[string]any); ok {
		t.labels = map[string]string{}
		for k, v := range m {
			if s, ok := v.(string); ok {
				t.labels[k] = s
			}
		}
	}
	for _, field := range []string{"initContainers", "containers"} {
		cs, _, _ := unstructured.NestedFieldNoCopy(pod, "spec", field)
		list, _ := cs.([]any)
		for _, c := range list {
			c, _ := c.(map[string]any)
			if img, ok := c["image"].(string); ok && img != "" {
				t.images = append(t.images, img)
			}
		}
	}
	return t
}

// gvkDiff compares, as multisets, what kubectl's decoder found with what
// the walk counted: missing is kubectl's but not counted, extra is
// counted but not kubectl's.
func gvkDiff(kubectl []gvk, objs []manifestObject) (missing, extra []gvk) {
	n := map[gvk]int{}
	for _, g := range kubectl {
		n[g]++
	}
	for _, o := range objs {
		n[gvk{o.group, o.version, o.kind}]--
	}
	for _, g := range slices.SortedFunc(maps.Keys(n), compareGVK) {
		for ; n[g] > 0; n[g]-- {
			missing = append(missing, g)
		}
		for ; n[g] < 0; n[g]++ {
			extra = append(extra, g)
		}
	}
	return missing, extra
}

// gvkList formats GVKs for a warning: "extensions/v1beta1 Ingress, v1 ConfigMap".
func gvkList(gs []gvk) string {
	s := make([]string, len(gs))
	for i, g := range gs {
		s[i] = g.String()
	}
	return strings.Join(s, ", ")
}

// String is "group/version Kind", or "version Kind" for the core group.
func (g gvk) String() string {
	if g.group == "" {
		return g.version + " " + g.kind
	}
	return g.group + "/" + g.version + " " + g.kind
}

func compareGVK(a, b gvk) int {
	return cmp.Or(strings.Compare(a.group, b.group), strings.Compare(a.version, b.version), strings.Compare(a.kind, b.kind))
}

// lastLine returns the last line that any node under n starts on (aliases
// are not followed).
func lastLine(n *yaml.Node) int {
	last := 0
	for stack := []*yaml.Node{n}; len(stack) > 0; {
		n, stack = stack[len(stack)-1], stack[:len(stack)-1]
		last = max(last, n.Line)
		stack = append(stack, n.Content...)
	}
	return last
}

// linesFrom returns text from its 1-based line n on.
func linesFrom(text []byte, n int) []byte {
	for ; n > 1; n-- {
		i := bytes.IndexByte(text, '\n')
		if i < 0 {
			return nil
		}
		text = text[i+1:]
	}
	return text
}

// readObjects returns the Kubernetes objects in root, the top node of one
// document or JSON value, as kubectl apply sends them. lineOffset converts
// node lines (relative to the document) to stream lines. Like kubectl's
// YAML-to-JSON conversion and unstructured scheme:
//
//   - A key's last value wins (a duplicate key is a warning), and merge keys
//     (`<<: *base`) apply (see lookup). Keys and values are read through
//     aliases (`*k : v` sets the key k's anchor holds) and !!binary
//     scalars are base64-decoded, as go-yaml v2 reads them.
//   - A top-level mapping with kind and an items key is a list, whatever
//     its kind and even without apiVersion (decodeToList): the wrapper is
//     not counted, `items: null` counts nothing, and an item with neither
//     apiVersion nor kind (a null item too) takes the list's apiVersion and
//     its kind minus "List" (an IngressList's untyped items are Ingresses;
//     without an apiVersion they have no version, and are not sent). An
//     item holding an items sequence is a list too, expanded without that
//     inference (kubectl rejects such a list; parseManifestStream warns).
//     kind: List without items counts nothing.
//
// It is an error, so the document is reported rather than counted:
//
//   - an object whose metadata.name or metadata.namespace is not a string:
//     it is not a valid Kubernetes object, and in practice it is an
//     unrendered chart template (`name: {{ include ... }}` parses as a flow
//     mapping) that must not be counted next to its rendered copy;
//   - list items reached through a YAML alias or merge key: expanding them
//     would let a few lines of nested anchors (`items: [*a, *a]` per level)
//     describe billions of objects. Scalar fields may still be aliases.
func readObjects(root *yaml.Node, lineOffset int, renderedFrom string) ([]manifestObject, []docError, error) {
	d := &objectReader{lineOffset: lineOffset, renderedFrom: renderedFrom, memo: map[mergeKey]hit{}}
	err := d.read(root, true, "", "")
	return d.objs, d.warnings, err
}

// objectReader collects the objects of one document (see readObjects).
type objectReader struct {
	lineOffset   int
	renderedFrom string
	objs         []manifestObject
	warnings     []docError
	memo         map[mergeKey]hit // lookups through merge keys
}

// hit is a mapping entry found by lookup; key and value are nil when the
// mapping has none.
type hit struct {
	key, value *yaml.Node
	merged     bool // brought in by a merge key
}

type mergeKey struct {
	node *yaml.Node
	name string
}

// read adds the objects in node n. top marks a document's top node, whose
// list items without apiVersion and kind take defAPIVersion and defKind.
func (d *objectReader) read(n *yaml.Node, top bool, defAPIVersion, defKind string) error {
	if n.Kind != yaml.MappingNode {
		if n.Kind == yaml.ScalarNode && n.Tag == "!!null" && defKind != "" && defAPIVersion != "" {
			// A typed list's null item: decodeToList infers it as it
			// infers an empty mapping.
			return d.object(n, defAPIVersion, defKind, n.Line)
		}
		return nil
	}
	avHit, err := d.lookup(n, "apiVersion")
	if err != nil {
		return err
	}
	kindHit, err := d.lookup(n, "kind")
	if err != nil {
		return err
	}
	items, err := d.lookup(n, "items")
	if err != nil {
		return err
	}
	av, k := scalar(avHit.value), scalar(kindHit.value)
	line := n.Line
	if av == "" && k == "" {
		av, k = defAPIVersion, defKind
	} else if avHit.key != nil {
		line = avHit.key.Line
	}
	// The unstructured scheme requires only kind: a top-level mapping
	// with kind and items is a list even without apiVersion (its untyped
	// items, inferred without a version, are not sent).
	if k == "" || av == "" && (!top || items.value == nil) {
		return nil
	}
	d.warnDuplicates(n, av, k, "apiVersion", "kind", "metadata", "items")
	isSeq := items.value != nil && deref(items.value).Kind == yaml.SequenceNode
	if k != "List" && !isSeq && (!top || items.value == nil) {
		return d.object(n, av, k, line)
	}
	if items.value == nil || !isSeq && deref(items.value).Tag == "!!null" {
		return nil // kind: List without items, or a top-level list with null items
	}
	if items.merged || items.value.Kind == yaml.AliasNode {
		return fmt.Errorf("%s %s: items reached through a YAML alias or merge key are not expanded", av, k)
	}
	if !isSeq {
		return fmt.Errorf("%s %s: items is not a list", av, k)
	}
	itemAPIVersion, itemKind := "", ""
	if top {
		itemAPIVersion, itemKind = av, strings.TrimSuffix(k, "List")
	}
	for _, item := range items.value.Content {
		if item.Kind == yaml.AliasNode {
			return fmt.Errorf("%s %s: an item is a YAML alias, which is not expanded", av, k)
		}
		if err := d.read(item, false, itemAPIVersion, itemKind); err != nil {
			return err
		}
	}
	return nil
}

// object adds mapping n as one object of apiVersion av and kind k, whose
// apiVersion is on document line line.
func (d *objectReader) object(n *yaml.Node, av, k string, line int) error {
	group, version := "", av
	if g, v, ok := strings.Cut(av, "/"); ok {
		group, version = g, v
	}
	if version == "" {
		return nil // apiVersion "group/": no API, and kubectl rejects it
	}
	ref := inventory.ObjectRef{Line: line + d.lineOffset, RenderedFrom: d.renderedFrom}
	metaHit, err := d.lookup(n, "metadata")
	if err != nil {
		return err
	}
	if meta := metaHit.value; meta != nil && deref(meta).Kind == yaml.MappingNode {
		meta = deref(meta)
		d.warnDuplicates(meta, av, k, "name", "namespace")
		for _, f := range []struct {
			key string
			dst *string
		}{{"name", &ref.Name}, {"namespace", &ref.Namespace}} {
			h, err := d.lookup(meta, f.key)
			if err != nil {
				return err
			}
			if h.value != nil && deref(h.value).Kind != yaml.ScalarNode {
				return fmt.Errorf("%s %s: metadata.%s is not a string (unrendered template?)", av, k, f.key)
			}
			*f.dst = scalar(h.value)
		}
		// Annotation values that are not strings are left to the
		// apiserver to reject; they suppress nothing.
		ann, err := d.lookup(meta, "annotations")
		if err != nil {
			return err
		}
		if ann.value != nil && deref(ann.value).Kind == yaml.MappingNode {
			var lerr error
			ref.Ignore, ref.IgnoreReason, ref.IgnoreLegacyKey, ref.IgnoreReasonLegacyKey = apigroup.ReadIgnore(func(key string) (string, bool) {
				h, err := d.lookup(deref(ann.value), key)
				if err != nil && lerr == nil {
					lerr = err
				}
				return scalar(h.value), h.value != nil
			})
			if lerr != nil {
				return lerr
			}
		}
	}
	d.objs = append(d.objs, manifestObject{group: group, version: version, kind: k, ref: ref})
	return nil
}

// lookup returns mapping m's entry name as kubectl's YAML decoder (go-yaml
// v2) resolves it: entries apply in order and a later one wins, including
// the keys a merge key (`<<: *base`, `<<: [*a, *b]`) brings in, and within
// a merged sequence the earlier mapping wins. Lookups through merge keys
// are memoized, so nested merges of aliased mappings cost linear time, and
// a mapping that merges itself sees none of its own keys that way.
func (d *objectReader) lookup(m *yaml.Node, name string) (hit, error) {
	var h hit
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		if isMergeKey(k) {
			mh, err := d.merge(v, name)
			if err != nil {
				return hit{}, err
			}
			if mh.value != nil {
				h = hit{key: mh.key, value: mh.value, merged: true}
			}
		} else if scalar(k) == name {
			h = hit{key: k, value: v}
		}
	}
	return h, nil
}

// isMergeKey reports whether mapping key k is a merge key as go-yaml v2
// reads one: a plain `<<` (or `!!merge <<`), not an alias of one.
func isMergeKey(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.Tag == "!!merge" && k.Value == "<<"
}

// merge looks name up in the value of a merge key: a mapping, or a
// sequence of mappings whose earlier entries win.
func (d *objectReader) merge(v *yaml.Node, name string) (hit, error) {
	v = deref(v)
	mk := mergeKey{v, name}
	if h, ok := d.memo[mk]; ok {
		return h, nil
	}
	d.memo[mk] = hit{} // a merge cycle resolves to nothing
	var h hit
	switch v.Kind {
	case yaml.MappingNode:
		var err error
		if h, err = d.lookup(v, name); err != nil {
			return hit{}, err
		}
	case yaml.SequenceNode:
		for i := len(v.Content) - 1; i >= 0; i-- {
			if deref(v.Content[i]).Kind != yaml.MappingNode {
				return hit{}, errors.New("a merge key's sequence holds something other than mappings")
			}
			eh, err := d.merge(v.Content[i], name)
			if err != nil {
				return hit{}, err
			}
			if eh.value != nil {
				h = eh
			}
		}
	default:
		return hit{}, errors.New("a merge key's value is neither a mapping nor a sequence of mappings")
	}
	d.memo[mk] = h
	return h, nil
}

// warnDuplicates adds a warning for each of names that is a key of
// mapping m more than once: kubectl decodes it without complaint and uses
// the last value, as lookup does, but the author may have meant another.
func (d *objectReader) warnDuplicates(m *yaml.Node, av, k string, names ...string) {
	seen := map[string]bool{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		key := m.Content[i]
		name := scalar(key)
		if isMergeKey(key) || !slices.Contains(names, name) {
			continue
		}
		if seen[name] {
			d.warnings = append(d.warnings, docError{line: key.Line + d.lineOffset,
				err: fmt.Errorf("%s %s: duplicate key %q: kubectl uses its last value", av, k, name)})
		}
		seen[name] = true
	}
}

// scalar returns a non-null scalar's value as kubectl's YAML decoder reads
// it, through aliases and with a !!binary scalar base64-decoded; anything
// else (absent, null, a mapping such as an unrendered `{{ .Values.name
// }}`, invalid base64, which kubectl rejects) is "".
func scalar(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	n = deref(n)
	if n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return ""
	}
	if n.Tag == "!!binary" {
		b, err := base64.StdEncoding.DecodeString(n.Value)
		if err != nil {
			return ""
		}
		return string(b)
	}
	return n.Value
}

// deref follows YAML aliases to the anchored node.
func deref(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

// apiVersionText and kindText find apiVersion and kind values in text that
// did not decode, written as YAML or JSON keys.
var (
	apiVersionText = regexp.MustCompile(`\bapiVersion["']?\s*:\s*["']?([A-Za-z0-9][-A-Za-z0-9.]*(?:/[A-Za-z0-9][-A-Za-z0-9.]*)?)`)
	kindText       = regexp.MustCompile(`\bkind["']?\s*:\s*["']?([A-Za-z][A-Za-z0-9]*)`)
)

// names reports the first of apis (in order) that an unassessed part could
// hold: one kubectl's decoder found in it, or one whose apiVersion and kind
// values (a kind ending in "List" also as its item kind, FooList's Foo)
// both appear in its text. It reads text that could not be decoded, so it
// errs towards naming too much. Each of apis is looked up, rather than
// every apiVersion in the text paired with every kind, so a crafted text
// holding thousands of each costs no more than reading it.
func (b docError) names(apis []gvk) (gvk, bool) {
	for _, g := range apis {
		if slices.Contains(b.named, g) {
			return g, true
		}
	}
	avs, kinds := map[string]bool{}, map[string]bool{}
	for _, m := range apiVersionText.FindAllSubmatch(b.unassessed, -1) {
		avs[string(m[1])] = true
	}
	for _, m := range kindText.FindAllSubmatch(b.unassessed, -1) {
		kinds[string(m[1])] = true
		if k, ok := strings.CutSuffix(string(m[1]), "List"); ok && k != "" {
			kinds[k] = true
		}
	}
	for _, g := range apis {
		av := g.version
		if g.group != "" {
			av = g.group + "/" + av
		}
		if avs[av] && kinds[g.kind] {
			return g, true
		}
	}
	return gvk{}, false
}

// helmSource returns the template path from helm template's
// "# Source: <chart>/templates/x.yaml" comment in a document's leading
// comment block, or "".
func helmSource(doc []byte) string {
	for line := range strings.Lines(string(doc)) {
		t := strings.TrimSpace(line)
		if src, ok := strings.CutPrefix(t, "# Source: "); ok {
			return strings.TrimSpace(src)
		}
		if t != "" && !strings.HasPrefix(t, "#") {
			return ""
		}
	}
	return ""
}

// accumulate adds objects to the per-GVK residency counts, keeping up to
// inventory.MaxObjectRefs refs per GVK and counting the rest as omitted.
func accumulate(counts map[gvk]*inventory.APIUsage, objs []manifestObject) {
	for _, o := range objs {
		k := gvk{o.group, o.version, o.kind}
		u := counts[k]
		if u == nil {
			u = &inventory.APIUsage{Group: o.group, Version: o.version, Kind: o.kind, Namespaces: map[string]int{}}
			counts[k] = u
		}
		u.Count++
		u.Namespaces[o.ref.Namespace]++ // cluster-scoped/unset → key ""
		if len(u.Objects) < inventory.MaxObjectRefs {
			u.Objects = append(u.Objects, o.ref)
		} else {
			u.ObjectsOmitted++
		}
	}
}

// manifestInventory wraps accumulated GVK counts in the offline-inventory
// envelope: only api-usage is assessable; every other capability degrades
// with reason (the callers then assess add-ons and CRDs, see assessAddOns
// and assessCRDs).
func manifestInventory(clusterID, reason string, counts map[gvk]*inventory.APIUsage) inventory.Inventory {
	inv := inventory.Inventory{
		SchemaVersion:   1,
		ClusterID:       clusterID,
		CollectorSchema: inventory.CurrentCollectorSchema,
		Source:          inventory.SourceFiles,
		CollectedAt:     time.Now().UTC(),
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{
			inventory.CapAPIUsage:        {Available: true},
			inventory.CapDeprecatedCalls: {Available: false, Reason: reason},
			inventory.CapHelm:            {Available: false, Reason: reason},
			inventory.CapAddOns:          {Available: false, Reason: reason},
			inventory.CapVersions:        {Available: false, Reason: reason},
		},
	}
	inv.APIUsage = usageRows(counts)
	return inv
}

// usageRows returns accumulated GVK counts as rows sorted by group,
// version and kind; nil when there are none.
func usageRows(counts map[gvk]*inventory.APIUsage) []inventory.APIUsage {
	var rows []inventory.APIUsage
	for _, u := range counts {
		rows = append(rows, *u)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Kind < b.Kind
	})
	return rows
}

// CollectManifests builds an Inventory from a single concatenated YAML/JSON
// manifest stream (the server's CI gate endpoint body). Same parsing as
// CollectFiles, but a document that fails to decode is an error rather than
// a warning: the stream is one deliberate request body, and an API response
// has nowhere to put a warning, so silently dropping it would be a false
// pass. Warnings about documents decoded anyway (duplicate keys, objects
// located by kubectl's decoder) are dropped. Object refs carry stream lines
// and no file. Add-ons and CRDs are assessed as CollectFiles assesses them,
// so the server gate without ?cluster= judges a render as scan --files
// does. With ?cluster=, the gate merges this inventory's API usage, add-ons
// and CRDs into the cluster's and judges its custom resources again
// against the merged CRDs (AssessCRDs).
func CollectManifests(r io.Reader, addons []registry.AddOn) (inventory.Inventory, error) {
	objs, ev, bad, err := parseManifestStream(r)
	if err != nil {
		return manifestInventory("manifests", "manifests mode", nil), err
	}
	for _, b := range bad {
		if b.unassessed != nil {
			return manifestInventory("manifests", "manifests mode", nil), fmt.Errorf("line %d: %w", b.line, b.err)
		}
	}
	counts := map[gvk]*inventory.APIUsage{}
	accumulate(counts, objs)
	inv := manifestInventory("manifests", "manifests mode", counts)
	assessAddOns(&inv, ev, addons)
	assessCRDs(&inv, crdsOf(objs))
	return inv, nil
}

// assessAddOns runs the add-on matcher over the evidence found in
// manifests and marks add-ons assessed.
func assessAddOns(inv *inventory.Inventory, ev addOnEvidence, addons []registry.AddOn) {
	var unrec []string
	inv.AddOns, unrec = matchAddOns(ev, addons)
	setUnrecognized(inv, unrec)
	inv.Capabilities[inventory.CapAddOns] = inventory.CapabilityStatus{Available: true}
}

// FilesSummary describes what a CollectFiles walk saw.
type FilesSummary struct {
	Files    int           // regular files walked
	Skipped  int           // files that held no Kubernetes object
	Objects  int           // Kubernetes objects found (List items, not wrappers)
	Warnings []FileWarning // in walk order
}

// FileWarning is a problem with part of a file: a document that could not
// be decoded and was skipped (Unassessed), or one decoded the way kubectl
// decodes it despite a duplicate key.
type FileWarning struct {
	File string // relative to the scanned root, slash-separated
	Line int    // 1-based line where the problem starts
	Err  error
	// Unassessed: the document (or the rest of the file, after an error
	// that ends it) was skipped, so no object in it was counted.
	Unassessed bool
}

// String is "file:line: error", or "dir: error" for a directory not walked
// (Line 0).
func (w FileWarning) String() string {
	if w.Line == 0 {
		return fmt.Sprintf("%s: %v", w.File, w.Err)
	}
	return fmt.Sprintf("%s:%d: %v", w.File, w.Line, w.Err)
}

// skipDir reports whether a directory below the scan root holds files that
// are not the repository's manifests: VCS metadata, node packages, and a
// Go module vendor directory (recognised by its modules.txt; vendored Go
// modules ship test manifests with old API versions). Any other vendor/
// is walked — GitOps repositories vendor upstream manifests they deploy.
// why names a skipped dependency tree for the warning; skipped VCS
// metadata has none, and is not worth one.
func skipDir(path, name string) (skip bool, why string) {
	switch name {
	case ".git", ".hg", ".svn":
		return true, ""
	case "node_modules":
		return true, "node packages"
	case "vendor":
		if _, err := os.Stat(filepath.Join(path, "modules.txt")); err == nil {
			return true, "a Go module vendor directory"
		}
	}
	return false, ""
}

// CollectFiles builds an Inventory from rendered manifests on disk
// (--files mode, CI gating). root is a directory, walked recursively in
// lexical order for *.yaml/*.yml/*.json, or a single file, parsed whatever
// its extension. API usage, add-ons and CRDs are assessed offline:
// add-ons from the pod templates of workload manifests (images and labels,
// see podTemplateOf) and IngressClass controllers, with the matchers a
// live scan uses (matchAddOns); CRDs from CustomResourceDefinition
// manifests, which judge the custom resources in the files (assessCRDs).
// Every other capability degrades with reason "files mode": a manifest
// carries no cluster version and no Helm release.
//
// A repository holds YAML that is not Kubernetes manifests (values files,
// workflows, unrendered chart templates), so non-manifest documents are
// skipped and documents that fail to decode become warnings in the summary,
// never an error; the caller decides what to do when no object was found.
// But a document that could not be decoded and whose text (or what
// kubectl's decoder finds in it) names an API the knowledge base lists as
// removed may hide a blocker: then api-usage is not available, its reason
// naming those documents, so the engine's verdict is at least unknown.
// Images in a document not decoded are not read either.
//
// Object refs carry paths relative to root (the base name for a single
// file). A symlink named as root is resolved. VCS metadata and dependency
// trees below root are not walked (see skipDir), nor are symlinked
// directories, which kubectl apply -R does not walk either (following them
// could also leave the repository, or loop); each skipped directory but VCS
// metadata is a warning. Symlinked files are read, as kubectl reads them.
// Only I/O errors fail the walk.
func CollectFiles(root string, k kb.KB) (inventory.Inventory, FilesSummary, error) {
	var removed []gvk
	for _, e := range k.APILifecycle {
		if e.Removed != nil {
			removed = append(removed, gvk{e.Group, e.Version, e.Kind})
		}
	}
	slices.SortFunc(removed, compareGVK)
	var hiding []string // "file:line (group/version Kind)" per unassessed part naming a removed API
	var crds []inventory.CRD
	counts := map[gvk]*inventory.APIUsage{}
	var ev addOnEvidence
	var sum FilesSummary
	if fi, err := os.Lstat(root); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		if st, err := os.Stat(root); err == nil && st.IsDir() { // WalkDir would read it as a file
			if root, err = filepath.EvalSymlinks(root); err != nil {
				return manifestInventory("files", "files mode", nil), sum, err
			}
		}
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.Base(path)
		if path != root {
			if rel, err = filepath.Rel(root, path); err != nil {
				return err
			}
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if path == root {
				return nil
			}
			if skip, why := skipDir(path, d.Name()); skip {
				if why != "" {
					sum.Warnings = append(sum.Warnings, FileWarning{File: rel, Err: fmt.Errorf("not walked: %s (scan it as the root to include it)", why)})
				}
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 && path != root {
			if st, err := os.Stat(path); err == nil && st.IsDir() {
				sum.Warnings = append(sum.Warnings, FileWarning{File: rel,
					Err: errors.New("symlinked directory not walked, as kubectl apply -R does not walk it (scan its target as the root to include it)")})
				return nil
			}
		}
		sum.Files++
		if path != root {
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".yaml" && ext != ".yml" && ext != ".json" {
				sum.Skipped++
				return nil
			}
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		objs, fileEv, bad, err := parseManifestStream(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for _, b := range bad {
			sum.Warnings = append(sum.Warnings, FileWarning{File: rel, Line: b.line, Err: b.err, Unassessed: b.unassessed != nil})
			if g, ok := b.names(removed); ok {
				hiding = append(hiding, fmt.Sprintf("%s:%d (%s)", rel, b.line, g))
			}
		}
		if len(objs) == 0 {
			sum.Skipped++
			return nil
		}
		for i := range objs {
			objs[i].ref.File = rel
		}
		sum.Objects += len(objs)
		accumulate(counts, objs)
		crds = append(crds, crdsOf(objs)...)
		ev.images = append(ev.images, fileEv.images...)
		ev.labelled = append(ev.labelled, fileEv.labelled...)
		ev.ingressControllers = append(ev.ingressControllers, fileEv.ingressControllers...)
		return nil
	})
	if err != nil {
		return manifestInventory("files", "files mode", nil), sum, err
	}
	inv := manifestInventory("files", "files mode", counts)
	assessAddOns(&inv, ev, k.AddOns)
	assessCRDs(&inv, crds)
	if len(hiding) > 0 {
		listed := hiding[:min(len(hiding), 5)]
		more := ""
		if n := len(hiding) - len(listed); n > 0 {
			more = fmt.Sprintf(" and %d more", n)
		}
		inv.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Reason: fmt.Sprintf(
			"%d document(s) that name a removed API could not be decoded, so their objects were not assessed: %s%s",
			len(hiding), strings.Join(listed, ", "), more)}
	}
	return inv, sum, nil
}
