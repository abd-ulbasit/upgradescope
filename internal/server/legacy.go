package server

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// Legacy agents. v0.1.x collectors filled two inventory fields with
// meanings this server no longer judges, without a schema bump (#125
// SV-12, KB-14):
//
//   - api-usage listed every served deprecated version of a resource, so it
//     counted objects the apiserver merely serves there (APF FlowSchemas on
//     1.28) as written through it — removed-API blockers nobody can fix —
//     and those LISTs landed in apiserver_requested_deprecated_apis, so
//     deprecated-calls reported the agent itself as a caller;
//   - a Helm-chart-found add-on's Version was the CHART version (Argo CD
//     chart 7.1.0 is app 2.10), judged against app release lines.
//
// Collectors now stamp inventory.CollectorSchema; before it (v0.1.x, and
// v0.2.0's release candidates) only the push envelope's agentVersion tells
// them apart (legacyInventory). A legacy inventory is judged through
// legacyView: those signals are not assessed (with the reason, so the
// verdict is unknown at best and the operator is told to upgrade the
// agent), and a chart version is kept as evidence only. Stored bytes are
// untouched; a server that learns more can judge them again.

// firstCurrentAgent is the lowest agentVersion whose collectors fill the
// fields with the meanings this server judges: v0.2.0's first
// pre-release.
var firstCurrentAgent = semver.MustParse("0.2.0-0")

// legacyInventory reports whether inv, pushed by an agent reporting
// agentVersion, was collected with v0.1.x field meanings. A stamped
// CollectorSchema is current. An unstamped inventory is legacy unless
// agentVersion is a semantic version (a leading "v" allowed) at or after
// 0.2.0-0: "dev", "" and "unknown" are what a v0.1.x agent built without
// a version reports — its Dockerfile, chart image tag and go install all
// default to "dev" — so they are not taken for newer source (#194).
func legacyInventory(inv inventory.Inventory, agentVersion string) bool {
	if inv.CollectorSchema > 0 {
		return false
	}
	v, err := semver.StrictNewVersion(strings.TrimPrefix(agentVersion, "v"))
	return err != nil || v.LessThan(firstCurrentAgent)
}

// legacyView returns inv as this server can judge it given the agent that
// collected it: unchanged for a current agent's (but for an unmarked one,
// see unattributedUsageView), and for a legacy one with api-usage and
// deprecated-calls not assessed and chart-found add-on versions moved to
// ChartVersion. inv's maps and slices are not modified.
func legacyView(inv inventory.Inventory, agentVersion string) inventory.Inventory {
	// Every snapshot is a cluster inventory: ingest refuses another source
	// (decodePushedInventory), and one stored before it did is judged as
	// one, so it is not excused versions and add-ons (#194).
	if inv.Source != "" && inv.Source != inventory.SourceCluster {
		inv.Source = inventory.SourceCluster
	}
	if !legacyInventory(inv, agentVersion) {
		if inv.CollectorSchema == 0 {
			return unattributedUsageView(inv)
		}
		return inv
	}
	caps := maps.Clone(inv.Capabilities)
	if caps == nil {
		caps = map[inventory.Capability]inventory.CapabilityStatus{}
	}
	caps[inventory.CapAPIUsage] = inventory.CapabilityStatus{Reason: fmt.Sprintf(
		"collected by agent %q, which predates v0.2.0 and counted every object of a kind the apiserver serves at a deprecated version, not the objects written through it; upgrade the agent to assess API usage", agentVersion)}
	caps[inventory.CapDeprecatedCalls] = inventory.CapabilityStatus{Reason: fmt.Sprintf(
		"collected by agent %q, which predates v0.2.0 and whose own requests to deprecated APIs are counted in the apiserver's deprecated-request metric; upgrade the agent to assess deprecated API callers", agentVersion)}
	inv.Capabilities = caps
	inv.APIUsage, inv.DeprecatedCalls = nil, nil

	inv.AddOns = slices.Clone(inv.AddOns)
	for i, a := range inv.AddOns {
		if a.Source != "chart" {
			continue
		}
		// v0.1.x put the chart version here; the app version is unknown.
		if a.ChartVersion == "" {
			inv.AddOns[i].ChartVersion = a.Version
		}
		inv.AddOns[i].Version = ""
	}
	return inv
}

// unattributedUsageView returns an unmarked inventory judged as current
// (agentVersion 0.2.0-0 or later) without the api-usage rows shaped like
// v0.1.x residency data: a count with no object named (#194 KB-14).
// Every collector since v0.2.0-rc.1 names each object it counts (Objects,
// then ObjectsOmitted past the cap), so such a row is one a v0.1.x
// collector would have written — every object the apiserver serves at the
// version — and who writes through it is unknown. It is not judged: the
// API is named in a partial api-usage gap instead, which the engine makes
// required when the target removes it, so the verdict is unknown rather
// than blocked or ready. inv's maps and slices are not modified.
func unattributedUsageView(inv inventory.Inventory) inventory.Inventory {
	st, ok := inv.Capabilities[inventory.CapAPIUsage]
	if !ok || !st.Available {
		return inv
	}
	var kept []inventory.APIUsage
	var unattributed []string
	for _, u := range inv.APIUsage {
		if u.Count > 0 && len(u.Objects) == 0 && u.ObjectsOmitted == 0 {
			gv := u.Version
			if u.Group != "" {
				gv = u.Group + "/" + u.Version
			}
			unattributed = append(unattributed, gv+" "+u.Kind)
			continue
		}
		kept = append(kept, u)
	}
	if len(unattributed) == 0 {
		return inv
	}
	reason := fmt.Sprintf("%d API usage count(s) name no object, as a v0.1.x collector's did, so who writes through the deprecated version is unknown; they were not judged", len(unattributed))
	if st.Reason != "" {
		reason = st.Reason + "; " + reason
	}
	st.Reason, st.Partial = reason, true
	st.Skipped = slices.Compact(slices.Sorted(slices.Values(append(slices.Clone(st.Skipped), unattributed...))))
	caps := maps.Clone(inv.Capabilities)
	caps[inventory.CapAPIUsage] = st
	inv.Capabilities, inv.APIUsage = caps, kept
	return inv
}
