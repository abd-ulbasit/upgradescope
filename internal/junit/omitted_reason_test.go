package junit

import (
	"fmt"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// The cap is the omitted objects' reason only when the finding reached it
// (#362): below inventory.MaxObjectRefs listed, the omitted objects are
// ones the finding never names, such as a cluster's pods beside its
// PersistentVolumes, and they are counted with no reason.
func TestFindingTextCapReasonOnlyAtTheCap(t *testing.T) {
	for _, tc := range []struct {
		listed int
		want   string
	}{
		{1, "\n- and 2 more\n"},
		{inventory.MaxObjectRefs - 1, "\n- and 2 more\n"},
		{inventory.MaxObjectRefs, "\n- and 2 more (at most 100 objects are recorded per finding)\n"},
	} {
		objs := make([]inventory.ObjectRef, tc.listed)
		for i := range objs {
			objs[i] = inventory.ObjectRef{Name: fmt.Sprintf("o%d", i)}
		}
		got := findingText(engine.Finding{Title: "In-tree volume plugin rbd removed in 1.31", Objects: objs, ObjectsOmitted: 2, Remediation: "x"})
		if !strings.Contains(got, tc.want) {
			t.Errorf("%d listed: want %q in\n%s", tc.listed, tc.want, got)
		}
	}
}
