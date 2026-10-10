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
	if len(a.Matchers.Images) == 0 && len(a.Matchers.Charts) == 0 && len(a.Matchers.Runtimes) == 0 && len(a.Matchers.Components) == 0 {
		errs = append(errs, fmt.Errorf("%s: at least one matcher (matchers.images, matchers.charts, matchers.runtimes or matchers.components) required", a.ID))
	}
	for _, m := range a.Matchers.Images {
		if err := validateImageMatcher(m); err != nil {
			errs = append(errs, fmt.Errorf("%s: matchers.images %q: %w", a.ID, m, err))
		}
	}
	for i, c := range a.Matchers.Components {
		errs = append(errs, validateComponent(fmt.Sprintf("%s: matchers.components[%d] (%s)", a.ID, i, c.Image), c, a.Matchers.Images)...)
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
	errs = append(errs, validateExtendedSupport(a.ID, a.Support)...)
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

// tagPatternPattern is a tag pattern: tag characters and "*" wildcards.
var tagPatternPattern = regexp.MustCompile(`^[A-Za-z0-9_.*-]+$`)

// validateImageMatcher accepts a repository path without registry host, tag
// or digest: the collector matches it on the path, so a host would stop it
// matching mirrors and the legacy k8s.gcr.io. The one exception is a
// provider build, named under its ProviderBuildPrefixes location. The path
// may end in a tag pattern with a "*" ("path:*-hardened*"), never in a tag:
// one tag is one release, not a product.
func validateImageMatcher(m string) error {
	if strings.Contains(m, "@") {
		return fmt.Errorf("must not carry a tag or digest")
	}
	m, pattern, tagged := strings.Cut(m, ":")
	if tagged {
		if err := validateTagPattern(pattern); err != nil {
			return err
		}
		if strings.HasPrefix(m, AnyPrefix) {
			return fmt.Errorf("an any-prefix matcher (%q) takes no tag pattern", AnyPrefix)
		}
	}
	if name, ok := strings.CutPrefix(m, AnyPrefix); ok {
		return validateAnyPrefixMatcher(name)
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

// validateTagPattern checks the part after ":" of an image matcher: tag
// characters with at least one "*" and at least one other character.
func validateTagPattern(p string) error {
	if !strings.Contains(p, "*") {
		return fmt.Errorf("must not carry a tag or digest; a tag pattern (\"path:*-hardened*\") needs a \"*\"")
	}
	if !tagPatternPattern.MatchString(p) || strings.Trim(p, "*") == "" {
		return fmt.Errorf("tag pattern %q must be tag characters (letters, digits, \"_\", \".\", \"-\") with \"*\" wildcards, and not \"*\" alone", p)
	}
	return nil
}

// validateComponent checks a component image: a path-only image matcher
// that no image matcher of the entry also claims (its version would be read
// two ways), and lines mapping MAJOR.MINOR component lines, each once, to
// product release lines, with a citation for the mapping.
func validateComponent(where string, c ComponentImage, images []string) []error {
	var errs []error
	if strings.ContainsAny(c.Image, ":@") || strings.HasPrefix(c.Image, AnyPrefix) {
		errs = append(errs, fmt.Errorf("%s: image must be a repository path without a tag pattern or the any-prefix %q", where, AnyPrefix))
	} else if err := validateImageMatcher(c.Image); err != nil {
		errs = append(errs, fmt.Errorf("%s: image: %w", where, err))
	}
	for _, m := range images {
		if matchersOverlap(m, c.Image) {
			errs = append(errs, fmt.Errorf("%s: image also matches matchers.images %q; list it in one place", where, m))
		}
	}
	if len(c.Lines) == 0 {
		errs = append(errs, fmt.Errorf("%s: at least one line required", where))
	}
	seen := map[string]bool{}
	for j, l := range c.Lines {
		if !k8sVerPattern.MatchString(l.Component) {
			errs = append(errs, fmt.Errorf("%s: lines[%d]: component %q must be MAJOR.MINOR of the image tag, such as \"1.5\"", where, j, l.Component))
		} else if seen[l.Component] {
			errs = append(errs, fmt.Errorf("%s: lines[%d]: duplicate component line %q", where, j, l.Component))
		}
		seen[l.Component] = true
		if !cyclePattern.MatchString(l.Product) {
			errs = append(errs, fmt.Errorf("%s: lines[%d]: product %q must be a dotted release line such as \"2.5\"", where, j, l.Product))
		}
	}
	return append(errs, validateCitations(where, c.Citations)...)
}

// validateExtendedSupport checks split support: extended_eol_date and
// extended_support_condition together, after an eol_date, on a supported
// product (one retired as a whole has no extended window).
func validateExtendedSupport(id string, s Support) []error {
	if s.ExtendedEOLDate == "" && s.ExtendedSupportCondition == "" {
		return nil
	}
	var errs []error
	if s.ExtendedEOLDate == "" || s.ExtendedSupportCondition == "" {
		errs = append(errs, fmt.Errorf("%s: support.extended_eol_date and extended_support_condition go together: the later date holds only under the condition", id))
	}
	if s.Status != "supported" {
		errs = append(errs, fmt.Errorf("%s: support.extended_eol_date requires support.status supported, got %q", id, s.Status))
	}
	if c := s.ExtendedSupportCondition; c != "" && !bareClause(c) {
		errs = append(errs, fmt.Errorf("%s: extended_support_condition %q must be a bare clause with no leading \"if\" and no trailing period (it completes \"only if ...\")", id, c))
	}
	if s.ExtendedEOLDate == "" {
		return errs
	}
	ext, err := time.Parse("2006-01-02", s.ExtendedEOLDate)
	if err != nil {
		return append(errs, fmt.Errorf("%s: extended_eol_date %q must be a valid YYYY-MM-DD date", id, s.ExtendedEOLDate))
	}
	if s.EOLDate == "" {
		return append(errs, fmt.Errorf("%s: support.extended_eol_date requires eol_date, the day support ends without the condition", id))
	}
	if end, err := time.Parse("2006-01-02", s.EOLDate); err == nil && !ext.After(end) {
		errs = append(errs, fmt.Errorf("%s: extended_eol_date %s must be later than eol_date %s", id, s.ExtendedEOLDate, s.EOLDate))
	}
	return errs
}

// bareClause reports whether a condition completes "only if ...": no
// surrounding space, no leading "if", no trailing period.
func bareClause(c string) bool {
	low := strings.ToLower(c)
	return c == strings.TrimSpace(c) && !strings.HasSuffix(c, ".") && !strings.HasPrefix(low, "if ") && !strings.HasPrefix(low, "only if ")
}

// genericImageNames are final path segments many unrelated products publish
// ("controller", "operator", "server", ...). An any-prefix matcher
// ("*/controller") on one would claim every other product's image of that
// name behind any registry, so it is refused.
var genericImageNames = map[string]bool{
	"controller": true, "operator": true, "server": true, "agent": true,
	"proxy": true, "manager": true, "webhook": true,
	"api": true, "app": true, "backend": true, "frontend": true, "web": true,
	"ui": true, "worker": true, "core": true, "node": true, "cli": true,
	"client": true, "daemon": true, "exporter": true, "gateway": true,
	"service": true, "sidecar": true, "init": true, "job": true,
	"runner": true, "scheduler": true, "metrics": true,
}

// validateAnyPrefixMatcher checks the part after "*/" of an any-prefix image
// matcher: one repository segment (so it is a name, not a path or a provider
// location) that is distinctive rather than generic.
func validateAnyPrefixMatcher(name string) error {
	if strings.Contains(name, "/") {
		return fmt.Errorf("an any-prefix matcher (%q) takes one final segment, e.g. %q; write a longer path without it, which already matches under any prefix", AnyPrefix, AnyPrefix+"etcd")
	}
	if !pathSegmentPattern.MatchString(name) {
		return fmt.Errorf("must be a lowercase repository path such as \"ingress-nginx/controller\"")
	}
	if genericImageNames[name] {
		return fmt.Errorf("%q is a generic name that other products publish too, so it cannot match under any registry prefix; use the vendor path (\"cilium/operator\")", name)
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
