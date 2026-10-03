package engine

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

type Category string

const (
	CatRemovedAPI         Category = "removed-api"
	CatDeprecatedAPI      Category = "deprecated-api"
	CatDeprecatedAPIInUse Category = "deprecated-api-in-use"
	CatEOLAddon           Category = "eol-addon"
	CatEOLApproaching     Category = "eol-approaching"
	CatVersionSkew        Category = "version-skew"
	CatChartIncompat      Category = "chart-incompat"
	CatKBStale            Category = "kb-stale"
	// CatAddOnNoData (info): a detected add-on whose version has no
	// lifecycle data, so its EOL and compatibility were not assessed.
	CatAddOnNoData Category = "addon-no-data"
	// CatUnknownAPI (info): objects of a built-in API group (core, or any
	// group k8s.io/api registers: KB.BuiltinGroups and the groups of its
	// entries) at a version/kind the KB does not know, so whether the
	// target serves it was not assessed. CRD and aggregated groups
	// produce nothing.
	CatUnknownAPI Category = "unknown-api"
	// CatCRDVersion: a CustomResourceDefinition version problem that
	// breaks an add-on upgrade, whatever the Kubernetes target: custom
	// resources at a version the CRD does not serve (blocker) or
	// deprecates (warning; info when none is in use), or a
	// status.storedVersions entry it no longer serves (warning).
	CatCRDVersion Category = "crd-version"
)

type Severity string

const (
	SevBlocker Severity = "blocker"
	SevWarning Severity = "warning"
	SevInfo    Severity = "info"
)

type Finding struct {
	Category Category `json:"category"`
	Severity Severity `json:"severity"`
	// Key is the finding's stable, count-free identity: it never embeds
	// object counts, node counts, or other volatile numbers, so the same
	// underlying problem keeps the same Key across snapshots even as Title
	// fluctuates ("3 objects" → "2 objects"). Notification delta diffing
	// keys on it. Convention: category + "/" + stable discriminator
	// (e.g. "removed-api/extensions/v1beta1/Ingress", "eol-addon/ingress-nginx").
	Key    string   `json:"key,omitempty"`
	Title  string   `json:"title"`           // one line, deterministic
	Detail string   `json:"detail"`          // evidence sentence(s), deterministic
	Teams  []string `json:"teams,omitempty"` // sorted, deduped; of every affected namespace, listed or not
	// Namespaces are the affected namespaces, sorted, at most
	// MaxFindingNamespaces; NamespacesOmitted counts the ones not listed.
	Namespaces        []string `json:"namespaces,omitempty"`
	NamespacesOmitted int      `json:"namespacesOmitted,omitempty"`
	Remediation       string   `json:"remediation,omitempty"`
	Citations         []string `json:"citations,omitempty"`
	// Objects identifies the affected objects for API-usage findings
	// (copied from inventory.APIUsage, so at most inventory.MaxObjectRefs),
	// sorted by file, line, namespace, name; ObjectsOmitted counts the
	// affected objects not listed. Empty when the collector could not
	// identify objects.
	Objects        []inventory.ObjectRef `json:"objects,omitempty"`
	ObjectsOmitted int                   `json:"objectsOmitted,omitempty"`
	// BaselineState is set only when the report was compared with a
	// baseline (scan --baseline): BaselineUnchanged when the baseline
	// already had this Key and every object listed here, else BaselineNew.
	// The CLI gate fails only on new findings; score and verdict count both.
	BaselineState BaselineState `json:"baselineState,omitempty"`
}

// BaselineState mirrors SARIF's result.baselineState values.
type BaselineState string

const (
	BaselineNew       BaselineState = "new"
	BaselineUnchanged BaselineState = "unchanged"
)

// SuppressedFinding is a finding, or some of its objects (Objects then
// lists only those), that an ignore rule or object annotation accepted.
// It is excluded from score and verdict but still reported, with why.
type SuppressedFinding struct {
	Finding
	Reason string `json:"reason"`
	// Source names what suppressed it: the config file, "annotation", or
	// "spec.ignore" (the agent).
	Source  string `json:"source"`
	Expires string `json:"expires,omitempty"` // YYYY-MM-DD, as the rule gave it
}

