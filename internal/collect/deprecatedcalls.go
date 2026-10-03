package collect

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

const (
	deprecatedAPIsMetric = "apiserver_requested_deprecated_apis"
	processStartMetric   = "process_start_time_seconds"
)

// collectDeprecatedCalls scrapes the apiserver /metrics endpoint
// (RBAC: nonResourceURLs ["/metrics"], verb get) and extracts
// apiserver_requested_deprecated_apis rows — deprecated APIs some client
// requested, the blind spot of manifest-only scanners. The metric does not
// say which client. Known limits (spec §4): gauge resets on apiserver
// restart; HA apiservers report independently. The scraped apiserver's
// process_start_time_seconds is recorded as Inventory.APIServerStartTime,
// so a reader can tell a reset from a fix.
//
// The scanner feeds this metric itself only through selfListed: the
// resources api-usage listed at a deprecated version, because nothing
// else serves a kind that is being removed ("group/version resource",
// e.g. "policy/v1beta1 podsecuritypolicies" on 1.24). It runs after
// api-usage, so on every scan, the first after an apiserver restart
// included, the rows are there. Their rows are kept, and the capability
// comes back partial with selfListed as Skipped: for those resources the
// metric cannot tell other clients from the scanner, and the engine does
// not report them as callers.
func collectDeprecatedCalls(ctx context.Context, rc rest.Interface, selfListed []string, inv *inventory.Inventory) error {
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
	// When the gauge started counting: kube-apiserver exposes its process
	// start time beside it. Whole seconds; left zero when absent or not a
	// plausible time.
	if fam, ok := families[processStartMetric]; ok && len(fam.GetMetric()) == 1 {
		if sec := fam.GetMetric()[0].GetGauge().GetValue(); sec > 0 && sec < 1e12 {
			inv.APIServerStartTime = time.Unix(int64(sec), 0).UTC()
		}
	}
	fam, ok := families[deprecatedAPIsMetric]
	if !ok {
		return selfRequests(selfListed) // no deprecated API requested since apiserver start
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
	return selfRequests(selfListed)
}

// selfRequests is the outcome of a successful scrape: nil, or, when the
// scanner listed deprecated endpoints itself, a partialError naming them.
func selfRequests(selfListed []string) error {
	if len(selfListed) == 0 {
		return nil
	}
	return partialError{
		msg: fmt.Sprintf("upgradescope lists %s itself (nothing else serves a kind being removed), so the metric cannot show whether other clients request it; apiserver audit logs (annotation k8s.io/deprecated) can",
			strings.Join(selfListed, ", ")),
		incomplete: true,
		skipped:    selfListed,
	}
}
