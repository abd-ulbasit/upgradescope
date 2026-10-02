package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readGz(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGenerate(t *testing.T) {
	when := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	if err := generate(dir, when); err != nil {
		t.Fatal(err)
	}

	for file, want := range map[string]string{
		"completions/upgradescope.bash": "__start_upgradescope",
		"completions/upgradescope.zsh":  "#compdef upgradescope",
		"completions/upgradescope.fish": "complete -c upgradescope",
		"completions/upgradescope.ps1":  "Register-ArgumentCompleter",
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

	root := readGz(t, filepath.Join(dir, "manpages/upgradescope.1.gz"))
	for _, want := range []string{`.TH "UPGRADESCOPE" "1" "Oct 2026"`, `upgradescope-scan(1)`, `upgradescope-version(1)`} {
		if !strings.Contains(root, want) {
			t.Errorf("upgradescope.1 lacks %q:\n%s", want, root)
		}
	}
	if strings.Contains(root, "Auto generated") {
		t.Error("upgradescope.1 carries cobra's auto-generated date tag (not reproducible)")
	}
	scan := readGz(t, filepath.Join(dir, "manpages/upgradescope-scan.1.gz"))
	if !strings.Contains(scan, `\fB--target\fP`) {
		t.Errorf("upgradescope-scan.1 does not document --target:\n%s", scan)
	}
	if _, err := os.Stat(filepath.Join(dir, "manpages", "upgradescope-help.1.gz")); err == nil {
		t.Error("manpages/upgradescope-help.1.gz generated, want none")
	}
}

// Packages and archives are checksummed and signed, so the same commit
// must produce the same bytes (gzip headers carry no mtime).
func TestGenerateReproducible(t *testing.T) {
	when := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a, b := t.TempDir(), t.TempDir()
	if err := generate(a, when); err != nil {
		t.Fatal(err)
	}
	if err := generate(b, when); err != nil {
		t.Fatal(err)
	}
	n := 0
	err := filepath.WalkDir(a, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		n++
		rel, _ := filepath.Rel(a, p)
		x, _ := os.ReadFile(p)
		y, err := os.ReadFile(filepath.Join(b, rel))
		if err != nil {
			t.Errorf("%s only in the first run", rel)
			return nil
		}
		if !bytes.Equal(x, y) {
			t.Errorf("%s differs between two runs", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("generate wrote nothing")
	}
}

// A rerun replaces the output: a command removed since the last run must
// not leave its man page behind to be packaged.
func TestGenerateReplaces(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "manpages", "upgradescope-gone.1.gz")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := generate(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale man page survived a rerun")
	}
}

func TestParseDate(t *testing.T) {
	got, err := parseDate("2026-10-01T12:00:00Z")
	if err != nil || !got.Equal(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("parseDate(RFC3339) = %v, %v", got, err)
	}
	t.Setenv("SOURCE_DATE_EPOCH", "1790000000")
	got, err = parseDate("")
	if err != nil || got.Unix() != 1790000000 {
		t.Errorf("parseDate(\"\") with SOURCE_DATE_EPOCH = %v, %v", got, err)
	}
	if _, err := parseDate("yesterday"); err == nil {
		t.Error("parseDate(\"yesterday\") succeeded")
	}
}
