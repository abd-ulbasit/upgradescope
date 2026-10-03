package chart

import (
	"bytes"
	"encoding/json"
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

// What the upgrade block says happens to the agent depends on how the chart
// runs it: with agent.manageCRD the pod exits at startup and a restart picks
// the CRD up; without, it stays up unready and the next tick does; with no
// agent there is no pod to mention. The CRD command shows in all three.
func TestNotesUpgradeBlockMatchesWhatTheAgentDoes(t *testing.T) {
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
	for _, tc := range []struct {
		name    string
		set     []string
		want    []string
		notWant []string
	}{
		{
			name:    "agent that manages the CRD",
			want:    []string{"exits at startup", "next restart picks", "commands below name the new resource"},
			notWant: []string{"never becomes Ready", "next tick picks the CRD up", "Install it with:"},
		},
		{
			name:    "agent that does not manage the CRD",
			set:     []string{"--set", "agent.manageCRD=false"},
			want:    []string{"stays up but never becomes Ready", "every tick fails", "next tick picks the CRD up", "commands below name the new resource"},
			notWant: []string{"exits at startup", "restart picks", "Install it with:"},
		},
		{
			name:    "no agent",
			set:     []string{"--set", "agent.enabled=false"},
			want:    []string{"Install it with:"},
			notWant: []string{"agent pod", "exits at startup", "restart", "next tick", "never becomes Ready", "commands below name"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := renderNotes(t, append([]string{"--is-upgrade"}, tc.set...)...)
			if !strings.Contains(out, cmd) {
				t.Errorf("NOTES do not name %q:\n%s", cmd, out)
			}
			// The trim markers join lines, so check each variant reads as prose, not
			// one 140-column line. Indented lines are commands and URLs.
			for _, line := range notesLines(t, out) {
				if !strings.HasPrefix(line, "  ") && len(line) > 90 {
					t.Errorf("NOTES line is %d columns: %q", len(line), line)
				}
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("NOTES lack %q:\n%s", w, out)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(out, w) {
					t.Errorf("NOTES wrongly contain %q:\n%s", w, out)
				}
			}
		})
	}
}

// notesLines returns the lines of the NOTES text in renderNotes' output,
// where it is the JSON string data.notes of a ConfigMap.
func notesLines(t *testing.T, out string) []string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), "notes: "); ok {
			var s string
			if err := json.Unmarshal([]byte(rest), &s); err != nil {
				t.Fatalf("notes data is not a JSON string: %v", err)
			}
			return strings.Split(s, "\n")
		}
	}
	t.Fatalf("no notes ConfigMap in:\n%s", out)
	return nil
}
