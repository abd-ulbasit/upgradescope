package engine

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/registry"
)

// SupportPhase is where a managed cluster's Kubernetes minor stands in its
// provider's support calendar.
type SupportPhase string

const (
	// SupportStandard: standard support, and it does not end within
	// supportWarnDays.
	SupportStandard SupportPhase = "standard"
	// SupportEnding: standard support ends within supportWarnDays; the
	// finding is a warning.
	SupportEnding SupportPhase = "ending"
	// SupportExtended: past standard support, before the provider stops
	// supporting the minor (EKS and GKE bill it; AKS's is platform
	// support); the finding is a blocker.
	SupportExtended SupportPhase = "extended"
	// SupportEnded: past the end of extended support, or past standard
	// support for a minor the provider offered no extended support for;
	// the finding is a blocker.
	SupportEnded SupportPhase = "ended"
)

// supportWarnDays is how long before a minor leaves standard support the
// finding starts to warn: the 90 days the add-on EOL warning uses, long
// enough to schedule an upgrade (two or three minors a year) and to put a
// dated cost in front of whoever approves the spend.
const supportWarnDays = 90

// hoursPerYear prices a year of cluster-hours: 365 days, the issue's
// (extended - standard) x 8760.
const hoursPerYear = 8760

// SupportStatus is a managed cluster's place in its provider's support
// calendar, set whenever the provider and the minor are known, whether or
// not it is a finding. Evaluate leaves Report.Support nil for a cluster
// whose provider is unknown, other, or has no dates for its minor.
type SupportStatus struct {
	Provider string       `json:"provider"` // eks | gke | aks
	Minor    string       `json:"minor"`    // the control plane's, "1.34"
	Phase    SupportPhase `json:"phase"`
	// ExtendedSupportFrom is the day standard support ends and extended
	// support begins (UTC; EKS bills from the start of that day).
	ExtendedSupportFrom string `json:"extendedSupportFrom"`
	// ExtendedSupportEnds is the day extended support ends; empty when the
	// provider offered none for the minor.
	ExtendedSupportEnds string `json:"extendedSupportEnds,omitempty"`
	// AnnualCostDelta is what extended support adds per cluster per year
	// at the provider's list price, (extended - standard) x 8760 in
	// Currency, as a decimal string ("4380.00"). Set only when the provider
	// publishes a price the dataset cites and the minor can still be in
	// extended support; PriceAsOf is the day the price was read. It is a
	// list price, not a bill.
	AnnualCostDelta string `json:"annualCostDelta,omitempty"`
	Currency        string `json:"currency,omitempty"`
	PriceAsOf       string `json:"priceAsOf,omitempty"`
}

// Summary is the status in one line for the scan table: the dates, and the
// annual extended-support cost labelled as a list price with its as-of day
// where the provider's price is cited.
func (s SupportStatus) Summary() string {
	head := strings.ToUpper(s.Provider) + " " + s.Minor + ": "
	cost := ""
	if s.AnnualCostDelta != "" {
		if cents, err := strconv.ParseInt(strings.Replace(s.AnnualCostDelta, ".", "", 1), 10, 64); err == nil {
			cost = fmt.Sprintf(" %s/yr per cluster (list price as of %s)", formatUSD(cents), s.PriceAsOf)
		}
	}
	switch s.Phase {
	case SupportExtended:
		if cost != "" {
			cost = "; adds" + cost
		}
		return fmt.Sprintf("%sin extended support since %s (until %s)%s", head, s.ExtendedSupportFrom, s.ExtendedSupportEnds, cost)
	case SupportEnded:
		if s.ExtendedSupportEnds == "" {
			return fmt.Sprintf("%sout of support (standard support ended %s)", head, s.ExtendedSupportFrom)
		}
		return fmt.Sprintf("%sout of support (extended support ended %s)", head, s.ExtendedSupportEnds)
	}
	if s.ExtendedSupportEnds == "" {
		return fmt.Sprintf("%sstandard support ends %s, no extended support offered", head, s.ExtendedSupportFrom)
	}
	if cost != "" {
		cost = "; extended support adds" + cost
	}
	return fmt.Sprintf("%sstandard support ends %s, extended support until %s%s", head, s.ExtendedSupportFrom, s.ExtendedSupportEnds, cost)
}

