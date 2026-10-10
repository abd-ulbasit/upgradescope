package inventory

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/validate/content"
)

// The Kubernetes identifiers an inventory carries are read by a collector
// from objects the apiserver validated, so in an inventory a server is
// sent they are valid Kubernetes identifiers, or the inventory is not a
// genuine one. The server refuses those (422) before storing anything:
// identifiers are what the reports it serves repeat, and a namespace
// "name" of 190 apostrophes, listed in every finding's namespaces and
// evidence, made a 21 MB push three 42 MB reports and a 200 MB HTML
// export.
//
// An empty identifier is accepted everywhere: it means unset (a
// cluster-scoped object's namespace, an object a collector could not
// name).
//
// The rules come from k8s.io/apimachinery/pkg/api/validate/content, the
// apiserver's own: in this apimachinery k8s.io/apimachinery/pkg/util/validation
// marks its IsQualifiedName, IsValidLabelValue and the like deprecated in
// favour of content's, and delegates to it, and IsPathSegmentName (the
// rule RBAC names follow) is only in content.

// Identifier rules, as the apiserver applies them.
const (
	// ruleNamespace: metadata.namespace and a Namespace's name.
	ruleNamespace = "a namespace name: an RFC 1123 label of at most 63 bytes"
	// ruleObjectName: an object's metadata.name. Kinds differ (most
	// require an RFC 1123 subdomain; RBAC kinds take any path segment
	// name, "system:node:x" included), so this is the most permissive
	// rule any kind has, the path segment rule, capped at the 253 bytes
	// an RFC 1123 subdomain may take.
	ruleObjectName = "an object name: at most 253 bytes, not '.' or '..', without '/' or '%'"
	// ruleSubdomain: a Node's name, a Helm release's name (its storage
	// Secret is named after it) and a cluster name.
	ruleSubdomain = "an RFC 1123 subdomain of at most 253 bytes"
	// ruleLabelValue: a label value, as the team label's is.
	ruleLabelValue = "a label value: at most 63 bytes"
	// ruleProvider: Inventory.Provider is bounded like a label value, not a
	// closed vocabulary: a newer agent may name a provider this build does
	// not know (the evaluation then has no calendar for it, as for other),
	// and the snapshot must still be accepted.
	ruleProvider = "a provider name: at most 63 bytes of letters, digits, '-', '_' or '.'"
)

// MaxObjectNameBytes caps an object name in an inventory: the most an
// RFC 1123 subdomain, which almost every kind's name must be, may take.
const MaxObjectNameBytes = 253

// IdentifierError is an identifier in an inventory, or a cluster name,
// that is not valid for what it names.
type IdentifierError struct {
	Field    string   // JSON path within the inventory, e.g. "apiUsage[3].namespaces"
	Value    string   // the identifier
	Rule     string   // what it must be
	Problems []string // why it is not, as k8s.io/apimachinery says
}

func (e *IdentifierError) Error() string {
	return fmt.Sprintf("%s: %s is not %s (%s)", e.Field, quoteShort(e.Value), e.Rule, strings.Join(e.Problems, "; "))
}

// Unquoted says which field holds an identifier that is not what it must
// be, and nothing of the identifier: what may be shown where the
// inventory's contents must not be. Field holds no map key.
func (e *IdentifierError) Unquoted() string { return e.Field + " is not " + e.Rule }

// quoteShort quotes at most the first 64 bytes of s, saying how long it
// is when it is longer: an identifier refused for its length can be
// megabytes.
func quoteShort(s string) string {
	const show = 64
	if len(s) <= show {
		return fmt.Sprintf("%q", s)
	}
	cut := show
	for cut > 0 && s[cut]&0xC0 == 0x80 { // not inside a UTF-8 sequence
		cut--
	}
	return fmt.Sprintf("%q… (%d bytes)", s[:cut], len(s))
}

func namespaceProblems(s string) []string { return content.IsDNS1123Label(s) }

func subdomainProblems(s string) []string { return content.IsDNS1123Subdomain(s) }

func labelValueProblems(s string) []string { return content.IsLabelValue(s) }

func objectNameProblems(s string) []string {
	problems := content.IsPathSegmentName(s)
	if len(s) > MaxObjectNameBytes {
		problems = append(problems, content.MaxLenError(MaxObjectNameBytes))
	}
	return problems
}

// problemsWith returns what is wrong with a non-empty value, or nil.
func problemsWith(value string, problems func(string) []string) []string {
	if value == "" {
		return nil
	}
	return problems(value)
}

// ValidateClusterName checks a cluster name, as an agent sends it and
// the server registers it: an RFC 1123 subdomain of at most 253 bytes,
// as a Kubernetes object's name usually is. It must not be empty.
func ValidateClusterName(name string) error {
	p := subdomainProblems(name)
	if name == "" {
		p = []string{content.EmptyError()}
	}
	if len(p) > 0 {
		return &IdentifierError{Field: "clusterName", Value: name, Rule: ruleSubdomain, Problems: p}
	}
	return nil
}

