package engine

import (
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func callsInv(c inventory.DeprecatedCall) inventory.Inventory {
	return inventory.Inventory{DeprecatedCalls: []inventory.DeprecatedCall{c}}
}

func TestEvalDeprecatedCallsSeverityVsTarget(t *testing.T) {
	row := inventory.DeprecatedCall{Group: "batch", Version: "v1beta1", Resource: "cronjobs", RemovedRelease: "1.25"}
	cases := []struct {
		name   string
		target inventory.Version
		sev    Severity
	}{
		{"removed at target", inventory.Version{Major: 1, Minor: 25}, SevBlocker},
		{"removed before target", inventory.Version{Major: 1, Minor: 26}, SevBlocker},
		{"removed at target+1", inventory.Version{Major: 1, Minor: 24}, SevWarning},
		{"removed beyond window", inventory.Version{Major: 1, Minor: 23}, SevInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := evalDeprecatedCalls(callsInv(row), tc.target)
			if len(fs) != 1 || fs[0].Severity != tc.sev || fs[0].Category != CatDeprecatedAPIInUse {
				t.Fatalf("want one %s deprecated-api-in-use, got %+v", tc.sev, fs)
			}
			if fs[0].Title != "clients still requesting batch/v1beta1 cronjobs (removed in 1.25)" {
				t.Fatalf("title = %q", fs[0].Title)
			}
		})
	}
}

func TestEvalDeprecatedCallsUnparseableIsInfo(t *testing.T) {
	row := inventory.DeprecatedCall{Group: "extensions", Version: "v1beta1", Resource: "ingresses"}
	fs := evalDeprecatedCalls(callsInv(row), inventory.Version{Major: 1, Minor: 34})
	if len(fs) != 1 || fs[0].Severity != SevInfo {
		t.Fatalf("missing removedRelease must yield info, got %+v", fs)
	}
	if fs[0].Title != "clients still requesting extensions/v1beta1 ingresses (deprecated)" {
		t.Fatalf("title = %q", fs[0].Title)
	}
}

func TestEvalDeprecatedCallsSubresource(t *testing.T) {
	row := inventory.DeprecatedCall{Group: "apps", Version: "v1beta2", Resource: "deployments",
		Subresource: "scale", RemovedRelease: "1.16"}
	fs := evalDeprecatedCalls(callsInv(row), inventory.Version{Major: 1, Minor: 34})
	if len(fs) != 1 || fs[0].Title != "clients still requesting apps/v1beta2 deployments/scale (removed in 1.16)" {
		t.Fatalf("got %+v", fs)
	}
}