// evalSupport places the cluster's control-plane minor in its provider's
// support calendar and, once standard support is ending or over, reports
// it: a warning from supportWarnDays before the day extended support
// begins, a blocker from that day. The status is returned in every phase;
// it is nil, with no finding, when the provider is unknown or other, the
// server version does not parse, or the dataset has no dates for the
// minor: nothing is inferred.
func evalSupport(inv inventory.Inventory, k kb.KB, now time.Time) (*SupportStatus, []Finding) {
	if inv.Source == inventory.SourceFiles {
		return nil, nil
	}
	p, ok := k.Provider(string(inv.Provider))
	if !ok {
		return nil, nil
	}
	server, err := inventory.ParseVersion(inv.ServerVersion)
	if err != nil {
		return nil, nil
	}
	minor := server.String()
	i := slices.IndexFunc(p.Versions, func(w registry.SupportWindow) bool { return w.Minor == minor })
	if i < 0 {
		return nil, nil
	}
	w := p.Versions[i]
	from, err := time.Parse("2006-01-02", w.StandardEnd)
	if err != nil {
		return nil, nil
	}
	var ends time.Time
	if w.ExtendedEnd != "" {
		if ends, err = time.Parse("2006-01-02", w.ExtendedEnd); err != nil {
			return nil, nil
		}
	}

	st := &SupportStatus{Provider: p.ID, Minor: minor, ExtendedSupportFrom: w.StandardEnd, ExtendedSupportEnds: w.ExtendedEnd}
	switch {
	case now.Before(from.AddDate(0, 0, -supportWarnDays)):
		st.Phase = SupportStandard
	case now.Before(from):
		st.Phase = SupportEnding
	case w.ExtendedEnd == "" || !now.Before(ends):
		st.Phase = SupportEnded
	default:
		st.Phase = SupportExtended
	}
	cost := ""
	if pr := p.Pricing; pr != nil && w.ExtendedEnd != "" && st.Phase != SupportEnded {
		cents := int64(math.Round((pr.ExtendedPerClusterHour - pr.StandardPerClusterHour) * hoursPerYear * 100))
		st.AnnualCostDelta = fmt.Sprintf("%d.%02d", cents/100, cents%100)
		st.Currency, st.PriceAsOf = pr.Currency, pr.AsOf
		cost = costSentence(*pr, cents)
	}
	if st.Phase == SupportStandard {
		return st, nil
	}

	f := Finding{
		Category:  CatSupportLifecycle,
		Key:       string(CatSupportLifecycle) + "/" + p.ID + "/" + minor,
		Citations: slices.Clone(p.Citations),
	}
	if st.AnnualCostDelta != "" {
		f.Citations = append(f.Citations, p.Pricing.Citations...)
	}
	extNote := ""
	if p.ExtendedSupportNote != "" {
		extNote = " " + p.ExtendedSupportNote
	}
	switch st.Phase {
	case SupportEnding:
		f.Severity = SevWarning
		f.Title = fmt.Sprintf("Kubernetes %s leaves %s standard support on %s", minor, p.DisplayName, w.StandardEnd)
		after := fmt.Sprintf("the cluster then moves to extended support until %s, when %s stops supporting it", w.ExtendedEnd, p.DisplayName)
		if w.ExtendedEnd == "" {
			after = fmt.Sprintf("%s offers no extended support for it", p.DisplayName)
		}
		f.Detail = fmt.Sprintf("%s ends standard support for Kubernetes %s on %s; %s.%s%s", p.DisplayName, minor, w.StandardEnd, after, cost, extNote)
	case SupportExtended:
		f.Severity = SevBlocker
		f.Title = fmt.Sprintf("Kubernetes %s is past %s standard support (ended %s)", minor, p.DisplayName, w.StandardEnd)
		f.Detail = fmt.Sprintf("%s ended standard support for Kubernetes %s on %s; the cluster is in extended support until %s, when %s stops supporting it.%s%s",
			p.DisplayName, minor, w.StandardEnd, w.ExtendedEnd, p.DisplayName, cost, extNote)
	default: // SupportEnded
		f.Severity = SevBlocker
		if w.ExtendedEnd == "" {
			f.Title = fmt.Sprintf("Kubernetes %s is out of %s support (standard support ended %s)", minor, p.DisplayName, w.StandardEnd)
			f.Detail = fmt.Sprintf("%s ended standard support for Kubernetes %s on %s and offered no extended support for it.%s", p.DisplayName, minor, w.StandardEnd, extNote)
		} else {
			f.Title = fmt.Sprintf("Kubernetes %s is out of %s support (extended support ended %s)", minor, p.DisplayName, w.ExtendedEnd)
			f.Detail = fmt.Sprintf("%s ended standard support for Kubernetes %s on %s and extended support on %s.%s", p.DisplayName, minor, w.StandardEnd, w.ExtendedEnd, extNote)
		}
	}
	f.Remediation = supportRemediation(p, server, now)
	return st, []Finding{f}
}

