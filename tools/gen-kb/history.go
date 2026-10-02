package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// historyFrom is the oldest k8s.io/api minor readHistory reads: v0.17.0 is
// the first release tagged v0.x (older ones are tagged kubernetes-1.x).
const historyFrom = 17

// snapshot is one k8s.io/api release read from source: every GVK it
// registers, with its APILifecycle* tags. An untagged type has a zero
// Introduced.
type snapshot struct {
	k8s   version // the Kubernetes minor the release tracks
	types map[gvkOut]entry
}

// readHistory downloads k8s.io/api v0.N.0 for every minor N from from up
// to (not including) to, through the module proxy and checksum database
// like any dependency, and reads each one with readSnapshot. The pinned
// release (to) is not read: the scheme extraction covers it.
func readHistory(from, to int) ([]snapshot, error) {
	var mods []string
	for m := from; m < to; m++ {
		mods = append(mods, fmt.Sprintf("k8s.io/api@v0.%d.0", m))
	}
	if len(mods) == 0 {
		return nil, nil
	}
	dirs, err := downloadModules(mods)
	if err != nil {
		return nil, err
	}
	hist := make([]snapshot, 0, len(mods))
	for i, dir := range dirs {
		types, err := readSnapshot(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", mods[i], err)
		}
		hist = append(hist, snapshot{k8s: version{Major: 1, Minor: from + i}, types: types})
	}
	return hist, nil
}

// downloadModules runs `go mod download -json` for mods ("path@version")
// and returns each one's extracted directory, in order. It does not touch
// go.mod or go.sum.
func downloadModules(mods []string) ([]string, error) {
	cmd := exec.Command("go", append([]string{"mod", "download", "-json"}, mods...)...)
	cmd.Stderr = os.Stderr
	out, runErr := cmd.Output()
	dirs := map[string]string{} // "path@version" → dir
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var m struct{ Path, Version, Dir, Error string }
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("go mod download: %w", err)
		}
		if m.Error != "" {
			return nil, fmt.Errorf("go mod download %s@%s: %s", m.Path, m.Version, m.Error)
		}
		dirs[m.Path+"@"+m.Version] = m.Dir
	}
	if runErr != nil {
		return nil, fmt.Errorf("go mod download: %w", runErr)
	}
	ordered := make([]string, len(mods))
	for i, mod := range mods {
		if ordered[i] = dirs[mod]; ordered[i] == "" {
			return nil, fmt.Errorf("go mod download: no directory for %s", mod)
		}
	}
	return ordered, nil
}

// readSnapshot reads the k8s.io/api module source at root: each
// <group>/<version>/register.go names the group, version and registered
// types (readRegister), and the package's APILifecycle* methods, when it
// has any, their lifecycle tags (readLifecycle). Kinds
// are filtered like the scheme extraction's (skipKind). Parsing source
// instead of compiling it lets one gen-kb build read every older release;
// TestReadSnapshotMatchesScheme holds it to the scheme extraction.
func readSnapshot(root string) (map[gvkOut]entry, error) {
	regs, err := filepath.Glob(filepath.Join(root, "*", "*", "register.go"))
	if err != nil {
		return nil, err
	}
	types := map[gvkOut]entry{}
	for _, reg := range regs {
		group, ver, kinds, err := readRegister(reg)
		if err != nil {
			return nil, err
		}
		tags, err := readLifecycle(filepath.Dir(reg))
		if err != nil {
			return nil, err
		}
		for _, kind := range kinds {
			if skipKind(schema.GroupVersionKind{Group: group, Version: ver, Kind: kind}) {
				continue
			}
			e := tags[kind] // zero when untagged
			e.Group, e.Version, e.Kind = group, ver, kind
			types[e.gvk()] = e
		}
	}
	if len(types) == 0 {
		return nil, fmt.Errorf("no types under %s", root)
	}
	return types, nil
}

// readRegister reads a k8s.io/api register.go: the GroupName constant, the
// Version of SchemeGroupVersion, and the local types passed to
// scheme.AddKnownTypes(SchemeGroupVersion, ...) (&metav1.Status{} and other
// packages' types are skipped, as the scheme extraction skips them).
func readRegister(path string) (group, ver string, kinds []string, err error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return "", "", nil, err
	}
	haveGroup := false
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			switch vs.Names[0].Name {
			case "GroupName":
				if group, ok = stringLit(vs.Values[0]); ok {
					haveGroup = true
				}
			case "SchemeGroupVersion":
				if cl, ok := vs.Values[0].(*ast.CompositeLit); ok {
					ver = compositeFields(cl)["Version"]
				}
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "AddKnownTypes" {
			return true
		}
		if gv, ok := call.Args[0].(*ast.Ident); !ok || gv.Name != "SchemeGroupVersion" {
			return true
		}
		for _, a := range call.Args[1:] {
			if u, ok := a.(*ast.UnaryExpr); ok && u.Op == token.AND {
				if cl, ok := u.X.(*ast.CompositeLit); ok {
					if id, ok := cl.Type.(*ast.Ident); ok {
						kinds = append(kinds, id.Name)
					}
				}
			}
		}
		return true
	})
	if !haveGroup || ver == "" || len(kinds) == 0 {
		return "", "", nil, fmt.Errorf("%s: no GroupName, SchemeGroupVersion version or AddKnownTypes call found", path)
	}
	return group, ver, kinds, nil
}

