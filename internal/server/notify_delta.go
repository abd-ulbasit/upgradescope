package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// Per-notification caps on noisy kinds: the overflow is counted in
// Notification.Omitted instead of listed.
const (
	maxBlockerChanges = 5
	maxEOLChanges     = 5
)

// becameReadyTitle is the became-ready change's title, the same for every
// target so the change merges across them (each target's score is in
// Notification.Targets).
const becameReadyTitle = "ready: all blockers resolved"

// ComputeDelta implements the notification delta rules for one target:
//
//   - prev == nil (first-ever evaluation of this cluster+target) → nothing.
//   - Blocker findings added since prev (diff by stable finding key) → one
//     new-blocker change each.
//   - Blocker count went >0 → 0 AND curr.Verdict is ready → one became-ready
//     change. Zero blockers with verdict unknown (a required check was not
//     assessed, e.g. a transient collector failure) is not readiness, and
//     neither is a pass that could not see a blocker of prev (computeDelta).
//   - eol-approaching warnings added since prev (by key) → one change each.
//
// Identity is Finding.Key — deliberately count-free, so a title-only change
// ("3 objects" → "2 objects") never re-alerts the same blocker. Findings
// with an empty Key (reports stored before Key existed) fall back to Title.
// Duplicate keys within one report give one change (first occurrence wins).
//
// Every change lists curr's target; buildNotification merges and caps the
// changes of one pass across targets. Order is deterministic: new-blockers
// in report order, became-ready, eol-approaching in report order.
func ComputeDelta(prev *engine.Report, curr engine.Report) []notify.Change {
	if prev == nil {
		return nil
	}
	heads := make([]findingHead, len(prev.Findings))
	for i, f := range prev.Findings {
		heads[i] = headOf(f)
	}
	changes, _ := computeDelta(heads, curr, nil)
	return changes
}

// computeDelta is ComputeDelta against the heads of the baseline's
// findings, the ones it carries forward included (storedHeads): all it
// reads of them. unassessed reports which of them curr could not have
// seen (nil: none), because a capability they come from was not assessed
// in curr but was in the pass that last saw them (unassessedIn). A
// blocker or eol-approaching finding of prev that is gone from curr but
// unassessed is carried, with what it was seen without: neither resolved
// by this pass nor news when its capability returns, and no became-ready
// while a carried blocker remains. Nor is a deprecated caller that folds
// into a carried usage finding (engine.FoldsInto) news: it is that
// finding, seen without api-usage. carried is what the new evaluation's
// baseline keeps of prev, until a pass that assessed it shows it gone.
func computeDelta(prev []findingHead, curr engine.Report, unassessed func(findingHead) bool) (changes []notify.Change, carried []findingHead) {
	target := []string{curr.Target.String()}

	prevBlockers := headKeys(prev, findingHead.blocker)
	prevEOL := headKeys(prev, findingHead.eolApproaching)

	if unassessed != nil {
		present := keySet(curr.Findings, func(engine.Finding) bool { return true })
		kept := map[string]bool{}
		for _, h := range prev {
			sig := findingSignature(h)
			if (!h.blocker() && !h.eolApproaching()) || present[h.key()] || kept[sig] || !unassessed(h) {
				continue
			}
			kept[sig] = true
			carried = append(carried, h)
		}
	}

	currBlockerCount := 0
	seenBlockers := map[string]bool{}
	for _, f := range curr.Findings {
		if !isBlocker(f) {
			continue
		}
		currBlockerCount++
		k := findingKey(f)
		if seenBlockers[k] {
			continue
		}
		seenBlockers[k] = true
		// A caller folded into a usage finding of prev splits out of it
		// when api-usage did not see that API: the carried finding,
		// seen through the /metrics scrape.
		if !prevBlockers[k] && !slices.ContainsFunc(carried, func(h findingHead) bool { return engine.FoldsInto(k, h.key()) }) {
			changes = append(changes, change(notify.KindNewBlocker, f, target))
		}
	}

	if len(prevBlockers) > 0 && currBlockerCount == 0 && curr.Verdict == engine.VerdictReady &&
		!slices.ContainsFunc(carried, findingHead.blocker) {
		changes = append(changes, notify.Change{Kind: notify.KindBecameReady, Title: becameReadyTitle, Targets: target})
	}

	seenEOL := map[string]bool{}
	for _, f := range curr.Findings {
		if !isEOLApproaching(f) {
			continue
		}
		k := findingKey(f)
		if seenEOL[k] {
			continue
		}
		seenEOL[k] = true
		if !prevEOL[k] {
			changes = append(changes, change(notify.KindEOLApproaching, f, target))
		}
	}
	return changes, carried
}

