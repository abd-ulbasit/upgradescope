// registry/validate.go
package registry

import (
	"cmp"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
)

// SchemaVersion is the only registry schema version this build understands.
const SchemaVersion = 1

var (
	idPattern     = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	k8sVerPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	validStatuses = map[string]bool{"supported": true, "eol": true, "unknown": true}
)

// Validate checks one AddOn against the schema_version 1 rules and returns
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
	if len(a.Matchers.Images) == 0 && len(a.Matchers.Charts) == 0 {
		errs = append(errs, fmt.Errorf("%s: at least one matcher (matchers.images or matchers.charts) required", a.ID))
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
	if a.Support.EOLDate != "" {
		if _, err := time.Parse("2006-01-02", a.Support.EOLDate); err != nil {
			errs = append(errs, fmt.Errorf("%s: eol_date %q must be a valid YYYY-MM-DD date", a.ID, a.Support.EOLDate))
		}
	}
	for i, c := range a.Compat {
		if _, err := semver.NewConstraint(c.Range); err != nil {
			errs = append(errs, fmt.Errorf("%s: compat[%d].range %q: invalid semver constraint: %w", a.ID, i, c.Range, err))
		}
		minOK := k8sVerPattern.MatchString(c.K8sMin)
		maxOK := k8sVerPattern.MatchString(c.K8sMax)
		if !minOK {
			errs = append(errs, fmt.Errorf("%s: compat[%d].k8s_min %q must be MAJOR.MINOR (e.g. \"1.21\")", a.ID, i, c.K8sMin))
		}
		if !maxOK {
			errs = append(errs, fmt.Errorf("%s: compat[%d].k8s_max %q must be MAJOR.MINOR (e.g. \"1.36\")", a.ID, i, c.K8sMax))
		}
		// Cross-check only when both parse — a transposed range like
		// min "1.30" / max "1.21" would match no cluster at all and
		// silently disable the compat row.
		if minOK && maxOK && compareK8sVer(c.K8sMin, c.K8sMax) > 0 {
			errs = append(errs, fmt.Errorf("%s: compat[%d]: k8s_min %q must not exceed k8s_max %q", a.ID, i, c.K8sMin, c.K8sMax))
		}
		if len(c.Citations) == 0 {
			errs = append(errs, fmt.Errorf("%s: compat[%d]: at least one citation required", a.ID, i))
		}
		for _, u := range c.Citations {
			if err := validateCitationURL(u); err != nil {
				errs = append(errs, fmt.Errorf("%s: compat[%d] citation %q: %w", a.ID, i, u, err))
			}
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
	return nil
}
