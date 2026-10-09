package collect

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
// so the server can tell a caller missing because the gauge reset from
// one that is gone (evaluate.go, deprecatedCallsHold).
//
// The scanner feeds this metric itself only through self.listed: the
// resources api-usage listed at a deprecated version, because nothing
// else serves a kind that is being removed ("group/version resource",
// e.g. "policy/v1beta1 podsecuritypolicies" on 1.24). It runs after
// api-usage, so on every scan, the first after an apiserver restart
// included, the rows are there. Their rows are kept, and the capability
// comes back partial with self.listed as Skipped: for those resources the
// metric cannot tell other clients from the scanner, and the engine does
// not report them as callers. When api-usage's discovery did not get
// through (self.undiscovered), it cannot say what the scanner lists, and a
// row an earlier scan's LIST left in the gauge would be reported as
// another client's: every row (not of a subresource, which the scanner
// never requests) at one of those group/versions is named in Skipped too,
// for this scan (#239). When discovery did not answer at all (self.blind),
// those are every group/version at which the KB schedules a removal, also
// ones the scanner would not list at on this cluster, so other clients'
// real calls there are withheld for the scan too; the reason says it is
// that broad.
func collectDeprecatedCalls(ctx context.Context, rc rest.Interface, self selfCalls, inv *inventory.Inventory) error {
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
	// Since when the gauge has counted: kube-apiserver exposes its process
	// start time beside it. Whole seconds; left zero when absent, when
	// there is not exactly one series, or when the value is not a time
	// (NaN, ±Inf, not after the epoch, beyond year 5000). It is read from a
	// gauge or untyped series, as client_golang exposes it; any other type
	// is not the standard metric and is treated as absent.
	if fam, ok := families[processStartMetric]; ok && len(fam.GetMetric()) == 1 {
		m := fam.GetMetric()[0]
		sec := m.GetGauge().GetValue()
		if m.GetGauge() == nil {
			sec = m.GetUntyped().GetValue()
		}
		if sec >= 1 && sec < 1e11 {
			inv.APIServerStartTime = time.Unix(int64(sec), 0).UTC()
		}
	}
	fam, ok := families[deprecatedAPIsMetric]
	if !ok {
		return selfRequests(self, nil) // no deprecated API requested since apiserver start
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
	var unattributed []string // rows that may be an earlier scan's own LISTs
	for _, c := range calls {
		gv := schema.GroupVersion{Group: c.Group, Version: c.Version}.String()
		if row := gv + " " + c.Resource; c.Subresource == "" && slices.Contains(self.undiscovered, gv) &&
			!slices.Contains(self.listed, row) && !slices.Contains(unattributed, row) {
			unattributed = append(unattributed, row)
		}
	}
	return selfRequests(self, unattributed)
}

// selfRequests is the outcome of a successful scrape: nil, or, when the
// scanner listed deprecated endpoints itself or rows may be its own, a
// partialError naming them.
func selfRequests(self selfCalls, unattributed []string) error {
	if len(self.listed) == 0 && len(unattributed) == 0 {
		return nil
	}
	var msgs []string
	if len(self.listed) > 0 {
		msgs = append(msgs, fmt.Sprintf("upgradescope lists %s itself (nothing else serves a kind being removed), so the metric cannot show whether other clients request it; apiserver audit logs (annotation k8s.io/deprecated) can",
			strings.Join(self.listed, ", ")))
	}
	switch {
	case len(unattributed) > 0 && self.blind:
		msgs = append(msgs, fmt.Sprintf("API discovery did not answer on this scan, so it is not known what upgradescope lists, and the metric keeps an earlier scan's own LISTs until the apiserver restarts: every row at a group/version where the knowledge base schedules a removal is withheld for this scan, whether or not upgradescope would list there (it lists a deprecated version only where nothing else serves the kind) and whichever client sent it, so %s are not attributed to any client; apiserver audit logs (annotation k8s.io/deprecated) can tell",
			strings.Join(unattributed, ", ")))
	case len(unattributed) > 0:
		var gvs []string
		for _, row := range unattributed {
			if gv, _, _ := strings.Cut(row, " "); !slices.Contains(gvs, gv) {
				gvs = append(gvs, gv)
			}
		}
		msgs = append(msgs, fmt.Sprintf("API discovery did not show what upgradescope lists at %s on this scan, and the metric keeps an earlier scan's own LISTs until the apiserver restarts, so %s may be upgradescope's and are not attributed to other clients; apiserver audit logs (annotation k8s.io/deprecated) can tell",
			strings.Join(gvs, ", "), strings.Join(unattributed, ", ")))
	}
	return partialError{
		msg:        strings.Join(msgs, "; "),
		incomplete: true,
		skipped:    slices.Sorted(slices.Values(append(slices.Clone(self.listed), unattributed...))),
	}
}