// upgradeLookback is how many minors below a new default target
// upgradeBaseline looks for the cluster's previous one: one for an
// upgrade the agent pushed on both sides of, more when it missed a push
// across several upgrades.
const upgradeLookback = 3

// upgradeBaseline is the notification baseline of a default target with
// no decided evaluation yet, because the cluster upgraded (1.35 → 1.36
// makes the default target 1.37): the latest decided evaluation of the
// nearest lower target within upgradeLookback minors, which was the
// default target before the upgrade. So a blocker the new target adds
// (an API removed in 1.37) is announced, and blockers the cluster had
// already been told about at the old target are not announced again.
//
// The other candidate, the previous snapshot evaluated at the new
// target, would hide exactly the blockers that are news: the cluster
// already used that API before the upgrade.
//
// Only the default target (the next minor above cur.ServerVersion) has
// one. An extra target's first evaluation stays the silent baseline: a
// target added to --targets is a new question, and announcing it would
// page every cluster in the fleet on the restart. ErrNotFound when there
// is no baseline.
func (s *Server) upgradeBaseline(ctx context.Context, clusterID int64, cur engine.Report) (store.Evaluation, error) {
	server, err := inventory.ParseVersion(cur.ServerVersion)
	if err != nil || server.Next() != cur.Target {
		return store.Evaluation{}, store.ErrNotFound
	}
	for minor := cur.Target.Minor - 1; minor >= max(cur.Target.Minor-upgradeLookback, 0); minor-- {
		prev, err := s.cfg.Store.LatestKnownEvaluation(ctx, clusterID, inventory.Version{Major: cur.Target.Major, Minor: minor}.String())
		if !errors.Is(err, store.ErrNotFound) {
			return prev, err
		}
	}
	return store.Evaluation{}, store.ErrNotFound
}

func change(kind string, f engine.Finding, targets []string) notify.Change {
	return notify.Change{Kind: kind, Key: f.Key, Severity: string(f.Severity), Title: f.Title, Detail: f.Detail, Targets: targets}
}

// targetDelta is one target's verdict after a pass and its changes, and
// what its evaluation's baseline carries forward (computeDelta).
type targetDelta struct {
	target  notify.Target
	changes []notify.Change
	carried []findingHead
}

// kindRank orders a notification's changes: blockers first.
var kindRank = map[string]int{notify.KindNewBlocker: 0, notify.KindBecameReady: 1, notify.KindEOLApproaching: 2}

// kindCap is the per-notification cap of a kind (0 = none).
var kindCap = map[string]int{notify.KindNewBlocker: maxBlockerChanges, notify.KindEOLApproaching: maxEOLChanges}

// buildNotification groups one pass's deltas for one cluster into a single
// notification: a change found for several targets with the same wording
// (an EOL add-on is a blocker for each of them) becomes one change listing
// them all, then
// new-blocker and eol-approaching changes are capped, the rest counted in
// Omitted. ok is false when the pass changed nothing.
func buildNotification(cluster store.Cluster, deltas []targetDelta, now time.Time, deliveryID string) (n notify.Notification, ok bool) {
	var m changeMerger
	for _, d := range deltas {
		m.add(d)
	}
	return m.notification(cluster, now, deliveryID)
}

