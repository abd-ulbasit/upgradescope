package server

import (
	"fmt"
	"maps"
	"regexp"
	"slices"

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
// The inventory carries no collector marker, so the push envelope's
// agentVersion decides (legacyAgentVersion). A legacy inventory is judged
// through legacyView: those signals are not assessed (with the reason, so
// the verdict is unknown at best and the operator is told to upgrade the
// agent), and a chart version is kept as evidence only. Stored bytes are
// untouched; a server that learns more can judge them again.

// legacyAgentVersion matches the releases up to v0.1.1 (0.0.x, 0.1.0,
// 0.1.1), with or without a leading "v", and their pre-releases and Go
// pseudo-versions (built before that release). A build from later source
// is not legacy: a 0.1.2-0.<date>-<commit> pseudo-version, a GoReleaser
// 0.1.2-SNAPSHOT, "dev", or no version at all.
var legacyAgentVersion = regexp.MustCompile(`^v?0\.(0\.\d+|1\.[01])([-+].*)?$`)

// legacyAgent reports whether a push's agentVersion is a release whose
// collectors predate the field meanings this server judges.
func legacyAgent(agentVersion string) bool {
	return legacyAgentVersion.MatchString(agentVersion)
}

// legacyView returns inv as this server can judge it given the agent that
// collected it: unchanged for a current agent, and for a legacy one with
// api-usage and deprecated-calls not assessed and chart-found add-on
// versions moved to ChartVersion. inv's maps and slices are not modified.
func legacyView(inv inventory.Inventory, agentVersion string) inventory.Inventory {
	if !legacyAgent(agentVersion) {
		return inv
	}
	caps := maps.Clone(inv.Capabilities)
	if caps == nil {
		caps = map[inventory.Capability]inventory.CapabilityStatus{}
	}
	caps[inventory.CapAPIUsage] = inventory.CapabilityStatus{Reason: fmt.Sprintf(
		"collected by agent %s, which counted every object of a kind the apiserver serves at a deprecated version, not the objects written through it; upgrade the agent to assess API usage", agentVersion)}
	caps[inventory.CapDeprecatedCalls] = inventory.CapabilityStatus{Reason: fmt.Sprintf(
		"collected by agent %s, whose own requests to deprecated APIs are counted in the apiserver's deprecated-request metric; upgrade the agent to assess deprecated API callers", agentVersion)}
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
