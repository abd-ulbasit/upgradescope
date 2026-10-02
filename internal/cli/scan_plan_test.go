package cli

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// planStub judges inv the way runScan does after collecting, --plan
// included.
func planStub(t *testing.T, inv inventory.Inventory) func(scanOptions) (engine.Report, error) {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	return func(opts scanOptions) (engine.Report, error) {
		return evaluateScan(inv, k, opts, time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)), nil
	}
}

// planInventory is a 1.31 cluster with a DRA v1alpha3 DeviceClass (removed
// in 1.34: a warning at the 1.33 hop, a blocker from 1.34) and 1.31
// kubelets (out of skew from 1.35).
func planInventory() inventory.Inventory {
	inv := liveInventory("v1.31.4")
	inv.APIUsage = []inventory.APIUsage{{
		Group: "resource.k8s.io", Version: "v1alpha3", Kind: "DeviceClass", Count: 1, Namespaces: map[string]int{"": 1},
		Objects: []inventory.ObjectRef{{Name: "gpu.example.com"}},
	}}
	inv.Nodes = []inventory.NodeInfo{{Name: "node-a", KubeletVersion: "v1.31.4"}}
	return inv
}

func TestScanPlanTable(t *testing.T) {
	out, err := execScan(t, []string{"--target", "1.36", "--plan"}, planStub(t, planInventory()))
	// The gate judges the final target, as without --plan.
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("err = %v, want ErrGateFailed (blockers at 1.36)\n%s", err, out)
	}
	for _, want := range []string{
		"UPGRADE PLAN  1.31 → 1.36 in 5 upgrades",
		"  1.31 → 1.32  ready    0 blockers, 0 warnings, 1 info\n",
		"      + info     [deprecated-api] resource.k8s.io/v1alpha3 DeviceClass deprecated in 1.34, after target 1.32 (1 object)\n",
		"  1.32 → 1.33  ready    0 blockers, 1 warning, 0 info\n",
		"      + warning  [removed-api] resource.k8s.io/v1alpha3 DeviceClass removed in 1.34 (1 object)\n",
		"  1.33 → 1.34  blocked  1 blocker, 0 warnings, 0 info\n",
		"      ~ warning → blocker  resource.k8s.io/v1alpha3 DeviceClass removed in 1.34 (1 object) (listed at 1.33)\n",
		"      + blocker  [version-skew] 1 node(s) would exceed kubelet version skew after upgrading to 1.35\n",
		"      1 carried from earlier upgrades\n",
		"  1.35 → 1.36  blocked  2 blockers, 0 warnings, 0 info\n      nothing new; 2 carried from earlier upgrades\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output lacks %q:\n%s", want, out)
		}
	}
	// The plan precedes the findings at the final target.
	if strings.Index(out, "UPGRADE PLAN") > strings.Index(out, "\nBLOCKER (") {
		t.Errorf("the plan is not before the findings:\n%s", out)
	}
	// Without --plan, there is no plan.
	out, _ = execScan(t, []string{"--target", "1.36"}, planStub(t, planInventory()))
	if strings.Contains(out, "UPGRADE PLAN") {
		t.Errorf("a scan without --plan renders a plan:\n%s", out)
	}
}

func TestScanPlanMarkdown(t *testing.T) {
	out, _ := execScan(t, []string{"--target", "1.36", "--plan", "--output", "markdown"}, planStub(t, planInventory()))
	for _, want := range []string{
		"**Upgrade plan:** 1.31 → 1.36 in 5 upgrades. Each finding is listed at the first upgrade it affects.\n",
		"| Upgrade | Verdict | Blockers | Warnings | New findings | Severity changes |\n|---|---|---|---|---|---|\n",
		"| 1.32 → 1.33 | ready | 0 | 1 | warning: resource.k8s.io/v1alpha3 DeviceClass removed in 1.34 (1 object) |  |\n",
		"| 1.33 → 1.34 | blocked | 1 | 0 |  | warning → blocker: resource.k8s.io/v1alpha3 DeviceClass removed in 1.34 (1 object) |\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown lacks %q:\n%s", want, out)
		}
	}
}

