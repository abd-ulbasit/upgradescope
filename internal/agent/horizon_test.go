package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	metadatafake "k8s.io/client-go/metadata/fake"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
)

// VS-11 (#130): a default ClusterReadiness (no spec.targets) on a cluster
// already at the knowledge base's horizon is judged at the next minor,
// which the KB does not cover. With nothing else missing and no blocker,
// that gap alone makes the verdict unknown: status.notAssessed names
// kb-coverage and the Ready condition is Unknown, naming it required.
// hack/e2e.sh proves the same on kind's newest minor.
func TestTickDefaultTargetPastKBHorizonIsUnknown(t *testing.T) {
	k := mustKB(t)
	horizon := k.MaxKnownK8s
	clients := fakeClients(t, fmt.Sprintf("v%s.0", horizon))
	scheme := runtime.NewScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	clients.Metadata = metadatafake.NewSimpleMetadataClient(scheme) // api-usage assessed: nothing uses a flagged API
	dyn := fakeDyn()
	cfg := Config{}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	r := newRunner(clients, dyn, k, cfg)
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(r.last.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(r.last.reports))
	}
	var required []string
	for _, g := range r.last.reports[0].NotAssessed {
		if g.Required {
			required = append(required, string(g.Capability))
		}
	}
	if !slices.Equal(required, []string{"kb-coverage"}) {
		t.Fatalf("required gaps = %v, want only kb-coverage: the verdict must be unknown because of the horizon alone", required)
	}

	st := readCRStatus(t, dyn, crd.DefaultName)
	want := horizon.Next().String()
	if len(st.Targets) != 1 || st.Targets[0].Target != want {
		t.Fatalf("Targets = %+v, want the default next minor %s", st.Targets, want)
	}
	if got := st.Targets[0]; got.Verdict != "unknown" || got.Ready || got.Blockers != 0 {
		t.Errorf("target %s: verdict %q ready %v blockers %d, want unknown/false/0", want, got.Verdict, got.Ready, got.Blockers)
	}
	if !slices.ContainsFunc(st.NotAssessed, func(s string) bool { return strings.HasPrefix(s, "kb-coverage: ") }) {
		t.Errorf("notAssessed = %q, want a kb-coverage entry", st.NotAssessed)
	}
	i := slices.IndexFunc(st.Conditions, func(c metav1.Condition) bool { return c.Type == crd.ConditionReady })
	if i < 0 {
		t.Fatalf("conditions = %+v, want Ready", st.Conditions)
	}
	if c := st.Conditions[i]; c.Status != metav1.ConditionUnknown || c.Reason != crd.ReasonNotAssessed ||
		!strings.Contains(c.Message, "kb-coverage (required)") {
		t.Errorf("Ready = %s/%s %q, want Unknown/%s naming kb-coverage (required)", c.Status, c.Reason, c.Message, crd.ReasonNotAssessed)
	}
}
