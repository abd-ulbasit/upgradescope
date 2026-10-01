package collect

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v3"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// manifestObject is one Kubernetes object found in a manifest stream.
// ref.File is left for the caller (CollectFiles) to fill in.
type manifestObject struct {
	group, version, kind string
	ref                  inventory.ObjectRef
}

// docError is one document that is not valid YAML/JSON.
type docError struct {
	line int // 1-based line where the document starts
	err  error
}

// gvk keys the per-GVK residency accumulator shared by CollectFiles and
// CollectManifests.
type gvk struct{ group, version, kind string }

// parseManifestStream splits one YAML/JSON stream into documents and returns
// the Kubernetes objects in it, in stream order, with their lines.
//
// Splitting follows kubectl's apimachinery YAMLReader (a line starting with
// "---" ends a document; only spaces or a comment may follow it, so
// "--- # comment" works where a plain `^---$` split would corrupt), but
// counts lines so every object can be located. Documents are parsed into
// yaml.v3 nodes, which carry line numbers; JSON is a YAML subset.
//
// A document that is not a mapping with apiVersion and kind is not a
// Kubernetes object (values.yaml, Chart.yaml, workflows, kustomize patches,
// Ansible lists, scalars) and is skipped. kind: List — and a typed *List
// whose items are all typed objects — is expanded into its items,
// recursively; the wrapper itself is not counted. A document that fails to
// parse is returned in bad; an invalid separator makes the rest of the
// stream unsplittable, so it ends parsing with one bad entry (the document
// before it is still parsed, as YAMLReader has returned it). err is only
// ever a read error from r.
func parseManifestStream(r io.Reader) (objs []manifestObject, bad []docError, err error) {
	br := bufio.NewReader(r)
	var doc bytes.Buffer
	lineNo, docStart := 0, 0
	flush := func() {
		defer doc.Reset()
		if strings.TrimSpace(doc.String()) == "" {
			return
		}
		var n yaml.Node
		if uerr := yaml.Unmarshal(doc.Bytes(), &n); uerr != nil {
			bad = append(bad, docError{line: docStart, err: uerr})
			return
		}
		if n.Kind != yaml.DocumentNode || len(n.Content) == 0 {
			return // comment-only document
		}
		docObjs, oerr := appendObjects(nil, n.Content[0], docStart-1, helmSource(doc.Bytes()))
		if oerr != nil {
			bad = append(bad, docError{line: docStart, err: oerr})
			return
		}
		objs = append(objs, docObjs...)
	}
	for {
		line, rerr := br.ReadBytes('\n')
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return nil, nil, rerr
		}
		if len(line) > 0 {
			lineNo++
			if rest, ok := bytes.CutPrefix(line, []byte("---")); ok {
				if t := bytes.TrimSpace(rest); len(t) > 0 && t[0] != '#' {
					flush() // the document before it is complete
					bad = append(bad, docError{line: lineNo, err: fmt.Errorf("invalid YAML document separator: %s", t)})
					return objs, bad, nil
				}
				flush()
			} else {
				if doc.Len() == 0 {
					docStart = lineNo
				}
				doc.Write(line)
			}
		}
		if rerr != nil { // io.EOF
			flush()
			return objs, bad, nil
		}
	}
}

// appendObjects appends the Kubernetes objects in node n — n itself, or the
// items of a List wrapper — to objs. lineOffset converts node lines
// (relative to the document) to stream lines.
//
// Objects and List items are never reached through YAML aliases: expanding
// aliased items would let a few lines of nested anchors (`items: [*a, *a]`
// per level) describe billions of objects, and an aliased item has no line
// of its own anyway. Scalar fields may still be aliases.
//
// An object whose metadata.name or metadata.namespace is not a string is
// an error: it is not a valid Kubernetes object, and in practice it is an
// unrendered chart template (`name: {{ include ... }}` parses as a flow
// mapping) that must not be counted next to its rendered copy.
func appendObjects(objs []manifestObject, n *yaml.Node, lineOffset int, renderedFrom string) ([]manifestObject, error) {
	if n.Kind != yaml.MappingNode {
		return objs, nil
	}
	apiVersionKey, apiVersion := field(n, "apiVersion")
	_, kind := field(n, "kind")
	av, k := scalar(apiVersion), scalar(kind)
	if av == "" || k == "" {
		return objs, nil
	}
	if _, items := field(n, "items"); items != nil {
		if items.Kind == yaml.SequenceNode && (k == "List" || strings.HasSuffix(k, "List") && allTyped(items)) {
			for _, item := range items.Content {
				var err error
				if objs, err = appendObjects(objs, item, lineOffset, renderedFrom); err != nil {
					return objs, err
				}
			}
			return objs, nil
		}
	}
	group, version := "", av
	if g, v, ok := strings.Cut(av, "/"); ok {
		group, version = g, v
	}
	ref := inventory.ObjectRef{Line: apiVersionKey.Line + lineOffset, RenderedFrom: renderedFrom}
	if _, meta := field(n, "metadata"); meta != nil && deref(meta).Kind == yaml.MappingNode {
		for _, f := range []struct {
			key string
			dst *string
		}{{"name", &ref.Name}, {"namespace", &ref.Namespace}} {
			_, v := field(deref(meta), f.key)
			if v != nil && deref(v).Kind != yaml.ScalarNode {
				return objs, fmt.Errorf("%s %s: metadata.%s is not a string (unrendered template?)", av, k, f.key)
			}
			*f.dst = scalar(v)
		}
	}
	return append(objs, manifestObject{group: group, version: version, kind: k, ref: ref}), nil
}

