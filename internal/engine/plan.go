package engine

import (
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// Hop is one control-plane upgrade of an upgrade plan (see Plan): the
// cluster as it is, judged at To. Findings lists only what this hop adds;
// a finding listed at an earlier hop is referenced instead, in Changed
// when its severity differs from the hop before, else in Carried. So each
// finding is listed in full once, at its first hop, and Findings, Changed
// and Carried together are everything the cluster has to fix before the
// control plane reaches To.
type Hop struct {
	From  inventory.Version `json:"from"`
	To    inventory.Version `json:"to"`
	Score int               `json:"score"` // Evaluate's at To
	// Verdict is Evaluate's at To: blocked when any finding of this hop,
	// new or referenced, is a blocker.
	Verdict  Verdict      `json:"verdict"`
	Findings []Finding    `json:"findings"` // first seen at this hop; Evaluate's order
	Changed  []FindingRef `json:"changed,omitempty"`
	Carried  []FindingRef `json:"carried,omitempty"`
	// NotAssessed is Evaluate's at To, every gap (not only new ones).
	NotAssessed []CapabilityGap `json:"notAssessed,omitempty"`
}

// FindingRef refers to a finding listed in full at an earlier hop, with
// its severity and title at this one.
type FindingRef struct {
	Key      string   `json:"key"`
	Severity Severity `json:"severity"`
	// Was is the severity at the last earlier hop that had the finding;
	// set only in Hop.Changed.
	Was   Severity `json:"was,omitempty"`
	Title string   `json:"title"`
	// Since is the To of the hop that lists the finding in full.
	Since inventory.Version `json:"since"`
}

// Count is the number of this hop's findings, new or referenced, at sev.
func (h Hop) Count(sev Severity) int {
	n := 0
	for _, s := range h.severities() {
		if s == sev {
			n++
		}
	}
	return n
}

// severities maps each finding of the hop, new or referenced, to its
// severity at this hop.
func (h Hop) severities() map[string]Severity {
	out := make(map[string]Severity, len(h.Findings)+len(h.Changed)+len(h.Carried))
	for _, f := range h.Findings {
		out[findingKey(f)] = f.Severity
	}
	for _, r := range h.Changed {
		out[r.Key] = r.Severity
	}
	for _, r := range h.Carried {
		out[r.Key] = r.Severity
	}
	return out
}

// upgradePathKey is the key of evalUpgradePath's info finding. It restates
// the plan, so no hop lists it.
const upgradePathKey = string(CatVersionSkew) + "/upgrade-path"

// Plan judges the cluster at each hop from from to to (HopTargets) and
// returns the hops in order, each finding listed at its first hop only
// (see Hop). It is empty when to is not an upgrade of from. Like Evaluate
// it is pure: the final hop's blockers, score and verdict are Evaluate's
// at to.
func Plan(inv inventory.Inventory, k kb.KB, from, to inventory.Version, now time.Time) []Hop {
	targets := HopTargets(k, from, to)
	reports := make([]Report, len(targets))
	for i, t := range targets {
		reports[i] = Evaluate(inv, k, t, now)
	}
	return PlanReports(from, reports)
}

// HopTargets is the minor the control plane reaches at each hop from from
// to to: from each version, the farthest k.UpgradeSteps step that does not
// pass to, else the next minor. Empty when to is not newer than from (or
// on another major, which Kubernetes has never released).
func HopTargets(k kb.KB, from, to inventory.Version) []inventory.Version {
	targets := []inventory.Version{}
	if from.Major != to.Major {
		return targets
	}
	for at := from; at.Compare(to) < 0; {
		next := at.Next()
		for _, s := range k.UpgradeSteps {
			if s.From == at && s.To.Compare(next) > 0 && s.To.Compare(to) <= 0 {
				next = s.To
			}
		}
		targets = append(targets, next)
		at = next
	}
	return targets
}

// PlanReports builds the hops of a plan from from out of the reports
// judged at each hop's target, in order (Plan evaluates them; the scan
// command passes them through its ignore rules first). A finding is
// identified by its Key.
func PlanReports(from inventory.Version, reports []Report) []Hop {
	type seen struct {
		since inventory.Version
		sev   Severity
	}
	listed := map[string]seen{}
	hops := make([]Hop, 0, len(reports))
	for _, r := range reports {
		h := Hop{From: from, To: r.Target, Score: r.Score, Verdict: r.Verdict, Findings: []Finding{}, NotAssessed: r.NotAssessed}
		for _, f := range r.Findings {
			key := findingKey(f)
			if key == upgradePathKey {
				continue
			}
			prev, ok := listed[key]
			switch {
			case !ok:
				h.Findings = append(h.Findings, f)
				listed[key] = seen{since: r.Target, sev: f.Severity}
				continue
			case prev.sev != f.Severity:
				h.Changed = append(h.Changed, FindingRef{Key: key, Severity: f.Severity, Was: prev.sev, Title: f.Title, Since: prev.since})
			default:
				h.Carried = append(h.Carried, FindingRef{Key: key, Severity: f.Severity, Title: f.Title, Since: prev.since})
			}
			listed[key] = seen{since: prev.since, sev: f.Severity}
		}
		hops = append(hops, h)
		from = r.Target
	}
	return hops
}

// findingKey is f's Key, or for a keyless finding (none today) its
// category and title.
func findingKey(f Finding) string {
	if f.Key != "" {
		return f.Key
	}
	return string(f.Category) + "\x00" + f.Title
}