// readLifecycle reads the APILifecycle* methods of the package in dir into
// tags per type name. Most live in zz_generated.prerelease-lifecycle.go,
// some are hand-written (core/v1 lifecycle.go), so every non-test file
// that declares one is parsed. As in the scheme extraction, a type counts
// as tagged only with an APILifecycleIntroduced method, and a zero
// deprecation, removal or replacement is no tag. A method body of any
// other shape is an error.
func readLifecycle(dir string) (map[string]entry, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	all := map[string]*entry{}
	introduced := map[string]bool{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if !bytes.Contains(src, []byte(") APILifecycle")) {
			continue // generated.pb.go and the like: skip the parse
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		if err := lifecycleMethods(path, f, all, introduced); err != nil {
			return nil, err
		}
	}
	tags := map[string]entry{}
	for name, e := range all {
		if introduced[name] {
			tags[name] = *e
		}
	}
	return tags, nil
}

// lifecycleMethods adds the APILifecycle* methods declared in f to all
// (per receiver type) and introduced.
func lifecycleMethods(path string, f *ast.File, all map[string]*entry, introduced map[string]bool) error {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 || fd.Body == nil || len(fd.Body.List) != 1 {
			continue
		}
		star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		recv, ok := star.X.(*ast.Ident)
		if !ok {
			continue
		}
		ret, ok := fd.Body.List[0].(*ast.ReturnStmt)
		if !ok {
			continue
		}
		name := fd.Name.Name
		if !strings.HasPrefix(name, "APILifecycle") {
			continue
		}
		e := all[recv.Name]
		if e == nil {
			e = &entry{}
			all[recv.Name] = e
		}
		bad := fmt.Errorf("%s: unexpected body of (*%s).%s", path, recv.Name, name)
		switch name {
		case "APILifecycleIntroduced", "APILifecycleDeprecated", "APILifecycleRemoved":
			v, ok := versionLit(ret.Results)
			if !ok {
				return bad
			}
			switch {
			case name == "APILifecycleIntroduced":
				e.Introduced = v
				introduced[recv.Name] = true
			case v == (version{}):
			case name == "APILifecycleDeprecated":
				e.Deprecated = &v
			default:
				e.Removed = &v
			}
		case "APILifecycleReplacement":
			if len(ret.Results) != 1 {
				return bad
			}
			cl, ok := ret.Results[0].(*ast.CompositeLit)
			if !ok {
				return bad
			}
			fields := compositeFields(cl)
			if g := (gvkOut{Group: fields["Group"], Version: fields["Version"], Kind: fields["Kind"]}); g != (gvkOut{}) {
				e.Replacement = &g
			}
		}
	}
	return nil
}

// versionLit reads `return 1, 31`.
func versionLit(results []ast.Expr) (version, bool) {
	if len(results) != 2 {
		return version{}, false
	}
	var n [2]int
	for i, r := range results {
		lit, ok := r.(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			return version{}, false
		}
		v, err := strconv.Atoi(lit.Value)
		if err != nil {
			return version{}, false
		}
		n[i] = v
	}
	return version{Major: n[0], Minor: n[1]}, true
}

// compositeFields returns the string-literal keyed fields of a composite
// literal ({Group: "x", Version: "v1"}); other fields are left out.
func compositeFields(cl *ast.CompositeLit) map[string]string {
	fields := map[string]string{}
	for _, el := range cl.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		k, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		if s, ok := stringLit(kv.Value); ok {
			fields[k.Name] = s
		}
	}
	return fields
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// deletedTypes returns a tombstone for every type the history snapshots
// (consecutive minors, oldest first) register that upstream (the pinned
// release) no longer does, sorted. A deleted type is not served from the
// release that deleted it, so Removed is that release — the one after the
// newest snapshot holding the type — unless its tags schedule an earlier
// removal; a tag scheduling a later one (alpha DRA types were tagged for
// removals after their deletion) yields to the deletion, and Removed is
// then marked RemovedInferred. Where kube-apiserver stopped serving the
// type earlier still, removalFixes corrects it. Tags come from the newest snapshot that has
// any; an untagged type's Introduced is the first snapshot holding it,
// which for one already in the oldest snapshot only bounds it.
func deletedTypes(hist []snapshot, upstream map[gvkOut]bool) []entry {
	type span struct {
		first, last int
		tags        *entry
	}
	spans := map[gvkOut]*span{}
	for i, s := range hist {
		for k, e := range s.types {
			if upstream[k] {
				continue
			}
			sp := spans[k]
			if sp == nil {
				sp = &span{first: i}
				spans[k] = sp
			}
			sp.last = i
			if e.Introduced != (version{}) {
				sp.tags = &e
			}
		}
	}
	out := make([]entry, 0, len(spans))
	for k, sp := range spans {
		e := entry{Group: k.Group, Version: k.Version, Kind: k.Kind, Introduced: hist[sp.first].k8s}
		if sp.tags != nil {
			e = *sp.tags
		}
		fixReplacement(&e)
		gone := version{Major: 1, Minor: hist[sp.last].k8s.Minor + 1}
		if e.Removed == nil || gone.before(*e.Removed) {
			e.Removed, e.RemovedInferred = &gone, true
		}
		fixRemoval(&e)
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].less(out[j]) })
	return out
}
