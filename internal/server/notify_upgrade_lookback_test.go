package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
)

// TestUpgradeBaselineSpansASkippedMinor: the agent missed the push at 1.35
// (1.34 → 1.36). The only decided evaluation below the new default target
// 1.37 is 1.35's, two minors down, and it is still the baseline: the
// ServiceCIDR v1beta1 call, fine at 1.35, is announced as a blocker for
// 1.37. With upgradeLookback = 1 the baseline is not found and the first
// evaluation of 1.37 would be silent.
func TestUpgradeBaselineSpansASkippedMinor(t *testing.T) {
	srv, rec := upgradeServer(t, openSQLite(t))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	pushInventory(t, ts.URL, "tok", atVersion("v1.34.5", serviceCIDRv1beta1))
	pushInventory(t, ts.URL, "tok", atVersion("v1.36.1", serviceCIDRv1beta1))
	srv.deliverOutbox(context.Background())

	evs := rec.all()
	if len(evs) != 1 || evs[0].Kind != notify.KindNewBlocker || evs[0].Target != "1.37" ||
		!strings.Contains(evs[0].Title, "ServiceCIDR") {
		t.Fatalf("want the ServiceCIDR new-blocker for 1.37 across the skipped minor, got %+v", evs)
	}
}

// TestUpgradeBaselineIsTheNearestLowerTarget: 1.33 (clean) → 1.34 (PSP) →
// 1.36 (PSP). Below 1.37 the cluster has decided evaluations for 1.34
// (clean, from the 1.33 push) and 1.35 (PSP, from the 1.34 push). The
// nearest, 1.35, is the baseline, so the PSP blocker it already had is not
// announced again. Searching farthest-first would pick 1.34's clean
// evaluation and announce it.
func TestUpgradeBaselineIsTheNearestLowerTarget(t *testing.T) {
	srv, rec := upgradeServer(t, openSQLite(t))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	pushInventory(t, ts.URL, "tok", atVersion("v1.33.5"))
	pushInventory(t, ts.URL, "tok", atVersion("v1.34.5", podSecurityPolicy))
	srv.deliverOutbox(context.Background())
	before := len(rec.all())

	pushInventory(t, ts.URL, "tok", atVersion("v1.36.1", podSecurityPolicy))
	srv.deliverOutbox(context.Background())
	if evs := rec.all(); len(evs) != before {
		t.Fatalf("PSP was already a blocker at the nearest lower target, got %+v", evs[before:])
	}
}
