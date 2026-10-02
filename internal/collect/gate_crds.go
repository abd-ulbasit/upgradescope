package collect

import "github.com/abd-ulbasit/upgradescope/internal/inventory"

// AssessCRDs judges the custom resources in inv.APIUsage against crds as
// files mode judges a render against the CRDs in it (assessCRDs): a later
// definition of a group and kind replaces an earlier one, inv.CRDs gets
// the result, each with the custom resources at a version it deprecates,
// does not serve or does not list as its Usage, and the crds capability is
// partial, naming them, when custom resources have no CRD among crds.
// inv.Capabilities must not be nil. The server gate judges posted
// manifests with it against a cluster's CRDs merged with the manifests'
// own (#150).
func AssessCRDs(inv *inventory.Inventory, crds []inventory.CRD) {
	assessCRDs(inv, crds)
}
