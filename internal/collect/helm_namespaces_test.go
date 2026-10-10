package collect

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// helmNamespacedFixture is a release in each of kube-system, ingress-nginx
// and apps, the third outside the namespaces the tests below list.
func helmNamespacedFixture(t *testing.T) []runtime.Object {
	return []runtime.Object{
		helmSecret(t, helmRev{ns: "kube-system", release: "cilium", rev: 1, status: "deployed", chart: "cilium", chartVersion: "1.18.0"}),
		helmSecret(t, helmRev{ns: "ingress-nginx", release: "ingress-nginx", rev: 2, status: "deployed", chart: "ingress-nginx", chartVersion: "4.12.0"}),
		helmConfigMap(t, helmRev{ns: "ingress-nginx", release: "legacy", rev: 1, status: "deployed", chart: "legacy", chartVersion: "0.1.0"}),
		helmSecret(t, helmRev{ns: "apps", release: "web", rev: 1, status: "deployed", chart: "web", chartVersion: "3.0.0"}),
	}
}

// #344: with --helm-namespaces (the chart's rbac.helmSecretsNamespaces) the
// agent holds a Role in each listed namespace and no cluster-wide grant, so
// Helm's storage is listed in those namespaces only, never across the
// cluster, and the releases elsewhere are a named gap: the capability is
// partial, saying which namespaces were read.
func TestCollectHelmListsOnlyTheListedNamespaces(t *testing.T) {
	kube, meta := helmClients(t, helmNamespacedFixture(t)...)
	var inv inventory.Inventory
	err := collectHelmWith(context.Background(), kube, meta, nil, nil, []string{"kube-system", "ingress-nginx"}, &inv)
	var pe partialError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a partialError", err)
	}

	var listed []string
	for _, a := range meta.Actions() {
		l, ok := a.(clienttesting.ListAction)
		if !ok {
			t.Errorf("metadata request %s %s, want lists only", a.GetVerb(), a.GetResource().Resource)
			continue
		}
		if a.GetNamespace() == "" {
			t.Errorf("cluster-wide list of %s, want only the listed namespaces", a.GetResource().Resource)
		}
		listed = append(listed, a.GetResource().Resource+" "+a.GetNamespace()+"?"+l.GetListRestrictions().Labels.String())
	}
	slices.Sort(listed)
	want := []string{
		"configmaps ingress-nginx?owner=helm", "configmaps kube-system?owner=helm",
		"secrets ingress-nginx?owner=helm", "secrets kube-system?owner=helm",
	}
	if !slices.Equal(listed, want) {
		t.Errorf("metadata lists = %v\nwant %v", listed, want)
	}
	for _, a := range kube.Actions() {
		if ns := a.GetNamespace(); ns != "kube-system" && ns != "ingress-nginx" {
			t.Errorf("typed-client %s %s in namespace %q, want only the listed namespaces", a.GetVerb(), a.GetResource().Resource, ns)
		}
	}

	var got []string
	for _, r := range inv.HelmReleases {
		got = append(got, r.Namespace+"/"+r.Name)
	}
	if want := []string{"ingress-nginx/ingress-nginx", "ingress-nginx/legacy", "kube-system/cilium"}; !slices.Equal(got, want) {
		t.Errorf("releases = %v, want %v (the apps release is outside the list)", got, want)
	}

	const reason = "Helm releases read only in namespaces ingress-nginx, kube-system (rbac.helmSecretsNamespaces); releases in other namespaces were not assessed"
	if !pe.incomplete || !strings.Contains(pe.msg, reason) || !strings.HasPrefix(pe.msg, "helm releases: 2 via secrets, 1 via configmaps") {
		t.Errorf("partial = %v, reason %q; want incomplete, counting the releases and saying %q", pe.incomplete, pe.msg, reason)
	}
	if !reflect.DeepEqual(pe.skipped, []string{helmSkippedOtherNamespaces}) {
		t.Errorf("skipped = %q, want [%q]: no slash, so every Helm finding of a release elsewhere is a gap", pe.skipped, helmSkippedOtherNamespaces)
	}

	// Through runSteps, as the agent reports it: available, partial, the
	// reason in the capability.
	kube, meta = helmClients(t, helmNamespacedFixture(t)...)
	inv = inventory.Inventory{Capabilities: map[inventory.Capability]inventory.CapabilityStatus{}}
	runSteps(context.Background(), &inv, []step{{cap: inventory.CapHelm, run: func(ctx context.Context, inv *inventory.Inventory) error {
		return collectHelmStep(ctx, Clients{Kube: kube, Metadata: meta}, nil, nil, []string{"ingress-nginx", "kube-system"}, nil, inv)
	}}})
	if st := inv.Capabilities[inventory.CapHelm]; !st.Available || !st.Partial || !strings.Contains(st.Reason, reason) {
		t.Errorf("helm capability = %+v, want available and partial with %q", st, reason)
	}
}

