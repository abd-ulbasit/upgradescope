package collect

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// manifestObject is one Kubernetes object found in a manifest stream.
// ref.File is left for the caller (CollectFiles) to fill in.
type manifestObject struct {
	group, version, kind string
	ref                  inventory.ObjectRef
}

// docError is a problem with part of a manifest stream. unassessed is the
// text that could not be decoded — a document, a JSON value, or the rest
// of the stream after an error that ends it — so nothing in it was
// counted (namedAPIs reads it). A document decoded the way kubectl decodes
// it, with only a warning (a duplicate key), has no unassessed text.
type docError struct {
	line       int // 1-based line where the problem starts
	err        error
	unassessed []byte
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
// their lines.
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
// so every object can be located.
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
func parseManifestStream(r io.Reader) (objs []manifestObject, bad []docError, err error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, err
	}
	p := &streamParser{data: data}
	for i, c := range data {
		if c == '\n' {
			p.newlines = append(p.newlines, i)
		}
	}
	yamlFrom := 0
	if utilyaml.IsJSONBuffer(data[:min(len(data), jsonPeek)]) {
		yamlFrom = p.jsonStream()
	}
	if yamlFrom >= 0 {
		p.yamlStream(yamlFrom)
	}
	return p.objs, p.bad, nil
}

// streamParser holds one stream being decoded and what was found in it.
type streamParser struct {
	data     []byte
	newlines []int // offsets of '\n' in data
	objs     []manifestObject
	bad      []docError
}

// line returns the 1-based line of byte offset off.
func (p *streamParser) line(off int) int {
	return 1 + sort.SearchInts(p.newlines, off)
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
		if err != nil {
			p.bad = append(p.bad, docError{line: first, err: err, unassessed: text})
			return
		}
		if n.Kind != yaml.DocumentNode || len(n.Content) == 0 {
			continue // comment-only document
		}
		root := n.Content[0]
		last = lastLine(root)
		objs, warnings, oerr := readObjects(root, first-1, renderedFrom)
		p.bad = append(p.bad, warnings...)
		if oerr != nil {
			p.bad = append(p.bad, docError{line: first, err: oerr, unassessed: text})
			return
		}
		p.objs = append(p.objs, objs...)
	}
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
//     (`<<: *base`) apply (see lookup).
//   - An object with an items key is a list, whatever its kind
//     (decodeToList): the wrapper is not counted, null items count nothing,
//     and an item with neither apiVersion nor kind takes the list's
//     apiVersion and its kind minus "List" (an IngressList's untyped items
//     are Ingresses). An item holding an items sequence is a list too,
//     expanded without that inference (FlattenListVisitor). kind: List
//     without items counts nothing.
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
	av, k := scalar(avHit.value), scalar(kindHit.value)
	line := n.Line
	if av == "" && k == "" {
		av, k = defAPIVersion, defKind
	} else if avHit.key != nil {
		line = avHit.key.Line
	}
	if av == "" || k == "" {
		return nil
	}
	d.warnDuplicates(n, av, k, "apiVersion", "kind", "metadata", "items")
	items, err := d.lookup(n, "items")
	if err != nil {
		return err
	}
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
			ignore, err := d.lookup(deref(ann.value), IgnoreAnnotation)
			if err != nil {
				return err
			}
			reason, err := d.lookup(deref(ann.value), IgnoreReasonAnnotation)
			if err != nil {
				return err
			}
			ref.Ignore, ref.IgnoreReason = scalar(ignore.value), scalar(reason.value)
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
		if k.Kind != yaml.ScalarNode {
			continue
		}
		if k.Tag == "!!merge" {
			mh, err := d.merge(v, name)
			if err != nil {
				return hit{}, err
			}
			if mh.value != nil {
				h = hit{key: mh.key, value: mh.value, merged: true}
			}
		} else if k.Value == name {
			h = hit{key: k, value: v}
		}
	}
	return h, nil
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
		if key.Kind != yaml.ScalarNode || key.Tag == "!!merge" || !slices.Contains(names, key.Value) {
			continue
		}
		if seen[key.Value] {
			d.warnings = append(d.warnings, docError{line: key.Line + d.lineOffset,
				err: fmt.Errorf("%s %s: duplicate key %q: kubectl uses its last value", av, k, key.Value)})
		}
		seen[key.Value] = true
	}
}