// ValidateIdentifiers checks every Kubernetes identifier in inv against
// the rule the apiserver applies to it, and returns the first that fails
// as an *IdentifierError: namespace names (API usage namespaces and object
// refs, Helm releases, add-on installs, the namespace list), object names,
// node names (a kube-proxy's too) and Helm release names, and team label
// values, and the provider name, bounded as a label value (an unknown
// provider is accepted: see ruleProvider). Fields that are not
// identifiers (versions, image repositories, field managers, annotation
// values) are not checked.
func (inv Inventory) ValidateIdentifiers() error {
	if p := problemsWith(string(inv.Provider), labelValueProblems); p != nil {
		return &IdentifierError{Field: "provider", Value: string(inv.Provider), Rule: ruleProvider, Problems: p}
	}
	for i, u := range inv.APIUsage {
		if err := validateUsage(func() string { return fmt.Sprintf("apiUsage[%d]", i) }, u); err != nil {
			return err
		}
	}
	for i, u := range inv.APIAuthorshipUnknown {
		if err := validateUsage(func() string { return fmt.Sprintf("apiAuthorshipUnknown[%d]", i) }, u); err != nil {
			return err
		}
	}
	for i, r := range inv.HelmReleases {
		at := func() string { return fmt.Sprintf("helmReleases[%d]", i) }
		if p := problemsWith(r.Name, subdomainProblems); p != nil {
			return &IdentifierError{Field: at() + ".name", Value: r.Name, Rule: ruleSubdomain, Problems: p}
		}
		if p := problemsWith(r.Namespace, namespaceProblems); p != nil {
			return &IdentifierError{Field: at() + ".namespace", Value: r.Namespace, Rule: ruleNamespace, Problems: p}
		}
		for j, u := range r.ManifestAPIs {
			if err := validateUsage(func() string { return fmt.Sprintf("%s.manifestApis[%d]", at(), j) }, u); err != nil {
				return err
			}
		}
	}
	for i, c := range inv.GitOpsCharts {
		at := func() string { return fmt.Sprintf("gitopsCharts[%d]", i) }
		if p := problemsWith(c.Name, subdomainProblems); p != nil {
			return &IdentifierError{Field: at() + ".name", Value: c.Name, Rule: ruleSubdomain, Problems: p}
		}
		if p := problemsWith(c.Namespace, namespaceProblems); p != nil {
			return &IdentifierError{Field: at() + ".namespace", Value: c.Namespace, Rule: ruleNamespace, Problems: p}
		}
		if p := problemsWith(c.Target, namespaceProblems); p != nil {
			return &IdentifierError{Field: at() + ".target", Value: c.Target, Rule: ruleNamespace, Problems: p}
		}
	}
	for i, a := range inv.AddOns {
		for j, ns := range a.Namespaces {
			if p := problemsWith(ns, namespaceProblems); p != nil {
				return &IdentifierError{Field: fmt.Sprintf("addOns[%d].namespaces[%d]", i, j), Value: ns, Rule: ruleNamespace, Problems: p}
			}
		}
	}
	for i, n := range inv.Nodes {
		if p := problemsWith(n.Name, subdomainProblems); p != nil {
			return &IdentifierError{Field: fmt.Sprintf("nodes[%d].name", i), Value: n.Name, Rule: ruleSubdomain, Problems: p}
		}
	}
	for i, cv := range inv.ControlPlane {
		if p := problemsWith(cv.Node, subdomainProblems); cv.Node != "" && p != nil {
			return &IdentifierError{Field: fmt.Sprintf("controlPlane[%d].node", i), Value: cv.Node, Rule: ruleSubdomain, Problems: p}
		}
	}
	for i, n := range inv.Namespaces {
		if p := problemsWith(n.Name, namespaceProblems); p != nil {
			return &IdentifierError{Field: fmt.Sprintf("namespaces[%d].name", i), Value: n.Name, Rule: ruleNamespace, Problems: p}
		}
		if p := problemsWith(n.Team, labelValueProblems); p != nil {
			return &IdentifierError{Field: fmt.Sprintf("namespaces[%d].team", i), Value: n.Team, Rule: ruleLabelValue, Problems: p}
		}
	}
	for i, c := range inv.CRDs {
		for j, u := range c.Usage {
			if err := validateUsage(func() string { return fmt.Sprintf("crds[%d].usage[%d]", i, j) }, u); err != nil {
				return err
			}
		}
	}
	return inv.validateVolumeIdentifiers()
}

// validateUsage checks one APIUsage's namespace keys and object refs; at
// names it. Of several invalid namespace keys the least is reported, so
// the message does not depend on map order.
func validateUsage(at func() string, u APIUsage) error {
	var bad *IdentifierError
	for ns := range u.Namespaces {
		if bad != nil && ns >= bad.Value {
			continue
		}
		if p := problemsWith(ns, namespaceProblems); p != nil {
			bad = &IdentifierError{Value: ns, Rule: ruleNamespace, Problems: p}
		}
	}
	if bad != nil {
		bad.Field = at() + ".namespaces"
		return bad
	}
	for k, o := range u.Objects {
		if p := problemsWith(o.Namespace, namespaceProblems); p != nil {
			return &IdentifierError{Field: fmt.Sprintf("%s.objects[%d].namespace", at(), k), Value: o.Namespace, Rule: ruleNamespace, Problems: p}
		}
		if p := problemsWith(o.Name, objectNameProblems); p != nil {
			return &IdentifierError{Field: fmt.Sprintf("%s.objects[%d].name", at(), k), Value: o.Name, Rule: ruleObjectName, Problems: p}
		}
	}
	return nil
}
