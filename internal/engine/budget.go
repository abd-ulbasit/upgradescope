package engine

import (
	"errors"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// ErrReportTooLarge is EvaluateWithin's error for an inventory whose
// report would be over its size limit.
var ErrReportTooLarge = errors.New("the report would be over its size limit")

// EvaluateWithin is Evaluate for a report of at most maxBytes: it charges
// each finding it builds, and what the report adds besides, at about the
// bytes its strings take (findingSize), and stops with ErrReportTooLarge
// as soon as the charge passes maxBytes. Its findings are built one input
// element at a time, so an evaluation that stops holds at most maxBytes
// of report and the findings of one element. Within the limit the report
// is Evaluate's.
//
// A server evaluates inventories anyone with a push token sends: the
// engine repeats what an inventory names (a team in every finding of its
// namespaces, a release in the text of each of its findings), so a few
// megabytes of a push could build a report many times its size.
func EvaluateWithin(inv inventory.Inventory, k kb.KB, target inventory.Version, now time.Time, maxBytes int) (Report, error) {
	return evaluate(inv, k, target, now, &budget{left: maxBytes})
}

// budget is what is left of an evaluation's size limit; nil is no limit.
type budget struct {
	left int
	over bool
}

// add appends f to *out, with its namespaces capped (capNamespaces), and
// charges it; it reports false, appending nothing, once the findings
// built so far are over the budget, after which the caller returns.
func (b *budget) add(out *[]Finding, f Finding) bool {
	capNamespaces(&f)
	if !b.charge(findingSize(&f)) {
		return false
	}
	*out = append(*out, f)
	return true
}

// addAll is add for each of fs, the findings of a check whose output does
// not grow with the inventory's size.
func (b *budget) addAll(out *[]Finding, fs []Finding) bool {
	for _, f := range fs {
		if !b.add(out, f) {
			return false
		}
	}
	return true
}

// charge takes n bytes from the budget and reports whether it is still
// within it.
func (b *budget) charge(n int) bool {
	if b == nil {
		return true
	}
	b.left -= n
	b.over = b.over || b.left < 0
	return !b.over
}

// exceeded reports whether the budget ran out.
func (b *budget) exceeded() bool { return b != nil && b.over }

// fieldOverhead is about what a field's name and punctuation take in a
// report's JSON.
const fieldOverhead = 16

// findingSize is about how many bytes f takes in a report: its strings
// and a field's overhead for each, a little more for each object.
func findingSize(f *Finding) int {
	n := 8 * fieldOverhead
	for _, s := range []string{string(f.Category), string(f.Severity), f.Key, f.Title, f.Detail, f.Remediation} {
		n += len(s)
	}
	for _, list := range [][]string{f.Teams, f.Namespaces, f.Citations} {
		for _, s := range list {
			n += len(s) + 3
		}
	}
	for _, o := range f.Objects {
		n += 8*fieldOverhead + len(o.Namespace) + len(o.Name) + len(o.File) + len(o.RenderedFrom) + len(o.Manager) + len(o.Ignore) + len(o.IgnoreReason)
	}
	return n
}

// reportBaseSize is findingSize for what a report has besides its
// findings, gaps and unrecognized images.
func reportBaseSize(inv inventory.Inventory, k kb.KB) int {
	return 12*fieldOverhead + len(inv.ClusterID) + len(inv.ServerVersion) + len(k.Version)
}

// gapSize is findingSize for a capability gap.
func gapSize(g *CapabilityGap) int {
	n := 4*fieldOverhead + len(g.Capability) + len(g.Reason)
	for _, s := range g.Skipped {
		n += len(s) + 3
	}
	return n
}