// changeMerger is buildNotification one target at a time, so a pass holds
// what its notification lists, not every change of every target: a
// knowledge-base update can make every finding of a report at the limit
// news, for each target. A change past its kind's cap is kept as the
// digest of its identity only, so the same change for a later target is
// still counted once.
type changeMerger struct {
	targets []notify.Target
	changes []notify.Change
	listed  map[[sha256.Size]byte]int      // identity → index in changes
	omitted map[[sha256.Size]byte]struct{} // identities past their kind's cap
	count   map[string]int                 // distinct changes of each kind
	counted map[string]int                 // of those, omitted
}

// add merges one target's delta: in target order, which the merged
// changes keep.
func (m *changeMerger) add(d targetDelta) {
	if len(d.changes) == 0 {
		return
	}
	if m.listed == nil {
		m.listed, m.omitted, m.count = map[[sha256.Size]byte]int{}, map[[sha256.Size]byte]struct{}{}, map[string]int{}
	}
	m.targets = append(m.targets, d.target)
	for _, c := range d.changes {
		// Same finding, same wording: one change for all its targets.
		// A title or detail that names the target keeps a change per
		// target, so no target is described in another's words.
		id := changeIdentity(c)
		if i, seen := m.listed[id]; seen {
			m.changes[i].Targets = append(m.changes[i].Targets, c.Targets...)
			continue
		}
		if _, seen := m.omitted[id]; seen {
			continue
		}
		m.count[c.Kind]++
		if limit := kindCap[c.Kind]; limit > 0 && m.count[c.Kind] > limit {
			m.omitted[id] = struct{}{}
			if m.counted == nil {
				m.counted = map[string]int{}
			}
			m.counted[c.Kind]++
			continue
		}
		m.listed[id] = len(m.changes)
		c.Targets = slices.Clone(c.Targets)
		m.changes = append(m.changes, c)
	}
}

// changeIdentity digests what makes two targets' changes one: kind, key,
// title and detail.
func changeIdentity(c notify.Change) [sha256.Size]byte {
	h := sha256.New()
	for _, part := range []string{c.Kind, c.Key, c.Title, c.Detail} {
		_, _ = io.WriteString(h, part) // a hash.Hash never fails a write
		_, _ = h.Write([]byte{0})
	}
	var id [sha256.Size]byte
	h.Sum(id[:0])
	return id
}

// notification is the merged changes as one notification, blockers
// first; ok is false when no target changed. It ends the merge.
func (m *changeMerger) notification(cluster store.Cluster, now time.Time, deliveryID string) (n notify.Notification, ok bool) {
	if len(m.changes) == 0 {
		return notify.Notification{}, false
	}
	slices.SortStableFunc(m.changes, func(a, b notify.Change) int { return cmp.Compare(kindRank[a.Kind], kindRank[b.Kind]) })
	n = notify.Notification{
		SchemaVersion: notify.SchemaVersion,
		DeliveryID:    deliveryID,
		Type:          notify.TypeReadinessChanged,
		Timestamp:     now.UTC(),
		Cluster:       notify.Cluster{ID: cluster.ID, Name: cluster.Name},
		Targets:       m.targets,
		Changes:       m.changes,
	}
	if len(m.counted) > 0 {
		n.Omitted = m.counted
	}
	return n, true
}

// newDeliveryID returns a random 128-bit id, hex: the notification's
// identity across retries and sinks.
func newDeliveryID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24+)
	return hex.EncodeToString(b[:])
}

func isBlocker(f engine.Finding) bool { return headOf(f).blocker() }

func isEOLApproaching(f engine.Finding) bool { return headOf(f).eolApproaching() }

// findingKey is the delta identity: the stable count-free Key when set,
// else Title — a defensive fallback for reports stored before Finding.Key
// existed (those deltas keep the old title-diff behavior).
func findingKey(f engine.Finding) string { return headOf(f).key() }

// findingHead is what a notification delta and sameResult read of a
// stored finding: its identity, severity and category. A stored report
// is up to maxReportBytes, and decoded whole its findings take several
// times that (their objects, namespaces and evidence); its heads take a
// fraction of it.
type findingHead struct {
	Category engine.Category `json:"category"`
	Severity engine.Severity `json:"severity"`
	Key      string          `json:"key,omitempty"`
	Title    string          `json:"title"` // kept only when Key is empty: the identity then
	// SeenWithout is, of a carried finding, the capabilities the pass
	// that last saw it did not assess for it (engine.HiddenBy): their
	// gaps cannot hide it, since that pass found it without them. Of a
	// report's own finding, storedHeads.baseline fills it in from the
	// report's NotAssessed.
	SeenWithout []inventory.Capability `json:"seenWithout,omitempty"`
}