// scalar returns a non-null scalar's value; anything else (absent, null, a
// mapping such as an unrendered `{{ .Values.name }}`) is "".
func scalar(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	n = deref(n)
	if n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return ""
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

// namedAPIs returns, sorted, every group/version/kind that text could
// name: each apiVersion value it holds paired with each kind value. It
// reads text that could not be decoded, so it errs towards naming too
// much.
func namedAPIs(text []byte) []gvk {
	var avs, kinds []string
	for _, m := range apiVersionText.FindAllSubmatch(text, -1) {
		if s := string(m[1]); !slices.Contains(avs, s) {
			avs = append(avs, s)
		}
	}
	for _, m := range kindText.FindAllSubmatch(text, -1) {
		if s := string(m[1]); !slices.Contains(kinds, s) {
			kinds = append(kinds, s)
		}
	}
	var out []gvk
	for _, av := range avs {
		group, version := "", av
		if g, v, ok := strings.Cut(av, "/"); ok {
			group, version = g, v
		}
		for _, k := range kinds {
			out = append(out, gvk{group, version, k})
		}
	}
	slices.SortFunc(out, func(a, b gvk) int {
		return strings.Compare(a.group+"/"+a.version+"/"+a.kind, b.group+"/"+b.version+"/"+b.kind)
	})
	return out
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
// with reason.
func manifestInventory(clusterID, reason string, counts map[gvk]*inventory.APIUsage) inventory.Inventory {
	inv := inventory.Inventory{
		SchemaVersion: 1,
		ClusterID:     clusterID,
		Source:        inventory.SourceFiles,
		CollectedAt:   time.Now().UTC(),
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
// pass. Warnings about documents decoded anyway (duplicate keys) are
// dropped. Object refs carry stream lines and no file.
func CollectManifests(r io.Reader) (inventory.Inventory, error) {
	objs, bad, err := parseManifestStream(r)
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
	return manifestInventory("manifests", "manifests mode", counts), nil
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

func (w FileWarning) String() string { return fmt.Sprintf("%s:%d: %v", w.File, w.Line, w.Err) }

// skipDir reports whether a directory below the scan root holds files that
// are not the repository's manifests: VCS metadata, node packages, and a
// Go module vendor directory (recognised by its modules.txt; vendored Go
// modules ship test manifests with old API versions). Any other vendor/
// is walked — GitOps repositories vendor upstream manifests they deploy.
func skipDir(path, name string) bool {
	switch name {
	case ".git", ".hg", ".svn", "node_modules":
		return true
	case "vendor":
		_, err := os.Stat(filepath.Join(path, "modules.txt"))
		return err == nil
	}
	return false
}

// CollectFiles builds an Inventory from rendered manifests on disk
// (--files mode, CI gating). root is a directory, walked recursively in
// lexical order for *.yaml/*.yml/*.json, or a single file, parsed whatever
// its extension. Only api-usage is assessable offline; every other
// capability degrades with reason "files mode".
//
// A repository holds YAML that is not Kubernetes manifests (values files,
// workflows, unrendered chart templates), so non-manifest documents are
// skipped and documents that fail to decode become warnings in the summary,
// never an error; the caller decides what to do when no object was found.
// But a document that could not be decoded and whose text names an API
// that lifecycle (the knowledge base) lists as removed may hide a blocker:
// then api-usage is not available, its reason naming those documents, so
// the engine's verdict is at least unknown.
//
// Object refs carry paths relative to root (the base name for a single
// file). VCS metadata and dependency trees below root are not walked (see
// skipDir). Only I/O errors fail the walk.
func CollectFiles(root string, lifecycle []kb.APILifecycleEntry) (inventory.Inventory, FilesSummary, error) {
	removed := map[gvk]bool{}
	for _, e := range lifecycle {
		if e.Removed != nil {
			removed[gvk{e.Group, e.Version, e.Kind}] = true
		}
	}
	var hiding []string // "file:line (group/version Kind)" per unassessed part naming a removed API
	counts := map[gvk]*inventory.APIUsage{}
	var sum FilesSummary
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipDir(path, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		sum.Files++
		rel := filepath.Base(path)
		if path != root {
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".yaml" && ext != ".yml" && ext != ".json" {
				sum.Skipped++
				return nil
			}
			if rel, err = filepath.Rel(root, path); err != nil {
				return err
			}
		}
		rel = filepath.ToSlash(rel)
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		objs, bad, err := parseManifestStream(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for _, b := range bad {
			sum.Warnings = append(sum.Warnings, FileWarning{File: rel, Line: b.line, Err: b.err, Unassessed: b.unassessed != nil})
			named := namedAPIs(b.unassessed)
			if i := slices.IndexFunc(named, func(g gvk) bool { return removed[g] }); i >= 0 {
				g := named[i]
				gv := g.version
				if g.group != "" {
					gv = g.group + "/" + gv
				}
				hiding = append(hiding, fmt.Sprintf("%s:%d (%s %s)", rel, b.line, gv, g.kind))
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
		return nil
	})
	if err != nil {
		return manifestInventory("files", "files mode", nil), sum, err
	}
	inv := manifestInventory("files", "files mode", counts)
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
