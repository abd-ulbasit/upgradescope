package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// manyTargets lists n distinct minors starting at 1.36.
func manyTargets(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("1.%d", 36+i)
	}
	return out
}

// A CR written before the schema bounded spec.targets can list any number.
// The agent evaluates the first crd.MaxTargets (deduped, in spec order) and
// says in one note how many it left out.
func TestResolveTargetsCapsAtMaxTargets(t *testing.T) {
	spec := crd.Spec{Targets: append([]string{"1.36", "1.36", "latest"}, manyTargets(30)...)}
	targets, notes, err := resolveTargets(spec, inventory.Inventory{ServerVersion: "v1.35.2"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range targets {
		got = append(got, v.String())
	}
	if want := manyTargets(crd.MaxTargets); !slices.Equal(got, want) {
		t.Errorf("targets = %v, want the first %d distinct valid ones %v", got, crd.MaxTargets, want)
	}
	var cut []string
	for _, n := range notes {
		if strings.Contains(n, "not assessed") {
			cut = append(cut, n)
		}
	}
	// 30 distinct valid minors, 8 kept: 22 left out. The repeats of 1.36 and
	// "latest" have their own skip notes and do not count.
	if len(cut) != 1 || !strings.HasPrefix(cut[0], "targets: 22 spec targets not assessed") {
		t.Errorf("notes = %q, want one targets note counting the 22 left out", notes)
	}
}

// Invalid and duplicate entries are noted one by one only up to a point;
// past it one line counts the rest, so notes do not grow with the spec.
func TestResolveTargetsBoundsSkipNotes(t *testing.T) {
	var raw []string
	for i := 0; i < 500; i++ {
		raw = append(raw, fmt.Sprintf("junk-%d-%s", i, strings.Repeat("z", 1000)))
	}
	targets, notes, err := resolveTargets(crd.Spec{Targets: append(raw, "1.37")}, inventory.Inventory{ServerVersion: "v1.35.2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Errorf("targets = %v, want just 1.37", targets)
	}
	if len(notes) > maxSkipNotes+1 {
		t.Errorf("%d notes for 500 invalid targets, want at most %d", len(notes), maxSkipNotes+1)
	}
	if last := notes[len(notes)-1]; !strings.Contains(last, fmt.Sprint(500-maxSkipNotes)) {
		t.Errorf("last note = %q, want the count of the %d it did not list", last, 500-maxSkipNotes)
	}
}

// --targets lands in spec.targets, where the schema allows MaxTargets: more
// would be rejected by the apiserver on every tick, so startup refuses it.
func TestConfigRejectsTooManyTargets(t *testing.T) {
	cfg := Config{Targets: manyTargets(crd.MaxTargets + 1)}
	if err := cfg.applyDefaults(); err == nil || !strings.Contains(err.Error(), fmt.Sprint(crd.MaxTargets)) {
		t.Fatalf("err = %v, want one naming the limit %d", err, crd.MaxTargets)
	}
	cfg = Config{Targets: manyTargets(crd.MaxTargets)}
	if err := cfg.applyDefaults(); err != nil {
		t.Errorf("exactly %d targets: %v", crd.MaxTargets, err)
	}
}

// The #191 repro: 1000 targets in the spec used to produce a ~4 MB status
// the apiserver rejected on every tick. Now crd.MaxTargets are evaluated, the
// write succeeds, and the rest is reported as not assessed.
func TestTickWithThousandSpecTargetsWritesBoundedStatus(t *testing.T) {
	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": crd.Group + "/" + crd.Version,
		"kind":       crd.Kind,
		"metadata":   map[string]interface{}{"name": "t1000"},
		"spec":       map[string]interface{}{"targets": toInterfaces(manyTargets(1000))},
	}}
	dyn := fakeDyn(cr)
	r := testRunner(t, dyn, "")
	r.cfg.CRName = "t1000"
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	st := readCRStatus(t, dyn, "t1000")
	var got []string
	for _, ts := range st.Targets {
		got = append(got, ts.Target)
	}
	if want := manyTargets(crd.MaxTargets); !slices.Equal(got, want) {
		t.Errorf("status targets = %v, want the first %d: %v", got, crd.MaxTargets, want)
	}
	if len(r.last.reports) != crd.MaxTargets {
		t.Errorf("evaluated %d targets, want %d", len(r.last.reports), crd.MaxTargets)
	}
	// The note leads, so a long list of capability gaps cannot fold it into
	// the "… and N more" line of the notAssessed bound.
	if n := st.NotAssessed[0]; !strings.HasPrefix(n, "targets: ") || !strings.Contains(n, "992 ") || !strings.Contains(n, "not assessed") {
		t.Errorf("notAssessed = %q, want a targets note first, counting the 992 entries left out", st.NotAssessed)
	}
	obj, err := dyn.Resource(crd.GVR()).Get(context.Background(), "t1000", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(obj.Object)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= 1<<20 {
		t.Errorf("stored CR is %d bytes, want under 1 MiB", len(raw))
	}
}

func toInterfaces(ss []string) []interface{} {
	out := make([]interface{}, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
