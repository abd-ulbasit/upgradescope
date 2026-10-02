package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The #119 repro: in a concatenated JSON stream every object is scanned,
// so a removed API after the first one fails the gate.
func TestScanFilesConcatenatedJSON(t *testing.T) {
	dir := writeFiles(t, map[string]string{"k.json": `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"w"}}` + "\n" +
		`{"apiVersion":"extensions/v1beta1","kind":"Ingress","metadata":{"name":"old"}}` + "\n"})
	out, _, err := execScanFiles(t, "--files", dir, "--output", "json", "--target", "1.37")
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("err = %v, want the gate to fail on the Ingress (stdout %s)", err, out)
	}
	if !strings.Contains(out, "removed-api/extensions/v1beta1/Ingress") {
		t.Errorf("report lacks the Ingress blocker:\n%s", out)
	}
}

// A document that could not be decoded but names a removed API may hide a
// blocker: the verdict is unknown, not ready, and the gate fails unless
// --allow-incomplete. The JSON report names the document under
// notAssessed, SARIF records ready=false, and stderr warns.
func TestScanFilesUnassessedRemovedAPIIsUnknown(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"rendered.yaml":            "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\n",
		"chart/templates/pdb.yaml": "apiVersion: policy/v1beta1\nkind: PodDisruptionBudget\nmetadata:\n  name: {{ include \"chart.fullname\" . }}\n",
	})
	out, stderr, err := execScanFiles(t, "--files", dir, "--output", "json")
	if !errors.Is(err, ErrIncomplete) || ExitCode(err) != 2 {
		t.Fatalf("err = %v, want ErrIncomplete (exit 2)", err)
	}
	var rep struct {
		Verdict     string `json:"verdict"`
		Ready       bool   `json:"ready"`
		NotAssessed []struct {
			Capability string `json:"capability"`
			Reason     string `json:"reason"`
			Required   bool   `json:"required"`
		} `json:"notAssessed"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	found := false
	for _, g := range rep.NotAssessed {
		found = found || g.Capability == "api-usage" && g.Required && strings.Contains(g.Reason, "chart/templates/pdb.yaml:1 (policy/v1beta1 PodDisruptionBudget)")
	}
	if rep.Verdict != "unknown" || rep.Ready || !found {
		t.Errorf("verdict %q ready %v notAssessed %+v; want unknown, naming the PDB template", rep.Verdict, rep.Ready, rep.NotAssessed)
	}
	if !strings.Contains(stderr, "warning: skipped ") || !strings.Contains(stderr, "pdb.yaml:1: ") {
		t.Errorf("stderr = %q, want a skipped-document warning", stderr)
	}

	out, _, err = execScanFiles(t, "--files", dir, "--output", "sarif")
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("sarif: err = %v, want ErrIncomplete", err)
	}
	var log struct {
		Runs []struct {
			Properties struct {
				Ready bool `json:"ready"`
			} `json:"properties"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(out), &log); err != nil || len(log.Runs) != 1 || log.Runs[0].Properties.Ready {
		t.Errorf("SARIF = %v %s, want run.properties.ready false", err, out)
	}

	if _, _, err := execScanFiles(t, "--files", dir, "--allow-incomplete"); err != nil {
		t.Errorf("--allow-incomplete: err = %v, want the gate to pass on findings alone", err)
	}
}

// --help says how --files decodes and what an undecodable document does to
// the verdict.
func TestScanHelpDocumentsFilesDecoding(t *testing.T) {
	cmd := newScanCmd()
	for _, want := range []string{"kubectl apply -f", "last value", "unknown"} {
		if !strings.Contains(cmd.Long, want) {
			t.Errorf("scan --help lacks %q", want)
		}
	}
}