// allTyped reports whether every item of a sequence is a mapping with its
// own apiVersion and kind — what distinguishes a typed list (IngressList)
// from a custom resource whose kind merely ends in "List".
func allTyped(items *yaml.Node) bool {
	for _, item := range items.Content {
		if item.Kind != yaml.MappingNode {
			return false
		}
		_, av := field(item, "apiVersion")
		_, k := field(item, "kind")
		if scalar(av) == "" || scalar(k) == "" {
			return false
		}
	}
	return true
}

// field returns the key and value nodes of a mapping entry (nil when absent).
func field(m *yaml.Node, name string) (key, value *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k := m.Content[i]; k.Kind == yaml.ScalarNode && k.Value == name {
			return k, m.Content[i+1]
		}
	}
	return nil, nil
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
		CollectedAt:   time.Now().UTC(),
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{
			inventory.CapAPIUsage:        {Available: true},
			inventory.CapDeprecatedCalls: {Available: false, Reason: reason},
			inventory.CapHelm:            {Available: false, Reason: reason},
			inventory.CapAddOns:          {Available: false, Reason: reason},
			inventory.CapVersions:        {Available: false, Reason: reason},
		},
	}
	for _, u := range counts {
		inv.APIUsage = append(inv.APIUsage, *u)
	}
	sort.Slice(inv.APIUsage, func(i, j int) bool {
		a, b := inv.APIUsage[i], inv.APIUsage[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Kind < b.Kind
	})
	return inv
}

// CollectManifests builds an Inventory from a single concatenated YAML/JSON
// manifest stream (the server's CI gate endpoint body). Same parsing as
// CollectFiles, but a document that fails to parse is an error rather than
// a warning: the stream is one deliberate request body, and an API response
// has nowhere to put a warning, so silently dropping it would be a false
// pass. Object refs carry stream lines and no file.
func CollectManifests(r io.Reader) (inventory.Inventory, error) {
	objs, bad, err := parseManifestStream(r)
	if err != nil {
		return manifestInventory("manifests", "manifests mode", nil), err
	}
	if len(bad) > 0 {
		return manifestInventory("manifests", "manifests mode", nil), fmt.Errorf("line %d: %w", bad[0].line, bad[0].err)
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
	Warnings []FileWarning // documents that could not be parsed, in walk order
}

// FileWarning is a document that is not valid YAML/JSON and was skipped.
type FileWarning struct {
	File string // relative to the scanned root, slash-separated
	Line int    // 1-based line where the document starts
	Err  error
}

func (w FileWarning) String() string { return fmt.Sprintf("%s:%d: %v", w.File, w.Line, w.Err) }

// CollectFiles builds an Inventory from rendered manifests on disk
// (--files mode, CI gating). root is a directory, walked recursively in
// lexical order for *.yaml/*.yml/*.json, or a single file, parsed whatever
// its extension. Only api-usage is assessable offline; every other
// capability degrades with reason "files mode".
//
// A repository holds YAML that is not Kubernetes manifests (values files,
// workflows, unrendered chart templates), so non-manifest documents are
// skipped and documents that fail to parse become warnings in the summary,
// never an error; the caller decides what to do when no object was found.
// Object refs carry paths relative to root (the base name for a single
// file). Only I/O errors fail the walk.
func CollectFiles(root string) (inventory.Inventory, FilesSummary, error) {
	counts := map[gvk]*inventory.APIUsage{}
	var sum FilesSummary
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
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
			sum.Warnings = append(sum.Warnings, FileWarning{File: rel, Line: b.line, Err: b.err})
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
	return manifestInventory("files", "files mode", counts), sum, nil
}
