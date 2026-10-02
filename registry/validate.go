// registry/validate.go
package registry

import (
	"cmp"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
)

// SchemaVersion is the only registry schema version this build understands.
// Version 2 added per-release-line cycles, runtime matchers, host-less image
// path matchers and single-bound compat rows.
const SchemaVersion = 2

var (
	idPattern     = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	k8sVerPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	cyclePattern  = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)
	validStatuses = map[string]bool{"supported": true, "eol": true, "unknown": true}
	// endoflife.date product slugs: lowercase alphanumerics separated by
	// single dashes or dots (e.g. "argo-cd", "graalvm-ce.17").
	eolSlugPattern = regexp.MustCompile(`^[a-z0-9]+([.-][a-z0-9]+)*$`)
	// One repository path segment as the OCI distribution spec allows it.
	pathSegmentPattern = regexp.MustCompile(`^[a-z0-9]+(([._]|__|-+)[a-z0-9]+)*$`)
	runtimePattern     = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// Validate checks one AddOn against the schema_version 2 rules and returns
// every violation (empty slice means valid).
func Validate(a AddOn) []error {
	var errs []error
	if a.SchemaVersion != SchemaVersion {
		errs = append(errs, fmt.Errorf("%s: schema_version must be %d, got %d", a.ID, SchemaVersion, a.SchemaVersion))
	}
	if a.ID == "" {
		errs = append(errs, fmt.Errorf("id must not be empty"))
	} else if !idPattern.MatchString(a.ID) {
		errs = append(errs, fmt.Errorf("%s: id must be kebab-case (lowercase alphanumerics separated by single dashes)", a.ID))
	}
	if len(a.Matchers.Images) == 0 && len(a.Matchers.Charts) == 0 && len(a.Matchers.Runtimes) == 0 {
		errs = append(errs, fmt.Errorf("%s: at least one matcher (matchers.images, matchers.charts or matchers.runtimes) required", a.ID))
	}
	for _, m := range a.Matchers.Images {
		if err := validateImageMatcher(m); err != nil {
			errs = append(errs, fmt.Errorf("%s: matchers.images %q: %w", a.ID, m, err))
		}
	}
	for _, r := range a.Matchers.Runtimes {
		if !runtimePattern.MatchString(r) {
			errs = append(errs, fmt.Errorf("%s: matchers.runtimes %q must be a runtime name such as \"containerd\"", a.ID, r))
		}
	}
	if !validStatuses[a.Support.Status] {
		errs = append(errs, fmt.Errorf("%s: support.status must be one of supported|eol|unknown, got %q", a.ID, a.Support.Status))
	}
	if a.Support.Status != "unknown" && len(a.Support.Citations) == 0 {
		errs = append(errs, fmt.Errorf("%s: at least one citation required when support.status is %q", a.ID, a.Support.Status))
	}
	for _, c := range a.Support.Citations {
		if err := validateCitationURL(c); err != nil {
			errs = append(errs, fmt.Errorf("%s: support citation %q: %w", a.ID, c, err))
		}
	}
	if a.EndoflifeProduct != "" && !eolSlugPattern.MatchString(a.EndoflifeProduct) {
		errs = append(errs, fmt.Errorf("%s: endoflife_product %q must be a lowercase endoflife.date slug (e.g. \"argo-cd\")", a.ID, a.EndoflifeProduct))
	}
	if a.Support.EOLDate != "" && a.Support.Status == "unknown" {
		// Citations are optional for unknown, so a date here would be an
		// uncited claim, yet the engine turns it into an end-of-life blocker.
		errs = append(errs, fmt.Errorf("%s: support.eol_date requires support.status supported or eol (a date is a claim, and unknown carries no citation)", a.ID))
	}
	if a.Support.EOLDate != "" {
		if _, err := time.Parse("2006-01-02", a.Support.EOLDate); err != nil {
			errs = append(errs, fmt.Errorf("%s: eol_date %q must be a valid YYYY-MM-DD date", a.ID, a.Support.EOLDate))
		}
	}
	seenCycles := map[string]bool{}
	for i, c := range a.Cycles {
		where := fmt.Sprintf("%s: cycles[%d] (%s)", a.ID, i, c.Cycle)
		if !cyclePattern.MatchString(c.Cycle) {
			errs = append(errs, fmt.Errorf("%s: cycle must be a dotted version such as \"1.31\", got %q", where, c.Cycle))
		} else if seenCycles[c.Cycle] {
			errs = append(errs, fmt.Errorf("%s: duplicate cycle %q", where, c.Cycle))
		}
		seenCycles[c.Cycle] = true
		switch {
		case c.EOL == nil:
			errs = append(errs, fmt.Errorf("%s: eol required (a YYYY-MM-DD date, true or false)", where))
		case c.EOL.Date != "":
			if _, err := time.Parse("2006-01-02", c.EOL.Date); err != nil {
				errs = append(errs, fmt.Errorf("%s: eol %q must be a valid YYYY-MM-DD date", where, c.EOL.Date))
			}
		}
		errs = append(errs, validateK8sBounds(where, c.K8sMin, c.K8sMax)...)
		errs = append(errs, validateCitations(where, c.Citations)...)
	}
	for i, c := range a.Compat {
		where := fmt.Sprintf("%s: compat[%d]", a.ID, i)
		if _, err := semver.NewConstraint(c.Range); err != nil {
			errs = append(errs, fmt.Errorf("%s.range %q: invalid semver constraint: %w", where, c.Range, err))
		}
		if c.K8sMin == "" && c.K8sMax == "" {
			errs = append(errs, fmt.Errorf("%s: at least one of k8s_min or k8s_max required", where))
		}
		errs = append(errs, validateK8sBounds(where, c.K8sMin, c.K8sMax)...)
		errs = append(errs, validateCitations(where, c.Citations)...)
	}
	return errs
}

