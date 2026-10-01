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
		for minor := 9; minor <= k.MaxKnownK8s.Minor+3; minor++ {
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
		}
	}
}

func gvString(g GVK) string {
	if g.Group == "" {
		return g.Version
	}
	return g.Group + "/" + g.Version
}