// The namespaces are a set: given in any order and repeated, each is
// listed once, and the reason names them sorted.
func TestCollectHelmNamespacesAreASet(t *testing.T) {
	kube, meta := helmClients(t, helmNamespacedFixture(t)...)
	var inv inventory.Inventory
	err := collectHelmWith(context.Background(), kube, meta, nil, nil, []string{"kube-system", "apps", "kube-system"}, &inv)
	var pe partialError
	if !errors.As(err, &pe) || !strings.Contains(pe.msg, "read only in namespaces apps, kube-system (") {
		t.Fatalf("err = %v, want a partial naming apps, kube-system", err)
	}
	if n := len(meta.Actions()); n != 4 {
		t.Errorf("%d metadata lists, want 4 (2 drivers × 2 namespaces)", n)
	}
}

// A listed namespace whose list is refused (the chart created no Role
// there, or it was deleted) is a driver not read, as a refused
// cluster-wide list is: for Secrets, Helm's default, the capability is
// unavailable and names the namespace, since its releases are unknown.
func TestCollectHelmRefusedNamespaceIsAGap(t *testing.T) {
	kube, meta := helmClients(t, helmNamespacedFixture(t)...)
	meta.PrependReactor("list", "secrets", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() != "ingress-nginx" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("RBAC"))
	})
	var inv inventory.Inventory
	err := collectHelmWith(context.Background(), kube, meta, nil, nil, []string{"kube-system", "ingress-nginx"}, &inv)
	if err == nil || errors.As(err, new(partialError)) {
		t.Fatalf("err = %v, want the capability unavailable: Secrets in a listed namespace went unread", err)
	}
	for _, want := range []string{"secrets not read: list secrets in namespace ingress-nginx: ", "forbidden", "read only in namespaces ingress-nginx, kube-system"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("reason %q\nmissing %q", err, want)
		}
	}
	// Every listed namespace is still listed: one refusal does not hide what
	// the others hold.
	var listed []string
	for _, a := range meta.Actions() {
		listed = append(listed, a.GetResource().Resource+" "+a.GetNamespace())
	}
	slices.Sort(listed)
	if want := []string{"configmaps ingress-nginx", "configmaps kube-system", "secrets ingress-nginx", "secrets kube-system"}; !slices.Equal(listed, want) {
		t.Errorf("lists = %v, want %v", listed, want)
	}
}

// No namespaces is the cluster-wide read, as before #344: the scan CLI and
// an agent without --helm-namespaces.
func TestCollectHelmWithoutNamespacesListsClusterWide(t *testing.T) {
	kube, meta := helmClients(t, helmNamespacedFixture(t)...)
	var inv inventory.Inventory
	err := collectHelmWith(context.Background(), kube, meta, nil, nil, nil, &inv)
	var pe partialError
	if !errors.As(err, &pe) || pe.incomplete || pe.msg != "helm releases: 3 via secrets, 1 via configmaps" {
		t.Fatalf("err = %v (incomplete %v), want the complete cluster-wide count", err, pe.incomplete)
	}
	for _, a := range meta.Actions() {
		if a.GetNamespace() != "" {
			t.Errorf("list in namespace %q, want cluster-wide", a.GetNamespace())
		}
	}
}