// CapabilityGap is one thing the evaluation could not assess. Capability is
// usually a collector capability whose sub-collector failed (Reason is its
// error). The engine also adds gaps of its own:
//
//   - capability "versions", when the inventory's versions capability is
//     available but the server version is missing or unparseable — the
//     kubelet and control-plane skew rules then had no reference version;
//   - capability "kb-coverage" (GapKBCoverage), when the target is newer
//     than the knowledge base's MaxKnownK8s — removals in releases the KB
//     does not know about cannot be found;
//   - capability "target" (GapTarget), when a cluster inventory's oldest
//     kube-apiserver already runs the target minor or a newer one — the
//     target is not an upgrade, and every check judges a newer minor.
//
// A capability that is available but Partial (it could not read all it
// covers) is a gap too, with Partial set and the capability's Skipped.
//
// Required marks gaps that make the verdict unknown, because a blocker may
// be behind them:
//
//   - api-usage, kb-coverage and target, always;
//   - versions, for cluster inventories;
//   - addons, for cluster inventories while the KB's add-on registry is
//     not empty: the EOL add-on check is a headline check, and files mode
//     has no running add-ons to detect;
//   - a partial addons, for cluster inventories, when Skipped names the pods
//     (inventory.SkippedPods): only Helm releases and IngressClasses were
//     read, and an add-on installed any other way goes undetected;
//   - a partial api-usage, when Skipped names an API the KB removes at or
//     before the target (an unchecked object of it would be a blocker);
//   - a partial versions, for cluster inventories, when Skipped names a
//     component whose version upstream would have told but was not read
//     (it may be the one past the skew policy), or "nodes", when no Node
//     was listed (a kubelet past the policy would be a blocker; a
//     forbidden Node list makes versions unavailable, required too).
//
// Other gaps only narrow what the report covers: a partial api-usage that
// skipped only APIs removed later or never (their findings are warnings
// or info) or none the KB flags, a partial versions naming no component (a
// vendor kube-proxy image, OKE's), deprecated-calls (managed control planes
// commonly deny /metrics), helm (Secrets are commonly denied; live objects
// are still checked by api-usage, and add-ons by their images).
type CapabilityGap struct {
	Capability inventory.Capability `json:"capability"`
	Reason     string               `json:"reason"`
	// Partial: the capability ran but did not read everything; Skipped is
	// what it named as unread (see inventory.CapabilityStatus).
	Partial  bool     `json:"partial,omitempty"`
	Skipped  []string `json:"skipped,omitempty"`
	Required bool     `json:"required,omitempty"`
}

// Label names the gap as every output renders it: the capability, with
// "partial" and "required" in parentheses when they apply, e.g.
// "api-usage (partial, required)".
func (g CapabilityGap) Label() string {
	var marks []string
	if g.Partial {
		marks = append(marks, "partial")
	}
	if g.Required {
		marks = append(marks, "required")
	}
	if len(marks) == 0 {
		return string(g.Capability)
	}
	return fmt.Sprintf("%s (%s)", g.Capability, strings.Join(marks, ", "))
}

// categorySources maps each finding category to the inventory
// capabilities its findings are built from: what a report must have
// assessed for a finding's absence to mean the problem is gone, unless
// the finding was itself found without one of them. Sources refines it
// by key; TestEveryCategoryHasSources keeps it complete.
//
// Add-on findings name {addons, versions, helm}: node container runtimes
// come from the nodes versions lists, and the add-ons collector detects
// add-ons from the Helm releases the helm step read too (chart evidence,
// whose app version is the install's), so an add-on found through its
// chart alone is missing while helm is not assessed. Helm-release
// findings are keyed category/helm-release/…, and come from helm alone
// (Sources). deprecated-api-in-use comes from the /metrics scrape only: a
// caller row that folds into its API's usage finding leaves no key of its
// own behind, and has one while api-usage does not see that API
// (FoldsInto).
var categorySources = map[Category][]inventory.Capability{
	CatRemovedAPI:         {inventory.CapAPIUsage},
	CatDeprecatedAPI:      {inventory.CapAPIUsage},
	CatUnknownAPI:         {inventory.CapAPIUsage},
	CatDeprecatedAPIInUse: {inventory.CapDeprecatedCalls},
	CatEOLAddon:           {inventory.CapAddOns, inventory.CapVersions, inventory.CapHelm},
	CatEOLApproaching:     {inventory.CapAddOns, inventory.CapVersions, inventory.CapHelm},
	CatAddOnNoData:        {inventory.CapAddOns, inventory.CapVersions, inventory.CapHelm},
	CatChartIncompat:      {inventory.CapAddOns, inventory.CapVersions, inventory.CapHelm},
	CatVersionSkew:        {inventory.CapVersions},
	CatCRDVersion:         {inventory.CapCRDs},
	CatKBStale:            {}, // the knowledge base's own date
}

