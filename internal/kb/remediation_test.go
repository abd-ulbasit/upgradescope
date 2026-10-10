package kb

import (
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func ver(minor int) *inventory.Version { return &inventory.Version{Major: 1, Minor: minor} }

func TestResolveReplacement(t *testing.T) {
	fs := func(v string, removed *inventory.Version, repl string) APILifecycleEntry {
		e := APILifecycleEntry{Group: "flowcontrol.apiserver.k8s.io", Version: v, Kind: "FlowSchema", Removed: removed}
		if repl != "" {
			e.Replacement = &GVK{Group: "flowcontrol.apiserver.k8s.io", Version: repl, Kind: "FlowSchema"}
		}
		return e
	}
	idx := NewIndex([]APILifecycleEntry{
		fs("v1beta1", ver(26), "v1beta3"),
		fs("v1beta3", ver(32), "v1"),
		fs("v1", nil, ""),
		// replacement names a GVK the KB does not know
		{Group: "x", Version: "v1beta1", Kind: "A", Removed: ver(20), Replacement: &GVK{Group: "x", Version: "v1", Kind: "A"}},
		// dead end: replacement itself removed with nowhere to go
		{Group: "y", Version: "v1alpha1", Kind: "B", Removed: ver(20), Replacement: &GVK{Group: "y", Version: "v1beta1", Kind: "B"}},
		{Group: "y", Version: "v1beta1", Kind: "B", Removed: ver(25)},
		// cycle
		{Group: "z", Version: "v1", Kind: "C", Removed: ver(20), Replacement: &GVK{Group: "z", Version: "v2", Kind: "C"}},
		{Group: "z", Version: "v2", Kind: "C", Removed: ver(20), Replacement: &GVK{Group: "z", Version: "v1", Kind: "C"}},
		// VolumeAttributesClass: v1alpha1 names v1 (1.34), v1beta1 (1.31) between
		{Group: "s", Version: "v1alpha1", Kind: "V", Introduced: *ver(29), Removed: ver(35), Replacement: &GVK{Group: "s", Version: "v1", Kind: "V"}},
		{Group: "s", Version: "v1beta1", Kind: "V", Introduced: *ver(31), Removed: ver(37), Replacement: &GVK{Group: "s", Version: "v1", Kind: "V"}},
		{Group: "s", Version: "v1", Kind: "V", Introduced: *ver(34)},
		// ClusterTrustBundle: v1beta1 names v1 (1.37); only the older v1alpha1 is served before
		{Group: "c", Version: "v1alpha1", Kind: "T", Introduced: *ver(26), Removed: ver(37)},
		{Group: "c", Version: "v1beta1", Kind: "T", Introduced: *ver(33), Removed: ver(40), Replacement: &GVK{Group: "c", Version: "v1", Kind: "T"}},
		{Group: "c", Version: "v1", Kind: "T", Introduced: *ver(37)},
		// v1alpha1 names v1 (1.30); v1beta1 and v1beta2 are both served
		// before it, listed oldest first
		{Group: "m", Version: "v1alpha1", Kind: "W", Introduced: *ver(20), Removed: ver(26), Replacement: &GVK{Group: "m", Version: "v1", Kind: "W"}},
		{Group: "m", Version: "v1beta1", Kind: "W", Introduced: *ver(22), Removed: ver(28), Replacement: &GVK{Group: "m", Version: "v1", Kind: "W"}},
		{Group: "m", Version: "v1beta2", Kind: "W", Introduced: *ver(24), Replacement: &GVK{Group: "m", Version: "v1", Kind: "W"}},
		{Group: "m", Version: "v1", Kind: "W", Introduced: *ver(30)},
	})
	start := func(g, v, k string) APILifecycleEntry {
		e, ok := idx.Lookup(g, v, k)
		if !ok {
			t.Fatalf("fixture missing %s/%s %s", g, v, k)
		}
		return e
	}

	cases := []struct {
		name   string
		e      APILifecycleEntry
		target inventory.Version
		want   string // "" = no remediation
	}{
		{"one hop still served", start("flowcontrol.apiserver.k8s.io", "v1beta1", "FlowSchema"), *ver(26), "flowcontrol.apiserver.k8s.io/v1beta3 FlowSchema"},
		{"hop removed at target follows chain", start("flowcontrol.apiserver.k8s.io", "v1beta1", "FlowSchema"), *ver(32), "flowcontrol.apiserver.k8s.io/v1 FlowSchema"},
		{"hop removed before target follows chain", start("flowcontrol.apiserver.k8s.io", "v1beta1", "FlowSchema"), *ver(36), "flowcontrol.apiserver.k8s.io/v1 FlowSchema"},
		{"no replacement", start("flowcontrol.apiserver.k8s.io", "v1", "FlowSchema"), *ver(36), ""},
		{"unknown replacement carries no removal evidence", start("x", "v1beta1", "A"), *ver(36), "x/v1 A"},
		{"dead end before target", start("y", "v1alpha1", "B"), *ver(30), ""},
		{"dead end not yet reached", start("y", "v1alpha1", "B"), *ver(22), "y/v1beta1 B"},
		{"cycle terminates", start("z", "v1", "C"), *ver(36), ""},
		// #237: a hop introduced after the target is not served there; the
		// newest served version of its kind newer than e is recommended
		// instead, or none.
		{"hop not yet introduced, served successor", start("s", "v1alpha1", "V"), *ver(33), "s/v1beta1 V"},
		{"hop introduced at target", start("s", "v1alpha1", "V"), *ver(34), "s/v1 V"},
		{"hop not yet introduced, nothing newer served", start("c", "v1beta1", "T"), *ver(36), ""},
		{"hop introduced at target, older alpha ignored", start("c", "v1beta1", "T"), *ver(37), "c/v1 T"},
		// RM-01: of two served successors, the newest is named, not the
		// first listed.
		{"hop not yet introduced, newest of two served successors", start("m", "v1alpha1", "W"), *ver(26), "m/v1beta2 W"},
	}
	for _, c := range cases {
		got, ok := idx.ResolveReplacement(c.e, c.target)
		gotS := ""
		if ok {
			gotS = got.Group + "/" + got.Version + " " + got.Kind
		}
		if gotS != c.want {
			t.Errorf("%s: ResolveReplacement(%s/%s %s, %s) = %q, want %q", c.name, c.e.Group, c.e.Version, c.e.Kind, c.target, gotS, c.want)
		}
	}
}

// When no served replacement is known, LaterReplacement names the one the
// chain reaches that a later release serves, and that release.
func TestLaterReplacement(t *testing.T) {
	idx := NewIndex([]APILifecycleEntry{
		{Group: "c", Version: "v1alpha1", Kind: "T", Introduced: *ver(26), Removed: ver(37)},
		{Group: "c", Version: "v1beta1", Kind: "T", Introduced: *ver(33), Removed: ver(40), Replacement: &GVK{Group: "c", Version: "v1", Kind: "T"}},
		{Group: "c", Version: "v1", Kind: "T", Introduced: *ver(37)},
	})
	e, _ := idx.Lookup("c", "v1beta1", "T")
	g, from, ok := idx.LaterReplacement(e, *ver(36))
	if !ok || g != (GVK{Group: "c", Version: "v1", Kind: "T"}) || from != *ver(37) {
		t.Errorf("LaterReplacement at 1.36 = %v %s %v, want c/v1 T from 1.37", g, from, ok)
	}
	if _, _, ok := idx.LaterReplacement(e, *ver(37)); ok {
		t.Error("LaterReplacement at 1.37 reports a replacement the target serves")
	}
}

// TestDeprecationGuideCoverage pins every GVK the upstream deprecation guide
// (https://kubernetes.io/docs/reference/using-api/deprecation-guide/) lists
// as no longer served: each must resolve in the merged KB with the guide's
// removal version, and the remediation resolved at that version must be
// the guide's migration target. A GVK the KB does not know is skipped by
// the engine, so a gap here is a silent false pass.
func TestDeprecationGuideCoverage(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	idx := NewIndex(k.APILifecycle)

	const (
		adm   = "admissionregistration.k8s.io"
		authn = "authentication.k8s.io"
		authz = "authorization.k8s.io"
		fc    = "flowcontrol.apiserver.k8s.io"
		rbac  = "rbac.authorization.k8s.io"
		stor  = "storage.k8s.io"
		netw  = "networking.k8s.io"
	)
	cases := []struct {
		group, version, kind string
		removed              int
		migrateTo            string // group/version the guide migrates to; "" = no API replacement
	}{
		// v1.32
		{fc, "v1beta3", "FlowSchema", 32, fc + "/v1"},
		{fc, "v1beta3", "PriorityLevelConfiguration", 32, fc + "/v1"},
		// v1.29 (guide: v1, or v1beta3 which is still served at 1.29)
		{fc, "v1beta2", "FlowSchema", 29, fc + "/v1beta3"},
		{fc, "v1beta2", "PriorityLevelConfiguration", 29, fc + "/v1beta3"},
		// v1.27
		{stor, "v1beta1", "CSIStorageCapacity", 27, stor + "/v1"},
		// v1.26 (guide: v1beta2; upstream tags v1beta3, also served at 1.26)
		{fc, "v1beta1", "FlowSchema", 26, fc + "/v1beta3"},
		{fc, "v1beta1", "PriorityLevelConfiguration", 26, fc + "/v1beta3"},
		{"autoscaling", "v2beta2", "HorizontalPodAutoscaler", 26, "autoscaling/v2"},
		// v1.25
		{"batch", "v1beta1", "CronJob", 25, "batch/v1"},
		{"discovery.k8s.io", "v1beta1", "EndpointSlice", 25, "discovery.k8s.io/v1"},
		{"events.k8s.io", "v1beta1", "Event", 25, "events.k8s.io/v1"},
		{"autoscaling", "v2beta1", "HorizontalPodAutoscaler", 25, "autoscaling/v2"},
		{"policy", "v1beta1", "PodDisruptionBudget", 25, "policy/v1"},
		{"policy", "v1beta1", "PodSecurityPolicy", 25, ""},
		{"node.k8s.io", "v1beta1", "RuntimeClass", 25, "node.k8s.io/v1"},
		// v1.22
		{adm, "v1beta1", "MutatingWebhookConfiguration", 22, adm + "/v1"},
		{adm, "v1beta1", "ValidatingWebhookConfiguration", 22, adm + "/v1"},
		{"apiextensions.k8s.io", "v1beta1", "CustomResourceDefinition", 22, "apiextensions.k8s.io/v1"},
		{"apiregistration.k8s.io", "v1beta1", "APIService", 22, "apiregistration.k8s.io/v1"},
		{authn, "v1beta1", "TokenReview", 22, authn + "/v1"},
		{authz, "v1beta1", "LocalSubjectAccessReview", 22, authz + "/v1"},
		{authz, "v1beta1", "SelfSubjectAccessReview", 22, authz + "/v1"},
		{authz, "v1beta1", "SubjectAccessReview", 22, authz + "/v1"},
		{authz, "v1beta1", "SelfSubjectRulesReview", 22, authz + "/v1"},
		{"certificates.k8s.io", "v1beta1", "CertificateSigningRequest", 22, "certificates.k8s.io/v1"},
		{"coordination.k8s.io", "v1beta1", "Lease", 22, "coordination.k8s.io/v1"},
		{"extensions", "v1beta1", "Ingress", 22, netw + "/v1"},
		{netw, "v1beta1", "Ingress", 22, netw + "/v1"},
		{netw, "v1beta1", "IngressClass", 22, netw + "/v1"},
		{rbac, "v1beta1", "ClusterRole", 22, rbac + "/v1"},
		{rbac, "v1beta1", "ClusterRoleBinding", 22, rbac + "/v1"},
		{rbac, "v1beta1", "Role", 22, rbac + "/v1"},
		{rbac, "v1beta1", "RoleBinding", 22, rbac + "/v1"},
		{"scheduling.k8s.io", "v1beta1", "PriorityClass", 22, "scheduling.k8s.io/v1"},
		{stor, "v1beta1", "CSIDriver", 22, stor + "/v1"},
		{stor, "v1beta1", "CSINode", 22, stor + "/v1"},
		{stor, "v1beta1", "StorageClass", 22, stor + "/v1"},
		{stor, "v1beta1", "VolumeAttachment", 22, stor + "/v1"},
		// v1.16. The guide also lists apps/v1beta1 ReplicaSet, but apps/v1beta1
		// never registered ReplicaSet (k8s.io/api apps/v1beta1/register.go),
		// so there is nothing a manifest could have used.
		{"extensions", "v1beta1", "NetworkPolicy", 16, netw + "/v1"},
		{"extensions", "v1beta1", "DaemonSet", 16, "apps/v1"},
		{"apps", "v1beta2", "DaemonSet", 16, "apps/v1"},
		{"extensions", "v1beta1", "Deployment", 16, "apps/v1"},
		{"apps", "v1beta1", "Deployment", 16, "apps/v1"},
		{"apps", "v1beta2", "Deployment", 16, "apps/v1"},
		{"apps", "v1beta1", "StatefulSet", 16, "apps/v1"},
		{"apps", "v1beta2", "StatefulSet", 16, "apps/v1"},
		{"extensions", "v1beta1", "ReplicaSet", 16, "apps/v1"},
		{"apps", "v1beta2", "ReplicaSet", 16, "apps/v1"},
		{"extensions", "v1beta1", "PodSecurityPolicy", 16, "policy/v1beta1"},
	}
	for _, c := range cases {
		e, ok := idx.Lookup(c.group, c.version, c.kind)
		if !ok {
			t.Errorf("KB missing %s/%s %s (deprecation guide: removed in 1.%d)", c.group, c.version, c.kind, c.removed)
			continue
		}
		if e.Removed == nil || *e.Removed != *ver(c.removed) {
			t.Errorf("%s/%s %s: Removed = %v, want 1.%d", c.group, c.version, c.kind, e.Removed, c.removed)
		}
		r, ok := idx.ResolveReplacement(e, *ver(c.removed))
		got := ""
		if ok {
			got = gvString(r)
			if r.Kind != c.kind {
				t.Errorf("%s/%s %s: remediation kind = %s, want %s", c.group, c.version, c.kind, r.Kind, c.kind)
			}
		}
		if got != c.migrateTo {
			t.Errorf("%s/%s %s: remediation at 1.%d = %q, want %q", c.group, c.version, c.kind, c.removed, got, c.migrateTo)
		}
	}
}

// TestRemediationNeverPointsAtRemovedAPI: for every KB entry and every
// target, the resolved remediation names a GVK the KB knows that is still
// served at that target, and every replacement tag in the data names a GVK
// the KB knows (so resolution can only stop at a genuine dead end).
func TestRemediationNeverPointsAtRemovedAPI(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	idx := NewIndex(k.APILifecycle)
	for _, e := range k.APILifecycle {
		if e.Replacement != nil {
			if _, ok := idx.Lookup(e.Replacement.Group, e.Replacement.Version, e.Replacement.Kind); !ok {
				t.Errorf("%s/%s %s: replacement %s %s is not in the KB", e.Group, e.Version, e.Kind, gvString(*e.Replacement), e.Replacement.Kind)
			}
		}
		for minor := 9; minor <= k.MaxKnownK8s.Minor+3; minor++ { // #237 asks 1.20 onwards; every target is checked

			target := *ver(minor)
			r, ok := idx.ResolveReplacement(e, target)
			if !ok {
				continue
			}
			re, known := idx.Lookup(r.Group, r.Version, r.Kind)
			if !known {
				t.Errorf("%s/%s %s @%s: remediation %s %s is not in the KB", e.Group, e.Version, e.Kind, target, gvString(r), r.Kind)
				continue
			}
			if re.Removed != nil && re.Removed.Compare(target) <= 0 {
				t.Errorf("%s/%s %s @%s: remediation %s %s is removed in %s", e.Group, e.Version, e.Kind, target, gvString(r), r.Kind, re.Removed)
			}
			if re.Introduced.Compare(target) > 0 {
				t.Errorf("%s/%s %s @%s: remediation %s %s is introduced in %s, after the target", e.Group, e.Version, e.Kind, target, gvString(r), r.Kind, re.Introduced)
			}
		}
	}
}

// #237's cases on the shipped dataset: at 1.33 VolumeAttributesClass
// v1alpha1 migrates to v1beta1 (v1 is served from 1.34); at 1.36
// ClusterTrustBundle v1beta1 has no served replacement, and v1 is served
// from 1.37.
func TestResolveReplacementShippedServedOnly(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	idx := NewIndex(k.APILifecycle)
	vac, _ := idx.Lookup("storage.k8s.io", "v1alpha1", "VolumeAttributesClass")
	if r, ok := idx.ResolveReplacement(vac, *ver(33)); !ok || r != (GVK{Group: "storage.k8s.io", Version: "v1beta1", Kind: "VolumeAttributesClass"}) {
		t.Errorf("VolumeAttributesClass v1alpha1 @1.33 = %v %v, want storage.k8s.io/v1beta1", r, ok)
	}
	ctb, _ := idx.Lookup("certificates.k8s.io", "v1beta1", "ClusterTrustBundle")
	if r, ok := idx.ResolveReplacement(ctb, *ver(36)); ok {
		t.Errorf("ClusterTrustBundle v1beta1 @1.36 = %v, want none served", r)
	}
	if g, from, ok := idx.LaterReplacement(ctb, *ver(36)); !ok || g.Version != "v1" || from != *ver(37) {
		t.Errorf("LaterReplacement(ClusterTrustBundle v1beta1, 1.36) = %v %s %v, want v1 from 1.37", g, from, ok)
	}
}

func gvString(g GVK) string {
	if g.Group == "" {
		return g.Version
	}
	return g.Group + "/" + g.Version
}

// ServedAlternative: for an API the target does not serve yet, the newest
// version of its group and kind that the target does serve.
func TestServedAlternative(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	idx := NewIndex(k.APILifecycle)
	cases := []struct {
		group, version, kind string
		target               int
		want                 string // group/version, "" = none
	}{
		// resource.k8s.io v1 is served from 1.34; v1beta2 (1.33) is the newest before it.
		{"resource.k8s.io", "v1", "DeviceClass", 33, "resource.k8s.io/v1beta2"},
		{"resource.k8s.io", "v1", "DeviceClass", 32, "resource.k8s.io/v1beta1"},
		// Workload v1beta1 is served from 1.37; at 1.36 only v1alpha2 is.
		{"scheduling.k8s.io", "v1beta1", "Workload", 36, "scheduling.k8s.io/v1alpha2"},
		{"scheduling.k8s.io", "v1beta1", "Workload", 35, "scheduling.k8s.io/v1alpha1"},
		// Nothing of the kind is served before 1.35.
		{"scheduling.k8s.io", "v1beta1", "Workload", 34, ""},
		// v1 VolumeAttributesClass is served from 1.34, v1beta1 from 1.31.
		{"storage.k8s.io", "v1", "VolumeAttributesClass", 33, "storage.k8s.io/v1beta1"},
	}
	for _, c := range cases {
		e, ok := idx.Lookup(c.group, c.version, c.kind)
		if !ok {
			t.Errorf("KB lacks %s/%s %s", c.group, c.version, c.kind)
			continue
		}
		got := ""
		if g, ok := idx.ServedAlternative(e, *ver(c.target)); ok {
			got = gvString(g)
		}
		if got != c.want {
			t.Errorf("ServedAlternative(%s/%s %s, 1.%d) = %q, want %q", c.group, c.version, c.kind, c.target, got, c.want)
		}
	}
}

// For every entry and every target that does not serve it yet, the
// alternative is a version of the same kind the KB knows the target
// serves, never the entry itself.
func TestServedAlternativeIsServed(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	idx := NewIndex(k.APILifecycle)
	unserved := 0
	for _, e := range k.APILifecycle {
		for minor := 9; minor <= k.MaxKnownK8s.Minor+3; minor++ {
			target := *ver(minor)
			if e.Introduced.Compare(target) <= 0 {
				continue
			}
			unserved++
			g, ok := idx.ServedAlternative(e, target)
			if !ok {
				continue
			}
			re, known := idx.Lookup(g.Group, g.Version, g.Kind)
			switch {
			case !known:
				t.Errorf("%s/%s %s @%s: alternative %s %s is not in the KB", e.Group, e.Version, e.Kind, target, gvString(g), g.Kind)
			case !servedAt(re, target):
				t.Errorf("%s/%s %s @%s: alternative %s %s is not served (introduced %s, removed %v)", e.Group, e.Version, e.Kind, target, gvString(g), g.Kind, re.Introduced, re.Removed)
			case g.Group != e.Group || g.Kind != e.Kind || g.Version == e.Version:
				t.Errorf("%s/%s %s @%s: alternative %s %s is not another version of the same kind", e.Group, e.Version, e.Kind, target, gvString(g), g.Kind)
			}
		}
	}
	if unserved == 0 {
		t.Fatal("no entry is introduced after a checked target: the test checked nothing")
	}
}

// #266: a removed entry whose kind has a GA version carries a remediation
// at its removal release, so the blockers a user hits when upgrading past a
// removal say what to migrate to. Where the GA version is introduced after
// the removal (resource.k8s.io v1alpha1 ResourceClaim, removed 1.27, v1
// from 1.34), the remediation names the newest version served at the
// removal, or the release the GA version is served from.
func TestEveryRemovedEntryWithAGASuccessorHasARemedy(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	idx := NewIndex(k.APILifecycle)
	checked := 0
	for _, e := range k.APILifecycle {
		if e.Removed == nil {
			continue
		}
		var ga *APILifecycleEntry
		for _, c := range idx.byKind[GVK{Group: e.Group, Kind: e.Kind}] {
			if c.Deprecated == nil && c.Removed == nil && (ga == nil || c.Introduced.Compare(ga.Introduced) > 0) {
				ga = &c
			}
		}
		if ga == nil {
			continue
		}
		checked++
		name := e.Group + "/" + e.Version + " " + e.Kind
		if e.Replacement == nil {
			t.Errorf("%s (removed %s) has no replacement, but %s/%s is GA (introduced %s)", name, e.Removed, ga.Group, ga.Version, ga.Introduced)
			continue
		}
		if _, ok := idx.ResolveReplacement(e, *e.Removed); ok {
			continue
		}
		if _, from, ok := idx.LaterReplacement(e, *e.Removed); !ok || from.Compare(*e.Removed) <= 0 {
			t.Errorf("%s: no remediation at its removal in %s: nothing serves its kind then and no later release is named", name, e.Removed)
		}
	}
	if checked < 30 {
		t.Errorf("checked only %d removed entries with a GA successor, want at least the 33 #266 lists", checked)
	}
}

// The examples of #266: the remediation of a removed beta blocker.
func TestRemovedBetaBlockersCarryARemediation(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	idx := NewIndex(k.APILifecycle)
	cases := []struct {
		group, version, kind string
		target               int
		want                 string // group/version, or "" with a later release named
		later                string
	}{
		{"admissionregistration.k8s.io", "v1beta1", "ValidatingAdmissionPolicy", 34, "admissionregistration.k8s.io/v1", ""},
		{"admissionregistration.k8s.io", "v1beta1", "ValidatingAdmissionPolicyBinding", 34, "admissionregistration.k8s.io/v1", ""},
		{"admissionregistration.k8s.io", "v1alpha1", "ValidatingAdmissionPolicy", 32, "admissionregistration.k8s.io/v1", ""},
		{"networking.k8s.io", "v1beta1", "ServiceCIDR", 37, "networking.k8s.io/v1", ""},
		{"networking.k8s.io", "v1beta1", "IPAddress", 37, "networking.k8s.io/v1", ""},
		{"authentication.k8s.io", "v1beta1", "SelfSubjectReview", 33, "authentication.k8s.io/v1", ""},
		{"resource.k8s.io", "v1beta1", "ResourceClaim", 38, "resource.k8s.io/v1", ""},
		{"batch", "v2alpha1", "CronJob", 21, "batch/v1", ""},
		// v1 does not exist yet when these were removed: the newest version
		// served then.
		{"networking.k8s.io", "v1alpha1", "ServiceCIDR", 31, "networking.k8s.io/v1beta1", ""},
		{"resource.k8s.io", "v1alpha1", "ResourceClaim", 27, "resource.k8s.io/v1alpha2", ""},
	}
	for _, c := range cases {
		e, ok := idx.Lookup(c.group, c.version, c.kind)
		if !ok {
			t.Errorf("KB lacks %s/%s %s", c.group, c.version, c.kind)
			continue
		}
		if e.Removed == nil || *e.Removed != *ver(c.target) {
			t.Errorf("%s/%s %s: removed %v, want 1.%d", c.group, c.version, c.kind, e.Removed, c.target)
		}
		r, ok := idx.ResolveReplacement(e, *ver(c.target))
		got := ""
		if ok {
			got = gvString(r)
		} else if g, from, ok := idx.LaterReplacement(e, *ver(c.target)); ok {
			got = ""
			if later := gvString(g) + " from " + from.String(); later != c.later {
				t.Errorf("%s/%s %s @1.%d: later replacement %q, want %q", c.group, c.version, c.kind, c.target, later, c.later)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%s/%s %s @1.%d: remediation %q, want %q", c.group, c.version, c.kind, c.target, got, c.want)
		}
	}
}

// #266: storage.k8s.io/v1alpha1 VolumeAttachment is dated by the release
// kube-apiserver stopped serving it, not by the upstream tag (1.24).
func TestVolumeAttachmentV1alpha1RemovedInTheReleaseItStoppedBeingServed(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	e, ok := NewIndex(k.APILifecycle).Lookup("storage.k8s.io", "v1alpha1", "VolumeAttachment")
	if !ok || e.Removed == nil || *e.Removed != *ver(23) || !e.RemovedInferred {
		t.Fatalf("storage.k8s.io/v1alpha1 VolumeAttachment = %+v, want removed 1.23, inferred", e)
	}
}

// Two versions introduced in the same release: the more stable one is the
// alternative (v1 over v1beta1 over v1alpha1), whatever the entry order.
func TestServedAlternativePrefersStabilityAtTheSameRelease(t *testing.T) {
	v := func(m int) inventory.Version { return inventory.Version{Major: 1, Minor: m} }
	entry := func(version string, intro int) APILifecycleEntry {
		return APILifecycleEntry{Group: "example.k8s.io", Version: version, Kind: "Thing", Introduced: v(intro)}
	}
	own := entry("v2", 40) // not served at 1.36
	for _, order := range [][]string{{"v1beta1", "v1", "v1alpha1"}, {"v1alpha1", "v1beta1", "v1"}, {"v1", "v1alpha1", "v1beta1"}} {
		var es []APILifecycleEntry
		for _, ver := range order {
			es = append(es, entry(ver, 30))
		}
		es = append(es, own)
		g, ok := NewIndex(es).ServedAlternative(own, v(36))
		if !ok || g.Version != "v1" {
			t.Errorf("order %v: ServedAlternative = %v, %v; want v1", order, g, ok)
		}
	}
}

// "Other than e's own": when e itself is served at the target (the caller
// asked about an API that is already available), it is never its own
// alternative, even when it is the newest served version of its kind, and
// a kind with no other served version has none.
func TestServedAlternativeIsNeverTheEntryItself(t *testing.T) {
	v := func(m int) inventory.Version { return inventory.Version{Major: 1, Minor: m} }
	entry := func(version string, intro int) APILifecycleEntry {
		return APILifecycleEntry{Group: "example.k8s.io", Version: version, Kind: "Thing", Introduced: v(intro)}
	}
	beta, ga := entry("v1beta1", 30), entry("v1", 34)
	idx := NewIndex([]APILifecycleEntry{beta, ga})
	// ga is served at 1.36 and is the newest: the alternative is the older beta.
	if g, ok := idx.ServedAlternative(ga, v(36)); !ok || g.Version != "v1beta1" {
		t.Errorf("ServedAlternative(v1 served at 1.36) = %v, %v; want example.k8s.io v1beta1", g, ok)
	}
	// And the other way round: beta is served too, and the newer v1 is the alternative.
	if g, ok := idx.ServedAlternative(beta, v(36)); !ok || g.Version != "v1" {
		t.Errorf("ServedAlternative(v1beta1 served at 1.36) = %v, %v; want example.k8s.io v1", g, ok)
	}
	// A kind with only the entry itself has no alternative, served or not.
	only := NewIndex([]APILifecycleEntry{ga})
	for _, target := range []int{33, 36} {
		if g, ok := only.ServedAlternative(ga, v(target)); ok {
			t.Errorf("ServedAlternative with no other version at 1.%d = %v, want none", target, g)
		}
	}
}

// #302: a removed alpha whose kind has no GA version (so no replacement
// chain) still has a successor of its kind served at its removal release
// when a later version exists: the engine names it (ServedSuccessor). The
// seven entries the verifier counted, with what the dataset serves then:
// not always the beta (LeaseCandidate v1alpha1 is removed in 1.32, before
// v1beta1 is served in 1.33).
func TestRemovedAlphasWithAPreGASuccessorHaveARemedy(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	idx := NewIndex(k.APILifecycle)
	want := []struct {
		group, version, kind string
		removed              int
		successor            string // group/version served at the removal release
	}{
		{"scheduling.k8s.io", "v1alpha2", "Workload", 37, "scheduling.k8s.io/v1beta1"},
		{"scheduling.k8s.io", "v1alpha2", "PodGroup", 37, "scheduling.k8s.io/v1beta1"},
		{"scheduling.k8s.io", "v1alpha1", "Workload", 36, "scheduling.k8s.io/v1alpha2"},
		{"coordination.k8s.io", "v1alpha2", "LeaseCandidate", 38, "coordination.k8s.io/v1beta1"},
		{"coordination.k8s.io", "v1alpha1", "LeaseCandidate", 32, "coordination.k8s.io/v1alpha2"},
		{"resource.k8s.io", "v1alpha1", "ResourceClass", 27, "resource.k8s.io/v1alpha2"},
		{"resource.k8s.io", "v1alpha2", "PodSchedulingContext", 31, "resource.k8s.io/v1alpha3"},
	}
	listed := map[GVK]bool{}
	for _, w := range want {
		e, ok := idx.Lookup(w.group, w.version, w.kind)
		if !ok {
			t.Errorf("KB lacks %s/%s %s", w.group, w.version, w.kind)
			continue
		}
		listed[GVK{Group: w.group, Version: w.version, Kind: w.kind}] = true
		if e.Removed == nil || *e.Removed != *ver(w.removed) {
			t.Errorf("%s/%s %s: removed %v, want 1.%d", w.group, w.version, w.kind, e.Removed, w.removed)
			continue
		}
		if r, ok := idx.ResolveReplacement(e, *e.Removed); ok {
			t.Errorf("%s/%s %s: has a replacement chain (%s) and is not a #302 case", w.group, w.version, w.kind, gvString(r))
		}
		got, ok := idx.ServedSuccessor(e, *e.Removed)
		if !ok || got.Group+"/"+got.Version != w.successor {
			t.Errorf("ServedSuccessor(%s/%s %s, %s) = %s/%s %v, want %s", w.group, w.version, w.kind, e.Removed, got.Group, got.Version, ok, w.successor)
		}
	}

	// Derived: every removed entry with a same-kind version introduced
	// after it and served at its removal release has a remedy there, by
	// the replacement chain, a later release's replacement, or the served
	// successor; and the entries that rely on the successor are exactly
	// the seven above (a new one is a deliberate addition to this list).
	reliant := map[GVK]bool{}
	for _, e := range k.APILifecycle {
		if e.Removed == nil {
			continue
		}
		hasSuccessor := false
		for _, c := range idx.byKind[GVK{Group: e.Group, Kind: e.Kind}] {
			if c.Version != e.Version && c.Introduced.Compare(e.Introduced) > 0 && servedAt(c, *e.Removed) {
				hasSuccessor = true
			}
		}
		if !hasSuccessor {
			continue
		}
		name := e.Group + "/" + e.Version + " " + e.Kind
		if _, ok := idx.ResolveReplacement(e, *e.Removed); ok {
			continue
		}
		if _, _, ok := idx.LaterReplacement(e, *e.Removed); ok {
			continue
		}
		s, ok := idx.ServedSuccessor(e, *e.Removed)
		if !ok {
			t.Errorf("%s (removed %s): a later version of its kind is served then, but ServedSuccessor names none", name, e.Removed)
			continue
		}
		if s.Introduced.Compare(e.Introduced) <= 0 || !servedAt(s, *e.Removed) || s.Kind != e.Kind || s.Group != e.Group {
			t.Errorf("%s (removed %s): successor %s is not a later version served then", name, e.Removed, s.Version)
		}
		reliant[GVK{Group: e.Group, Version: e.Version, Kind: e.Kind}] = true
	}
	for g := range reliant {
		if !listed[g] {
			t.Errorf("%s/%s %s relies on the served successor but is not in the list of seven", g.Group, g.Version, g.Kind)
		}
	}
	for g := range listed {
		if !reliant[g] {
			t.Errorf("%s/%s %s is in the list of seven but does not rely on the served successor", g.Group, g.Version, g.Kind)
		}
	}
}

// ServedSuccessor never names the entry itself, a version introduced
// before or with it, one the target does not serve yet or has removed, and
// breaks a tie in introduction by stability.
func TestServedSuccessor(t *testing.T) {
	entry := func(version string, intro int, removed *inventory.Version) APILifecycleEntry {
		return APILifecycleEntry{Group: "example.k8s.io", Version: version, Kind: "Thing", Introduced: *ver(intro), Removed: removed}
	}
	older, own := entry("v1alpha1", 30, ver(34)), entry("v1alpha2", 32, ver(36))
	idx := NewIndex([]APILifecycleEntry{
		older, own,
		entry("v1beta1", 35, nil),
		entry("v1alpha3", 35, nil), // same release as the beta: the beta wins
		entry("v1beta2", 38, nil),
		entry("v1", 40, nil),
	})
	for _, c := range []struct {
		e      APILifecycleEntry
		target int
		want   string
	}{
		{own, 36, "v1beta1"},
		{own, 34, ""}, // nothing introduced after it is served yet
		{own, 38, "v1beta2"},
		{own, 40, "v1"},
		{older, 34, "v1alpha2"}, // newer than the entry, not the entry itself
	} {
		got, ok := idx.ServedSuccessor(c.e, *ver(c.target))
		if g := got.Version; (g != c.want) || ok != (c.want != "") {
			t.Errorf("ServedSuccessor(%s, 1.%d) = %q %v, want %q", c.e.Version, c.target, g, ok, c.want)
		}
	}
	// Removed successors are not served.
	idx = NewIndex([]APILifecycleEntry{entry("v1alpha1", 30, ver(34)), entry("v1alpha2", 32, ver(36))})
	if got, ok := idx.ServedSuccessor(entry("v1alpha1", 30, ver(34)), *ver(36)); ok {
		t.Errorf("a successor removed by the target was named: %s", got.Version)
	}
}
