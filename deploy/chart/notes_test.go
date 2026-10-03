package chart

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
)

// renderNotes renders templates/NOTES.txt. helm template never prints
// NOTES, so the test renders a copy of the chart whose NOTES.txt is an
// ordinary template. extra are further helm template arguments.
func renderNotes(t *testing.T, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if path == filepath.Join("templates", "NOTES.txt") {
			// Helm parses a rendered template as YAML: carry the text in a
			// ConfigMap's data.
			path = filepath.Join("templates", "notes-copy.yaml")
			b = []byte("{{- define \"notes-body\" -}}\n" + string(b) + "\n{{- end }}\n" +
				"kind: ConfigMap\napiVersion: v1\nmetadata:\n  name: notes\ndata:\n  notes: {{ include \"notes-body\" . | toJson }}\n")
		}
		dst := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"template", "upgradescope", dir, "--namespace", "upgradescope"}, extra...)
	cmd := exec.Command(helmBin(t), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

// A plain helm upgrade across the group move leaves the agent exiting at
// startup, since Helm installs crds/ on first install only and the agent
// may not create the CRD. NOTES.txt on upgrade shows the same command the
// agent's error names, at this chart's release; a first install, which
// does install crds/, does not.
func TestNotesNameTheCRDInstallCommandOnUpgrade(t *testing.T) {
	raw, err := os.ReadFile("Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	cmd := crd.InstallCommand(meta.AppVersion)
	if strings.Contains(cmd, "<tag>") {
		t.Fatalf("Chart.yaml appVersion %q names no release tag: %s", meta.AppVersion, cmd)
	}
	if up := renderNotes(t, "--is-upgrade"); !strings.Contains(up, cmd) {
		t.Errorf("NOTES on upgrade do not name %q:\n%s", cmd, up)
	}
	if first := renderNotes(t); strings.Contains(first, "kubectl apply -f https://raw") {
		t.Errorf("NOTES on a first install name a CRD install command:\n%s", first)
	}
}