// Sources lists the inventory capabilities a finding of category c and
// key is built from; nil for a category this binary does not know.
func Sources(c Category, key string) []inventory.Capability {
	if strings.HasPrefix(key, string(c)+"/helm-release/") {
		return []inventory.Capability{inventory.CapHelm}
	}
	return categorySources[c]
}

// HiddenBy lists the capabilities of gaps, a report's NotAssessed, that
// leave the finding of category c and key unassessed, in gaps' order: a
// report with any could not have produced the finding. A gap hides it
// when its capability is one of the finding's Sources and is
// unavailable, or partial with Skipped naming what the finding is about.
// A partial capability's Skipped is what it did not read, so:
//
//   - api-usage and deprecated-calls leave unassessed the APIs they name
//     ("group/version Kind", "group/version resource");
//   - helm leaves unassessed the releases it names ("namespace/name"),
//     and every release when it names anything without a slash: a
//     storage driver, or a GitOps tool ("argocd", "flux"). A GitOps tool
//     is broader than it need be: its gap concerns the charts it deploys,
//     not the releases read fine from Secrets, which it hides findings of
//     too, since a release's key does not say which tool deployed it. It
//     also leaves unassessed every add-on when it names anything, since
//     an add-on's key does not say which release, if any, it was found
//     through;
//   - any other capability that names something leaves all of its
//     findings unassessed;
//   - a partial capability that names nothing read everything that could
//     have produced a finding.
//
// Every gap hides a finding of a category this binary does not know.
func HiddenBy(gaps []CapabilityGap, c Category, key string) []inventory.Capability {
	sources := Sources(c, key)
	var out []inventory.Capability
	for _, g := range gaps {
		hides := sources == nil || slices.Contains(sources, g.Capability) && (!g.Partial || g.skips(key))
		if hides && !slices.Contains(out, g.Capability) {
			out = append(out, g.Capability)
		}
	}
	return out
}

// skips reports whether a partial gap's Skipped names what key is about.
func (g CapabilityGap) skips(key string) bool {
	switch g.Capability {
	case inventory.CapAPIUsage, inventory.CapDeprecatedCalls:
		_, tail, _ := strings.Cut(key, "/") // category/group/version/name
		return slices.ContainsFunc(g.Skipped, func(s string) bool {
			group, version, name, ok := splitAPI(s)
			return ok && apiKey(group, version, name) == tail
		})
	case inventory.CapHelm:
		if !strings.Contains(key, "/helm-release/") {
			return len(g.Skipped) > 0 // an add-on finding
		}
		return slices.ContainsFunc(g.Skipped, func(s string) bool {
			// no slash: a storage driver or a GitOps tool
			return !strings.Contains(s, "/") || strings.HasSuffix(key, "/helm-release/"+s)
		})
	}
	return len(g.Skipped) > 0
}

// FoldsInto reports whether call, the key of a deprecated-api-in-use
// finding, names the API of usage, the key of an API usage finding
// (removed-api, deprecated-api, unknown-api): the match
// foldDeprecatedCalls folds a caller row by. A report with both findings
// has usage's key alone, so call's is a finding of its own in a report
// whose api-usage did not see that API, without anything having changed.
func FoldsInto(call, usage string) bool {
	cat, tail, _ := strings.Cut(usage, "/")
	switch Category(cat) {
	case CatRemovedAPI, CatDeprecatedAPI, CatUnknownAPI:
	default:
		return false
	}
	u := strings.Split(tail, "/") // group/version/Kind
	cat, tail, _ = strings.Cut(call, "/")
	c := strings.SplitN(tail, "/", 4) // group/version/resource[/subresource]
	return Category(cat) == CatDeprecatedAPIInUse && len(u) == 3 && u[0] != "helm-release" && len(c) >= 3 &&
		c[0] == u[0] && c[1] == u[1] && kindMatchesResource(u[2], c[2])
}

