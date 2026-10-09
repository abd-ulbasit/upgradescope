package server

import (
	"fmt"
	"maps"
	"regexp"
	"slices"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// Knowledge base skew (#268). An agent collects evidence by its own
// knowledge base: it lists API usage only for the group/version/kinds its
// lifecycle data flags (and reads a Helm release's stored manifest for the
// same), and it matches add-ons against its own registry, sending what no
// entry claims as untagged image repositories only. The server judges what
// it is sent with its own knowledge base, and a snapshot is re-judged
// whenever that changes ("agents and servers can be upgraded in either
// order"), but it cannot re-collect: an API its lifecycle data flags and the
// agent's did not was never listed, and an add-on its registry knows and the
// agent's did not was never matched (a retired product such as weave-net is
// a blocker at any version). Judging that evidence as complete read "ready"
// on a cluster the server's own knowledge base would block.
//
// The push envelope's kbVersion names the datasets an agent collected by
// ("<generatedFrom>; lifecycle <digest>; registry <digest>", kb.Version), so
// no evidence is added to the inventory and its size stays what it was.
// kbSkewView compares the digests with the server's own: where the lifecycle
// digests differ, api-usage (and the Helm manifests read by the same data)
// is partial over what the server's data would have collected, and where the
// registry digests differ, addons is. Both name inventory.SkippedNewerKB,
// which the engine makes a required gap for api-usage and addons: the
// verdict is unknown, never ready, until the agent and server run the same
// data. Evidence is complete only when the digests are the same; a digest
// cannot say which side is newer, so an agent with newer data than its
// server is read the same way (the server is the one to upgrade then).
//
// A server whose own version is not in the form (a test knowledge base)
// cannot compare and judges as before. An agent's kbVersion that is not in
// the form names no datasets at all, so its evidence is not known to be
// complete either.

var kbVersionRe = regexp.MustCompile(`; lifecycle ([0-9a-f]{8}); registry ([0-9a-f]{8})$`)

// kbDigests returns the lifecycle and registry digests of a kb.Version
// label; ok is false for a label not in that form.
func kbDigests(version string) (lifecycle, registry string, ok bool) {
	m := kbVersionRe.FindStringSubmatch(version)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// judgedView returns inv as this server judges a snapshot pushed by
// agentVersion, with knowledge base kbVersion: legacyView, then kbSkewView.
func judgedView(inv inventory.Inventory, agentVersion, kbVersion string, k kb.KB) inventory.Inventory {
	return kbSkewView(legacyView(inv, agentVersion), kbVersion, k)
}

// kbSkewView returns inv with the capabilities its agent's knowledge base
// left partial marked so. inv's maps and slices are not modified.
func kbSkewView(inv inventory.Inventory, agentKB string, k kb.KB) inventory.Inventory {
	serverLifecycle, serverRegistry, ok := kbDigests(k.Version)
	if !ok {
		return inv
	}
	agentLifecycle, agentRegistry, known := kbDigests(agentKB)
	lifecycleSkew := !known || agentLifecycle != serverLifecycle
	registrySkew := !known || agentRegistry != serverRegistry
	if !lifecycleSkew && !registrySkew {
		return inv
	}
	what := func(digest, server string) string {
		if !known {
			return fmt.Sprintf("the agent's knowledge base (%q) names no dataset digests", agentKB)
		}
		return fmt.Sprintf("the agent collected with %s, this server judges with %s", digest, server)
	}
	caps := maps.Clone(inv.Capabilities)
	mark := func(c inventory.Capability, reason string) {
		st, ok := caps[c]
		if !ok || !st.Available {
			return // not read at all: already a gap of its own
		}
		if st.Reason != "" {
			st.Reason += "; "
		}
		st.Reason += reason
		st.Partial = true
		st.Skipped = slices.Compact(slices.Sorted(slices.Values(append(slices.Clone(st.Skipped), inventory.SkippedNewerKB))))
		caps[c] = st
	}
	if lifecycleSkew {
		why := "knowledge base skew: lifecycle data differs (" + what(agentLifecycle, serverLifecycle) + "), so APIs this server flags that the agent's did not were never listed; upgrade the agent to the server's version to assess them"
		mark(inventory.CapAPIUsage, why)
		mark(inventory.CapHelm, "knowledge base skew: lifecycle data differs ("+what(agentLifecycle, serverLifecycle)+"), so a release's stored manifest was read only for the APIs the agent's flagged; upgrade the agent to the server's version")
	}
	if registrySkew {
		mark(inventory.CapAddOns, "knowledge base skew: add-on registry differs ("+what(agentRegistry, serverRegistry)+"), so add-ons this server's registry knows that the agent's did not were never matched, and an image no entry of the agent's claimed is listed untagged only; upgrade the agent to the server's version to assess them")
	}
	inv.Capabilities = caps
	return inv
}
