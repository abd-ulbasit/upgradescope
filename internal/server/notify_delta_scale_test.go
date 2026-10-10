package server

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// largeDelta is #241's computeDelta case: n carried findings of a
// capability the pass did not assess (API usage findings, which the
// carried check folds callers into, so every one is a candidate), and n
// new deprecated-caller blockers, none of which folds into one.
func largeDelta(t testing.TB, n int) ([]findingHead, engine.Report, func(findingHead) bool) {
	prev := make([]findingHead, n)
	findings := make([]engine.Finding, n)
	for i := range n {
		prev[i] = findingHead{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker,
			Key: fmt.Sprintf("removed-api/example.dev/v1beta1/Kind%d", i)}
		findings[i] = engine.Finding{Category: engine.CatDeprecatedAPIInUse, Severity: engine.SevBlocker,
			Key: fmt.Sprintf("deprecated-api-in-use/other.dev/v1/res%d", i), Title: fmt.Sprintf("caller %d", i)}
	}
	target, err := inventory.ParseVersion("1.36")
	if err != nil {
		t.Fatal(err)
	}
	curr := engine.Report{Target: target, Verdict: engine.VerdictBlocked, Findings: findings}
	return prev, curr, func(findingHead) bool { return true }
}

// TestComputeDeltaIsLinearInCarriedFindings (#241): every new blocker was
// compared with every carried finding (engine.FoldsInto), about 46 s at
// 20,000 x 20,000 of this fixture on an arm64 Mac (docs/claims.md NT-07),
// inside the ingest slot. Indexed by the API a caller
// folds by, it is one lookup per blocker.
func TestComputeDeltaIsLinearInCarriedFindings(t *testing.T) {
	prev, curr, unassessed := largeDelta(t, 20000)
	start := time.Now()
	changes, carried := computeDelta(prev, curr, unassessed)
	took := time.Since(start)
	if len(carried) != 20000 || len(changes) != 20000 {
		t.Fatalf("carried %d, changes %d, want 20000 of each", len(carried), len(changes))
	}
	t.Logf("computeDelta 20,000 x 20,000: %v", took)
	if took > time.Second {
		t.Errorf("computeDelta 20,000 x 20,000 took %v, want well under 1s", took)
	}
}

func BenchmarkComputeDelta20k(b *testing.B) {
	prev, curr, unassessed := largeDelta(b, 20000)
	for b.Loop() {
		computeDelta(prev, curr, unassessed)
	}
}

// TestCarriedFoldMatchesFoldsInto: the index answers exactly what
// comparing a caller with every carried finding through engine.FoldsInto
// answered, plural forms, case, subresources, non-usage findings and the
// key of a not-served-yet finding (removed-api/.../unserved, #300)
// included.
func TestCarriedFoldMatchesFoldsInto(t *testing.T) {
	var carried []findingHead
	for _, k := range []string{
		"removed-api/policy/v1beta1/PodSecurityPolicy",
		"deprecated-api/flowcontrol.apiserver.k8s.io/v1beta3/FlowSchema",
		"unknown-api/example.dev/v1/Gateway",
		"removed-api/example.dev/v1/Ingress",
		"removed-api/example.dev/v1/Policy",
		"removed-api/example.dev/v1/Day",
		"removed-api/example.dev/v1/STATUS",
		"removed-api/helm-release/ns/release",
		"removed-api/helm-release/ns/unserved",                       // a release named so, not a phase
		"removed-api/networking.k8s.io/v1beta1/ServiceCIDR/unserved", // #300: not served yet, the API is the base key's
		"removed-api/example.dev/v1/Day/other",                       // four parts that are no phase: no API
		"removed-api/core/v1",
		"chart-incompat/helm-release/ns/r",
		"eol-addon/ingress-nginx",
		"",
	} {
		carried = append(carried, findingHead{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Key: k, Title: "t"})
	}
	idx := newCarriedFolds(carried)
	for _, call := range []string{
		"deprecated-api-in-use/policy/v1beta1/podsecuritypolicies",
		"deprecated-api-in-use/flowcontrol.apiserver.k8s.io/v1beta3/flowschemas/status",
		"deprecated-api-in-use/example.dev/v1/gateways",
		"deprecated-api-in-use/example.dev/v1/gateway",
		"deprecated-api-in-use/example.dev/v1/ingresses",
		"deprecated-api-in-use/example.dev/v1/ingresss",
		"deprecated-api-in-use/example.dev/v1/policies",
		"deprecated-api-in-use/example.dev/v1/policys",
		"deprecated-api-in-use/example.dev/v1/days",
		"deprecated-api-in-use/example.dev/v1/daies",
		"deprecated-api-in-use/example.dev/v1/statuses",
		"deprecated-api-in-use/example.dev/v2/gateways",
		"deprecated-api-in-use/networking.k8s.io/v1beta1/servicecidrs",
		"deprecated-api-in-use/networking.k8s.io/v1beta1/servicecidrs/status",
		"deprecated-api-in-use/helm-release/ns/unserved",
		"deprecated-api-in-use/helm-release/ns/release",
		"deprecated-api-in-use/core/v1",
		"removed-api/policy/v1beta1/PodSecurityPolicy",
		"deprecated-api-in-use",
		"",
	} {
		want := slices.ContainsFunc(carried, func(h findingHead) bool { return engine.FoldsInto(call, h.key()) })
		if got := idx.folds(call); got != want {
			t.Errorf("folds(%q) = %v, FoldsInto over every carried finding = %v", call, got, want)
		}
	}
	if newCarriedFolds(nil).folds("deprecated-api-in-use/policy/v1beta1/podsecuritypolicies") {
		t.Error("an empty index folds a caller")
	}
}

// TestComputeDeltaFoldsCallerIntoCarriedUsage keeps the carried-fold
// behaviour (NT-01): a caller of an API whose usage finding is carried is
// that finding, not news; a caller of another API is.
func TestComputeDeltaFoldsCallerIntoCarriedUsage(t *testing.T) {
	prev := []findingHead{{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Key: "removed-api/policy/v1beta1/PodSecurityPolicy"}}
	curr := rep(t, 50,
		keyedBlocker("deprecated-api-in-use/policy/v1beta1/podsecuritypolicies", "psp caller"),
		keyedBlocker("deprecated-api-in-use/batch/v1beta1/cronjobs", "cronjob caller"))
	changes, carried := computeDelta(prev, curr, func(findingHead) bool { return true })
	if len(carried) != 1 {
		t.Fatalf("carried = %+v, want the PSP usage finding", carried)
	}
	var titles []string
	for _, c := range changes {
		titles = append(titles, c.Title)
	}
	if !reflect.DeepEqual(titles, []string{"cronjob caller"}) {
		t.Errorf("changes = %v, want only the cronjob caller announced", titles)
	}
}