// validateImageMatcher accepts a repository path without registry host, tag
// or digest: the collector matches it on the path, so a host would stop it
// matching mirrors and the legacy k8s.gcr.io. The one exception is a
// provider build, named under its ProviderBuildPrefixes location.
func validateImageMatcher(m string) error {
	if strings.ContainsAny(m, ":@") {
		return fmt.Errorf("must not carry a tag or digest")
	}
	path := m
	for _, p := range ProviderBuildPrefixes {
		if strings.HasPrefix(m, p) {
			path = strings.TrimPrefix(m, p)
			break
		}
	}
	segs := strings.Split(path, "/")
	if len(segs) > 1 && (strings.ContainsAny(segs[0], ".:") || segs[0] == "localhost") {
		return fmt.Errorf("must be a repository path without the registry host (e.g. \"ingress-nginx/controller\"); only provider builds name theirs (%s)",
			strings.Join(ProviderBuildPrefixes, ", "))
	}
	for _, s := range segs {
		if !pathSegmentPattern.MatchString(s) {
			return fmt.Errorf("must be a lowercase repository path such as \"ingress-nginx/controller\"")
		}
	}
	return nil
}

// validateK8sBounds checks optional MAJOR.MINOR bounds; when both are set,
// a transposed range would match no cluster and silently disable the row.
func validateK8sBounds(where, k8sMin, k8sMax string) []error {
	var errs []error
	minOK := k8sMin == "" || k8sVerPattern.MatchString(k8sMin)
	maxOK := k8sMax == "" || k8sVerPattern.MatchString(k8sMax)
	if !minOK {
		errs = append(errs, fmt.Errorf("%s: k8s_min %q must be MAJOR.MINOR (e.g. \"1.21\")", where, k8sMin))
	}
	if !maxOK {
		errs = append(errs, fmt.Errorf("%s: k8s_max %q must be MAJOR.MINOR (e.g. \"1.36\")", where, k8sMax))
	}
	if minOK && maxOK && k8sMin != "" && k8sMax != "" && compareK8sVer(k8sMin, k8sMax) > 0 {
		errs = append(errs, fmt.Errorf("%s: k8s_min %q must not exceed k8s_max %q", where, k8sMin, k8sMax))
	}
	return errs
}

func validateCitations(where string, citations []string) []error {
	if len(citations) == 0 {
		return []error{fmt.Errorf("%s: at least one citation required", where)}
	}
	var errs []error
	for _, u := range citations {
		if err := validateCitationURL(u); err != nil {
			errs = append(errs, fmt.Errorf("%s citation %q: %w", where, u, err))
		}
	}
	return errs
}

// compareK8sVer numerically compares two MAJOR.MINOR strings that already
// matched k8sVerPattern: -1 if a < b, 0 if equal, 1 if a > b.
// Numeric, not lexicographic — "1.9" < "1.21".
func compareK8sVer(a, b string) int {
	amaj, amin := splitK8sVer(a)
	bmaj, bmin := splitK8sVer(b)
	if amaj != bmaj {
		return cmp.Compare(amaj, bmaj)
	}
	return cmp.Compare(amin, bmin)
}

func splitK8sVer(s string) (major, minor int) {
	maj, min, _ := strings.Cut(s, ".")
	major, _ = strconv.Atoi(maj) // pattern-checked: cannot fail
	minor, _ = strconv.Atoi(min)
	return major, minor
}

func validateCitationURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("citation must be an http(s) URL, got scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("citation URL must have a host")
	}
	if host := u.Hostname(); reservedHost(host) {
		return fmt.Errorf("citation host %q is a reserved or local host, not a source: cite the upstream page that states the fact", host)
	}
	return nil
}

// reservedHost reports a host no upstream page can live on: a placeholder
// (the RFC 2606 example names and test, example, invalid TLDs), a local
// or private-network name (.local, .lan, .internal, ...) or an IP address,
// including IPv4 shorthand such as 127.1. The template in
// registry/CONTRIBUTING.md uses one, and a copy that keeps it would pass
// every other rule.
func reservedHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if !strings.Contains(host, ".") { // "localhost", "x"
		return true
	}
	if last := host[strings.LastIndex(host, ".")+1:]; strings.Trim(last, "0123456789") == "" { // 127.1: a numeric TLD is an address shorthand
		return true
	}
	for _, tld := range []string{"test", "example", "invalid", "localhost", "local", "internal", "lan", "localdomain", "home.arpa"} {
		if strings.HasSuffix(host, "."+tld) {
			return true
		}
	}
	for _, name := range []string{"example.com", "example.org", "example.net"} {
		if host == name || strings.HasSuffix(host, "."+name) {
			return true
		}
	}
	return false
}