// JSON gains hops[]; the rest of the report is the final target's.
func TestScanPlanJSON(t *testing.T) {
	out, err := execScan(t, []string{"--target", "1.36", "--plan", "--output", "json", "--fail-on", "never"}, planStub(t, planInventory()))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Target  string `json:"target"`
		Verdict string `json:"verdict"`
		Hops    []struct {
			From, To, Verdict string
			Changed           []struct{ Key, Severity, Was, Since string }
		} `json:"hops"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Target != "1.36" || got.Verdict != "blocked" || len(got.Hops) != 5 {
		t.Fatalf("target %s, verdict %s, %d hops; want 1.36, blocked, 5\n%s", got.Target, got.Verdict, len(got.Hops), out)
	}
	if h := got.Hops[2]; h.From != "1.33" || h.To != "1.34" || len(h.Changed) != 1 || h.Changed[0].Was != "warning" || h.Changed[0].Since != "1.33" {
		t.Errorf("hop 3 = %+v, want 1.33 → 1.34 with the DeviceClass changed from warning (listed at 1.33)", h)
	}

	sch, schemaDoc := compileReportSchema(t)
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Errorf("a --plan report does not validate against api/report.schema.json: %v", err)
	}
	if extra := unlistedFields(schemaDoc, schemaDoc, inst, ""); len(extra) > 0 {
		t.Errorf("a --plan report has fields api/report.schema.json does not list: %v", extra)
	}
}

// Files mode plans from --from; ignore rules apply to every hop, so a
// suppressed finding is in no hop.
func TestScanPlanFiles(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"rendered/all.yaml":  removedAPIs,
		".upgradescope.yaml": "ignore:\n  - key: removed-api/batch/v1beta1/CronJob\n    reason: deleted next sprint\n",
	})
	out, _, err := execScanFiles(t, "--files", filepath.Join(dir, "rendered"), "--config", filepath.Join(dir, ".upgradescope.yaml"),
		"--plan", "--from", "1.33", "--output", "json", "--fail-on", "never")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Hops []struct {
			From, To string
			Findings []struct{ Key string }
		} `json:"hops"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Hops) != 3 || got.Hops[0].From != "1.33" || got.Hops[2].To != "1.36" {
		t.Fatalf("hops = %+v, want 1.33 → 1.34 → 1.35 → 1.36", got.Hops)
	}
	if !strings.Contains(out, `"removed-api/networking.k8s.io/v1beta1/Ingress"`) {
		t.Errorf("the Ingress blocker is in no hop:\n%s", out)
	}
	for _, h := range got.Hops {
		for _, f := range h.Findings {
			if f.Key == "removed-api/batch/v1beta1/CronJob" {
				t.Errorf("hop %s lists the suppressed CronJob finding", h.To)
			}
		}
	}
}

func TestScanPlanFlagErrors(t *testing.T) {
	dir := writeFiles(t, map[string]string{"all.yaml": removedAPIs})
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--target", "1.36", "--from", "1.33"}, "--from needs --plan"},
		{[]string{"--target", "1.36", "--plan", "--from", "1.33"}, "--from is for --files scans"},
		{[]string{"--target", "1.36", "--plan", "--files", dir}, "--plan with --files needs --from"},
		{[]string{"--target", "1.36", "--plan", "--files", dir, "--from", "banana"}, `invalid --from "banana"`},
		{[]string{"--target", "1.36", "--plan", "--files", dir, "--from", "1.36"}, "--from 1.36 must be older than --target 1.36"},
		{[]string{"--target", "1.36", "--plan", "--output", "sarif"}, "--plan renders in table, markdown and json"},
		{[]string{"--target", "1.36", "--plan", "--output", "junit"}, "--plan renders in table, markdown and json"},
		{[]string{"--target", "1.36", "--plan", "--output", "gitlab-codequality"}, "--plan renders in table, markdown and json"},
	} {
		_, err := execScan(t, tc.args, planStub(t, planInventory()))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
}

// A live cluster whose kube-apiserver version is unknown has no plan: the
// scan says so and reports the target alone.
func TestScanPlanUnknownServerVersion(t *testing.T) {
	inv := liveInventory("")
	_, stderr, err := execScanStderr(t, []string{"--target", "1.36", "--plan", "--fail-on", "never"}, planStub(t, inv))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "warning: --plan: the cluster's kube-apiserver version is unknown, so there is no upgrade plan") {
		t.Errorf("stderr = %q", stderr)
	}
}
