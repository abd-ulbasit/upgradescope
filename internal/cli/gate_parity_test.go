package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// parityReport is what --files and /gate must agree on.
type parityReport struct {
	Verdict  string `json:"verdict"`
	Findings []struct {
		Key      string `json:"key"`
		Severity string `json:"severity"`
		Objects  []struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
			File      string `json:"file"`
			Line      int    `json:"line"`
		} `json:"objects"`
	} `json:"findings"`
}

// The CLI gate (scan --files, and so the Action) and the server gate
// (POST /api/v1/gate) share one decoder and must judge the same manifests
// the same way: every regression-corpus input, scanned as a file and
// posted with ?path= naming it, gives the same verdict and findings, down
// to each object's file and line. An input with a part that cannot be
// decoded is refused by both: the scan does not answer ready, and the gate
// answers 422.
func TestGateParityWithScanFiles(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv, err := server.New(server.Config{Store: st, KB: k})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	corpus, err := filepath.Glob("../collect/testdata/adversarial/*")
	if err != nil || len(corpus) == 0 {
		t.Fatalf("corpus: %v (%d files)", err, len(corpus))
	}
	for _, src := range corpus {
		name := filepath.Base(src)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			dir := writeFiles(t, map[string]string{name: string(raw)})
			out, _, scanErr := execScanFiles(t, "--files", filepath.Join(dir, name), "--output", "json", "--fail-on", "never")
			resp, err := http.Post(ts.URL+"/api/v1/gate?target=1.36&fail-on=never&path="+name, "application/x-yaml", bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var gate parityReport
			if scanErr != nil {
				// No object decoded: scan refuses to judge (exit 1); the
				// gate either refuses the stream or finds nothing in it.
				_ = json.Unmarshal(body, &gate)
				if ExitCode(scanErr) != 1 || resp.StatusCode == http.StatusOK && len(gate.Findings) > 0 {
					t.Errorf("scan: %v; gate: %d %s", scanErr, resp.StatusCode, body)
				}
				return
			}
			var cli parityReport
			if err := json.Unmarshal([]byte(out), &cli); err != nil {
				t.Fatalf("scan JSON: %v\n%s", err, out)
			}
			if resp.StatusCode == http.StatusUnprocessableEntity {
				if cli.Verdict == "ready" {
					t.Errorf("the gate refuses the stream (%s) but scan --files answers ready", body)
				}
				return
			}
			if err := json.Unmarshal(body, &gate); err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("gate: %d %v\n%s", resp.StatusCode, err, body)
			}
			if !reflect.DeepEqual(cli, gate) {
				t.Errorf("scan --files and /gate disagree:\nscan %+v\ngate %+v", cli, gate)
			}
		})
	}
}