func headOf(f engine.Finding) findingHead {
	return findingHead{Category: f.Category, Severity: f.Severity, Key: f.Key, Title: f.Title}
}

func (h findingHead) blocker() bool { return h.Severity == engine.SevBlocker }

func (h findingHead) eolApproaching() bool {
	return h.Category == engine.CatEOLApproaching && h.Severity == engine.SevWarning
}

// key is findingKey of the finding.
func (h findingHead) key() string {
	if h.Key != "" {
		return h.Key
	}
	return h.Title
}

// storedHeads is what the server reads of a stored report: its findings'
// heads, the capabilities it did not assess, and the heads of the
// findings its notification baseline carries forward, which the report
// does not have (computeDelta).
//
// carriedForward is the server's own: withCarried adds it to the encoded
// report it stores, and it is never served, because every read decodes
// the stored report into an engine.Report.
type storedHeads struct {
	Findings       []findingHead          `json:"findings"`
	NotAssessed    []engine.CapabilityGap `json:"notAssessed,omitempty"`
	CarriedForward []findingHead          `json:"carriedForward,omitempty"`
}

// baseline is the stored report as a notification baseline: its findings,
// each with the capabilities the report found it without (SeenWithout),
// and what it carries forward, which keeps those of the pass that last
// saw it.
func (h storedHeads) baseline() []findingHead {
	out := make([]findingHead, 0, len(h.Findings)+len(h.CarriedForward))
	for _, f := range h.Findings {
		f.SeenWithout = engine.HiddenBy(h.NotAssessed, f.Category, f.key())
		out = append(out, f)
	}
	return append(out, h.CarriedForward...)
}

// storedFindingHeads decodes a stored report's storedHeads, and nothing
// else of it.
func storedFindingHeads(report []byte) (storedHeads, error) {
	var r storedHeads
	if err := json.Unmarshal(report, &r); err != nil {
		return storedHeads{}, err
	}
	for _, hs := range [][]findingHead{r.Findings, r.CarriedForward} {
		for i := range hs {
			if hs[i].Key != "" {
				hs[i].Title = "" // not the identity: let it go
			}
		}
	}
	return r, nil
}

// withCarried adds carried to report, an encoded engine.Report, as its
// carriedForward field (storedHeads). The report is not decoded again:
// carried goes in before its closing brace.
func withCarried(report []byte, carried []findingHead) ([]byte, error) {
	if len(carried) == 0 {
		return report, nil
	}
	if len(report) < 2 || report[0] != '{' || report[len(report)-1] != '}' {
		return nil, errors.New("report is not a JSON object")
	}
	b, err := json.Marshal(carried)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(report)+len(b)+len(`,"carriedForward":`))
	out = append(out, report[:len(report)-1]...)
	out = append(out, `,"carriedForward":`...)
	out = append(out, b...)
	return append(out, '}'), nil
}

// sameHeads reports whether a and b hold the same (severity, key) pairs.
func sameHeads(a, b []findingHead) bool {
	sig := func(hs []findingHead) []string {
		out := make([]string, 0, len(hs))
		for _, h := range hs {
			out = append(out, findingSignature(h))
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	return slices.Equal(sig(a), sig(b))
}

func headKeys(hs []findingHead, keep func(findingHead) bool) map[string]bool {
	s := make(map[string]bool)
	for _, h := range hs {
		if keep(h) {
			s[h.key()] = true
		}
	}
	return s
}

func keySet(fs []engine.Finding, keep func(engine.Finding) bool) map[string]bool {
	s := make(map[string]bool)
	for _, f := range fs {
		if keep(f) {
			s[findingKey(f)] = true
		}
	}
	return s
}
