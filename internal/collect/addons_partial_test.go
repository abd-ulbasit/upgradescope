package collect

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// Issue #199 (NEW-addons-1): a role that cannot list pods cluster-wide
// still has the Helm releases and IngressClasses the collector read, and
// the add-ons they name must be matched, not thrown away with the pods. The
// capability is partial and, as it was when unavailable, stays a required
// gap: a pods-forbidden cluster whose ingress-nginx came from kubectl
// apply must never be ready.
func TestCollectAddOnsPodsForbiddenStillMatchesHelmReleasesAndIngressClasses(t *testing.T) {
	forbidden := func(group, resource string) k8stesting.ReactionFunc {
		return func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: group, Resource: resource}, "", errors.New("RBAC"))
		}
	}
	class := &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{Name: "nginx"},
		Spec:       networkingv1.IngressClassSpec{Controller: "k8s.io/ingress-nginx"},
	}
	release := inventory.HelmRelease{Name: "ingress-nginx", Namespace: "ingress-nginx", ChartName: "ingress-nginx",
		ChartVersion: "4.7.1", AppVersion: "1.8.1", Status: "deployed"}
	k := loadKB(t)
	target := inventory.Version{Major: 1, Minor: 34}

	// run collects add-ons through the capability machinery and evaluates
	// the inventory with every other capability available.
	run := func(cs *kubefake.Clientset, releases ...inventory.HelmRelease) (inventory.Inventory, engine.Report) {
		inv := inventory.Inventory{Capabilities: map[inventory.Capability]inventory.CapabilityStatus{},
			HelmReleases: releases, ServerVersion: "v1.33.4"}
		runSteps(context.Background(), &inv, []step{{cap: inventory.CapAddOns, run: func(ctx context.Context, inv *inventory.Inventory) error {
			return collectAddOns(ctx, cs, k.AddOns, inv)
		}}})
		inv.Capabilities[inventory.CapAPIUsage] = inventory.CapabilityStatus{Available: true}
		inv.Capabilities[inventory.CapVersions] = inventory.CapabilityStatus{Available: true}
		return inv, engine.Evaluate(inv, k, target, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	}
	addonsGap := func(rep engine.Report) (engine.CapabilityGap, bool) {
		for _, g := range rep.NotAssessed {
			if g.Capability == inventory.CapAddOns {
				return g, true
			}
		}
		return engine.CapabilityGap{}, false
	}
	noPods := func(objs ...runtime.Object) *kubefake.Clientset {
		cs := kubefake.NewClientset(objs...)
		cs.PrependReactor("list", "pods", forbidden("", "pods"))
		return cs
	}

	t.Run("helm release", func(t *testing.T) {
		inv, rep := run(noPods(), release)
		st := inv.Capabilities[inventory.CapAddOns]
		if !st.Available || !st.Partial || !reflect.DeepEqual(st.Skipped, []string{"v1 pods"}) || !strings.Contains(st.Reason, "list pods") {
			t.Errorf("addons capability = %+v, want available, partial, skipping v1 pods, with the list error as reason", st)
		}
		if len(inv.AddOns) != 1 || inv.AddOns[0].ID != "ingress-nginx" || inv.AddOns[0].Source != "chart" {
			t.Errorf("addons = %+v, want ingress-nginx from the Helm release", inv.AddOns)
		}
		if rep.Verdict != engine.VerdictBlocked || !slices.ContainsFunc(rep.Findings, func(f engine.Finding) bool { return f.Key == "eol-addon/ingress-nginx" }) {
			t.Errorf("verdict %s, findings %+v, want blocked by eol-addon/ingress-nginx", rep.Verdict, rep.Findings)
		}
	})
	t.Run("ingressclass", func(t *testing.T) {
		inv, _ := run(noPods(class))
		if len(inv.AddOns) != 1 || inv.AddOns[0].ID != "ingress-nginx" || inv.AddOns[0].Source != "ingressclass" {
			t.Errorf("addons = %+v, want ingress-nginx from the IngressClass", inv.AddOns)
		}
	})
	t.Run("nothing detectable is still unknown", func(t *testing.T) {
		inv, rep := run(noPods())
		if len(inv.AddOns) != 0 {
			t.Errorf("addons = %+v, want none", inv.AddOns)
		}
		g, ok := addonsGap(rep)
		if rep.Verdict != engine.VerdictUnknown || !ok || !g.Required || !g.Partial {
			t.Errorf("verdict %s, addons gap %+v (%v), want unknown on a required partial addons gap", rep.Verdict, g, ok)
		}
	})
	t.Run("nothing read at all is not assessed", func(t *testing.T) {
		cs := noPods()
		cs.PrependReactor("list", "ingressclasses", forbidden("networking.k8s.io", "ingressclasses"))
		inv, _ := run(cs)
		if st := inv.Capabilities[inventory.CapAddOns]; st.Available || st.Partial {
			t.Errorf("addons capability = %+v, want not available: no list was read", st)
		}
	})
	t.Run("ingressclasses unreadable stays optional", func(t *testing.T) {
		cs := kubefake.NewClientset()
		cs.PrependReactor("list", "ingressclasses", forbidden("networking.k8s.io", "ingressclasses"))
		_, rep := run(cs)
		if g, ok := addonsGap(rep); !ok || !g.Partial || g.Required {
			t.Errorf("addons gap = %+v (%v), want an optional partial gap", g, ok)
		}
	})
}
