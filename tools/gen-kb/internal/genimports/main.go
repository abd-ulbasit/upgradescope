// Command genimports writes gen-kb's zz_generated_imports.go: one import
// per upstream group/version package that exports AddToScheme, plus the
// addToSchemes slice main.go registers into its runtime.Scheme.
//
// It replaces a hand-written import list that had to be reconciled with
// `go list k8s.io/api/...` on every k8s.io/api bump — and broke the weekly
// refresh outright when upstream deleted a package (scheduling/v1alpha2 in
// v0.37). Run via `go generate ./...` in tools/gen-kb.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// patterns are the upstream trees whose types carry generated APILifecycle*
// methods. apiextensions-apiserver and kube-aggregator serve
// CustomResourceDefinition and APIService, which k8s.io/api does not hold.
// Their modules must be required in tools/gen-kb/go.mod at the same
// release as k8s.io/api.
var patterns = []string{
	"k8s.io/api/...",
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/...",
	"k8s.io/kube-aggregator/pkg/apis/apiregistration/...",
}

func main() {
	out := flag.String("out", "", "output path for the generated Go file (required)")
	flag.Parse()
	if *out == "" {
		log.Fatal("genimports: -out is required")
	}
	pkgs, err := discover(patterns)
	if err != nil {
		log.Fatalf("genimports: %v", err)
	}
	src, err := render(pkgs)
	if err != nil {
		log.Fatalf("genimports: %v", err)
	}
	if err := os.WriteFile(*out, src, 0o644); err != nil {
		log.Fatalf("genimports: write %s: %v", *out, err)
	}
	fmt.Printf("genimports: wrote %d group/version packages to %s\n", len(pkgs), *out)
}

// goPackage is the subset of `go list -json` output discover needs.
type goPackage struct {
	ImportPath string
	Dir        string
	GoFiles    []string
}

// discover lists the packages matching patterns and keeps the group/version
// ones that export AddToScheme. -find skips dependency resolution, so this
// works even while the previously generated file still imports a package
// upstream has since deleted.
func discover(patterns []string) ([]string, error) {
	cmd := exec.Command("go", append([]string{"list", "-find", "-json"}, patterns...)...)
	cmd.Stderr = os.Stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %s: %w", strings.Join(patterns, " "), err)
	}
	var paths []string
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var p goPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		if !isGroupVersion(p.ImportPath) {
			continue
		}
		ok, err := exportsAddToScheme(p.Dir, p.GoFiles)
		if err != nil {
			return nil, err
		}
		if ok {
			paths = append(paths, p.ImportPath)
		}
	}
	return paths, nil
}

var versionElem = regexp.MustCompile(`^v[0-9]+((alpha|beta)[0-9]+)?$`)

// isGroupVersion reports whether an import path ends in a Kubernetes API
// version element ("v1", "v1beta2", "v1alpha3").
func isGroupVersion(importPath string) bool {
	return versionElem.MatchString(path.Base(importPath))
}

// exportsAddToScheme reports whether any of the package's files declares a
// package-level AddToScheme (the var every generated register.go defines,
// or a plain func).
func exportsAddToScheme(dir string, files []string) (bool, error) {
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return false, fmt.Errorf("parse %s: %w", filepath.Join(dir, name), err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name == "AddToScheme" {
					return true, nil
				}
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					for _, n := range spec.(*ast.ValueSpec).Names {
						if n.Name == "AddToScheme" {
							return true, nil
						}
					}
				}
			}
		}
	}
	return false, nil
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]`)

// alias names an import after its last two path elements:
// "k8s.io/api/core/v1" → "corev1".
func alias(importPath string) string {
	dir, ver := path.Split(importPath)
	return nonAlnum.ReplaceAllString(path.Base(dir)+ver, "")
}

// render returns the gofmt'ed zz_generated_imports.go for paths.
func render(paths []string) ([]byte, error) {
	if len(paths) == 0 {
		return nil, errors.New("no group/version packages found — refusing to write an empty import list")
	}
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	byAlias := make(map[string]string, len(paths))
	var imports, adds strings.Builder
	for _, p := range paths {
		a := alias(p)
		if prev, dup := byAlias[a]; dup {
			return nil, fmt.Errorf("import alias %q collides for %s and %s", a, prev, p)
		}
		byAlias[a] = p
		fmt.Fprintf(&imports, "\t%s %q\n", a, p)
		fmt.Fprintf(&adds, "\t%s.AddToScheme,\n", a)
	}
	src := fmt.Sprintf(`// Code generated by internal/genimports; DO NOT EDIT.
// Regenerate with "go generate ./..." after bumping k8s.io/api.

package main

import (
	"k8s.io/apimachinery/pkg/runtime"

%s)

// addToSchemes registers every upstream group/version package that exports
// AddToScheme.
var addToSchemes = []func(*runtime.Scheme) error{
%s}
`, imports.String(), adds.String())
	return format.Source([]byte(src))
}
