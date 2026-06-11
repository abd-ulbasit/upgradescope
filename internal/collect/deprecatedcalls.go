package collect

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

const deprecatedAPIsMetric = "apiserver_requested_deprecated_apis"

// collectDeprecatedCalls scrapes the apiserver /metrics endpoint
// (RBAC: nonResourceURLs ["/metrics"], verb get) and extracts
// apiserver_requested_deprecated_apis rows — active callers of deprecated
// APIs, the blind spot of manifest-only scanners. Known limits (spec §4):
// gauge resets on apiserver restart; HA apiservers report independently.
func collectDeprecatedCalls(ctx context.Context, rc rest.Interface, inv *inventory.Inventory) error {
	raw, err := rc.Get().AbsPath("/metrics").DoRaw(ctx)
	if err != nil {
		// 401/403 is the expected state on managed control planes
		// (EKS/GKE/AKS often deny /metrics regardless of RBAC) — make
		// the capability reason say so instead of a raw client error.
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			return fmt.Errorf("apiserver /metrics forbidden (managed control planes often block this; see README §Managed clusters): %w", err)
		}
		return fmt.Errorf("get /metrics: %w", err)
	}
	// prometheus/common >= v0.66 requires an explicit name-validation
	// scheme; the zero-value TextParser panics. UTF-8 is the permissive
	// choice — we only read, never emit, metric names.
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("parse metrics exposition: %w", err)
	}
	fam, ok := families[deprecatedAPIsMetric]
	if !ok {
		return nil // no deprecated API requested since apiserver start
	}
	var calls []inventory.DeprecatedCall
	for _, m := range fam.GetMetric() {
		var c inventory.DeprecatedCall
		for _, lp := range m.GetLabel() {
			switch lp.GetName() {
			case "group":
				c.Group = lp.GetValue()
			case "version":
				c.Version = lp.GetValue()
			case "resource":
				c.Resource = lp.GetValue()
			case "subresource":
				c.Subresource = lp.GetValue()
			case "removed_release":
				c.RemovedRelease = lp.GetValue()
			}
		}
		calls = append(calls, c)
	}
	sort.Slice(calls, func(i, j int) bool {
		a, b := calls[i], calls[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		if a.Resource != b.Resource {
			return a.Resource < b.Resource
		}
		return a.Subresource < b.Subresource
	})
	inv.DeprecatedCalls = calls
	return nil
}
