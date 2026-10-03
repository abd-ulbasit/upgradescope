package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenReferenceCLI: one Markdown page per available command, from the
// same cobra tree the binary runs, with no cobra date tag and nothing that
// depends on the machine that generated it.
func TestGenReferenceCLI(t *testing.T) {
	dir := t.TempDir()
	if err := genCLIMarkdown(dir); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{
		"upgradescope.md":               "[upgradescope scan](upgradescope_scan.md)",
		"upgradescope_scan.md":          "--allow-incomplete",
		"upgradescope_serve.md":         "--retention",
		"upgradescope_agent.md":         "--interval",
		"upgradescope_tokens_create.md": "upgradescope tokens create",
	} {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if !strings.Contains(string(b), want) {
			t.Errorf("%s lacks %q", file, want)
		}
	}
	home, _ := os.UserHomeDir()
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte("Auto generated")) {
			t.Errorf("%s carries cobra's date tag (the committed copy would never be fresh)", d.Name())
		}
		if home != "" && bytes.Contains(b, []byte(home)) {
			t.Errorf("%s contains this machine's home directory", d.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "upgradescope_help.md")); err == nil {
		t.Error("upgradescope_help.md generated, want none")
	}
}

// TestGenCRDMarkdown: every schema field of the embedded CRD is a row, with
// its type, description and validation.
func TestGenCRDMarkdown(t *testing.T) {
	md, err := genCRDMarkdown(testCRD)
	if err != nil {
		t.Fatal(err)
	}
	got := string(md)
	for _, want := range []string{
		"# ClusterReadiness",
		"`example.dev/v1alpha1`",
		"| Scope | Cluster |",
		"`exr`",
		"[Acting on readiness](../guides/acting-on-readiness.md)",
		"| `spec.targets[]` | string | Minors to evaluate. | pattern `^[0-9]+\\.[0-9]+$` |",
		"| `status.targets[].verdict` | string | The verdict. | one of `ready`, `blocked`, `unknown` |",
		"| `status.targets[].findings` | array | — | at most 20 items |",
		"| `spec.ignore[].reason` | string | Why. | required; min length 1 |",
		"| Score | integer | `.status.targets[0].score` | — |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("crd.md lacks %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "`spec.ignore[].reason`") > strings.Index(got, "`status.targets[]`") {
		t.Error("rows are not in schema order (spec before status)")
	}
}

var testCRD = []byte(`apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: examples.example.dev
spec:
  group: example.dev
  scope: Cluster
  names:
    plural: examples
    singular: example
    kind: ClusterReadiness
    shortNames: [exr]
  versions:
    - name: v1alpha1
      served: true
      storage: true
      additionalPrinterColumns:
        - name: Score
          type: integer
          jsonPath: .status.targets[0].score
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                targets:
                  type: array
                  items:
                    type: string
                    description: Minors to evaluate.
                    pattern: '^[0-9]+\.[0-9]+$'
                ignore:
                  type: array
                  items:
                    type: object
                    required: [reason]
                    properties:
                      reason:
                        description: Why.
                        type: string
                        minLength: 1
            status:
              type: object
              properties:
                targets:
                  type: array
                  items:
                    type: object
                    properties:
                      verdict:
                        description: The verdict.
                        type: string
                        enum: [ready, blocked, unknown]
                      findings:
                        type: array
                        maxItems: 20
                        items:
                          type: string
`)

// TestReferenceIsFresh is the drift check for the committed references
// under docs/reference: CLI pages, CRD and REST API. A command, flag, CRD
// field or API operation changed without `make docs-gen` fails here, in
// the ordinary test run.
func TestReferenceIsFresh(t *testing.T) {
	root := filepath.Join("..", "..")
	dir := t.TempDir()
	if err := genReference(dir, filepath.Join(root, "api", "openapi.yaml")); err != nil {
		t.Fatal(err)
	}
	committed := filepath.Join(root, "docs", "reference")
	seen := map[string]bool{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		seen[rel] = true
		want, _ := os.ReadFile(p)
		got, err := os.ReadFile(filepath.Join(committed, rel))
		if err != nil {
			t.Errorf("docs/reference/%s is missing: run make docs-gen", rel)
			return nil
		}
		if !bytes.Equal(got, want) {
			t.Errorf("docs/reference/%s is stale: run make docs-gen", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A page for a command that no longer exists must go too.
	entries, err := os.ReadDir(filepath.Join(committed, "cli"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if rel := filepath.Join("cli", e.Name()); !seen[rel] {
			t.Errorf("docs/reference/%s matches no command: run make docs-gen", rel)
		}
	}
}