// splitAPI splits an API as a partial capability names it in Skipped,
// "group/version name" (core "v1 name"), into its parts.
func splitAPI(api string) (group, version, name string, ok bool) {
	gv, name, ok := strings.Cut(api, " ")
	if !ok {
		return "", "", "", false
	}
	version = gv
	if i := strings.LastIndex(gv, "/"); i >= 0 {
		group, version = gv[:i], gv[i+1:]
	}
	return group, version, name, true
}

// GapKBCoverage is the CapabilityGap capability recorded when the target is
// beyond the knowledge base horizon. It is not a collector capability.
const GapKBCoverage inventory.Capability = "kb-coverage"

// GapTarget is the CapabilityGap capability recorded when the target is not
// an upgrade of the cluster. It is not a collector capability.
const GapTarget inventory.Capability = "target"

// Verdict is the report's readiness answer.
type Verdict string

const (
	// VerdictReady: no blockers, and everything required was assessed.
	VerdictReady Verdict = "ready"
	// VerdictBlocked: at least one blocker finding.
	VerdictBlocked Verdict = "blocked"
	// VerdictUnknown: no blockers found, but a required gap (see
	// CapabilityGap.Required) means blockers may have been missed.
	VerdictUnknown Verdict = "unknown"
)

type Report struct {
	ClusterID string            `json:"clusterId"`
	Target    inventory.Version `json:"target"`
	// ServerVersion is the inventory's raw kube-apiserver GitVersion, the
	// version the target was judged against; empty in files mode.
	ServerVersion string `json:"serverVersion,omitempty"`
	// KubeContext and APIServer say which cluster a live scan read: the
	// kubeconfig context and the API server URL (scheme, host and port;
	// no credentials, path or query). Evaluate never sets them; the scan
	// command does. Empty in files mode.
	KubeContext string `json:"kubeContext,omitempty"`
	APIServer   string `json:"apiServer,omitempty"`
	KBVersion   string `json:"kbVersion"`
	Score       int    `json:"score"` // from findings only; see Score
	// Ready is Verdict == VerdictReady, kept for v0.1 consumers.
	Ready       bool            `json:"ready"`
	Verdict     Verdict         `json:"verdict"`
	Findings    []Finding       `json:"findings"`              // sorted: severity desc, category, title
	NotAssessed []CapabilityGap `json:"notAssessed,omitempty"` // sorted by capability
	// Suppressed lists what ignore rules took out of Findings (see
	// internal/suppress); Evaluate never sets it.
	Suppressed []SuppressedFinding `json:"suppressed,omitempty"`
	// UnrecognizedImages are the image repositories no registry image
	// matcher claims (inventory.UnrecognizedImages), so an add-on
	// detection gap is visible; never findings, so they change neither
	// score nor verdict. Sorted, deduplicated, at most
	// inventory.MaxUnrecognizedImages; UnrecognizedImagesOmitted counts
	// the ones the cap dropped.
	UnrecognizedImages        []string `json:"unrecognizedImages,omitempty"`
	UnrecognizedImagesOmitted int      `json:"unrecognizedImagesOmitted,omitempty"`
	// Hops is the upgrade plan up to Target (see Plan), set by scan
	// --plan; Evaluate never sets it. The rest of the report is the
	// final target's.
	Hops []Hop `json:"hops,omitempty"`
}

// Rescore recomputes Score, Verdict and Ready from Findings and
// NotAssessed, for callers that remove findings after Evaluate.
func (r *Report) Rescore() {
	r.Score, _ = Score(r.Findings)
	r.Verdict = verdictFor(r.Findings, r.NotAssessed)
	r.Ready = r.Verdict == VerdictReady
}

var severityRank = map[Severity]int{SevBlocker: 0, SevWarning: 1, SevInfo: 2}

// sortFindings orders findings deterministically: severity (blocker > warning
// > info), then category, title and key (lexical), so two findings with one
// title keep their order whatever order they were found in.
func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		if severityRank[fs[i].Severity] != severityRank[fs[j].Severity] {
			return severityRank[fs[i].Severity] < severityRank[fs[j].Severity]
		}
		if fs[i].Category != fs[j].Category {
			return fs[i].Category < fs[j].Category
		}
		if fs[i].Title != fs[j].Title {
			return fs[i].Title < fs[j].Title
		}
		return fs[i].Key < fs[j].Key
	})
}