// costSentence states the annual extended-support delta as a list price,
// with the prices it comes from and the day they were read, so it cannot
// be taken for the customer's bill.
func costSentence(pr registry.Pricing, cents int64) string {
	s := fmt.Sprintf(" Extended support costs %s more per cluster per year at list price ($%s against $%s per cluster-hour, over 8,760 hours; list price as of %s, not your bill).",
		formatUSD(cents), priceText(pr.ExtendedPerClusterHour), priceText(pr.StandardPerClusterHour), pr.AsOf)
	if pr.Note != "" {
		s += " It is " + pr.Note + "."
	}
	return s
}

// supportRemediation names the Kubernetes minors newer than the cluster's
// that are still in standard support (at most three, newest first) among
// the ones the dataset knows.
func supportRemediation(p registry.ProviderSupport, current inventory.Version, now time.Time) string {
	type minorEnd struct {
		v   inventory.Version
		raw string
	}
	var newer []minorEnd
	for _, w := range p.Versions {
		v, err := inventory.ParseVersion(w.Minor)
		end, derr := time.Parse("2006-01-02", w.StandardEnd)
		if err != nil || derr != nil || v.Compare(current) <= 0 || !now.Before(end) {
			continue
		}
		newer = append(newer, minorEnd{v, w.Minor})
	}
	slices.SortFunc(newer, func(a, b minorEnd) int { return b.v.Compare(a.v) })
	if len(newer) == 0 {
		return "Upgrade the control plane to a Kubernetes minor that is still in standard support."
	}
	names := make([]string, 0, 3)
	for _, m := range newer[:min(3, len(newer))] {
		names = append(names, m.raw)
	}
	return fmt.Sprintf("Upgrade the control plane, one minor at a time, to a version in standard support: %s.", strings.Join(names, ", "))
}

// priceText renders a per-hour price with at least two decimals: 0.6 as
// "0.60", 0.125 as "0.125".
func priceText(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	switch i := strings.IndexByte(s, '.'); {
	case i < 0:
		return s + ".00"
	case len(s)-i-1 < 2:
		return s + "0"
	}
	return s
}

// formatUSD renders cents as dollars with thousands separators; the cents
// are shown only when there are some: 438000 as "$4,380", 438050 as
// "$4,380.50".
func formatUSD(cents int64) string {
	dollars, rem := cents/100, cents%100
	digits := strconv.FormatInt(dollars, 10)
	var b strings.Builder
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if rem == 0 {
		return "$" + b.String()
	}
	return fmt.Sprintf("$%s.%02d", b.String(), rem)
}
