package inventory

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Version is a Kubernetes minor version (e.g. 1.34). Patch is ignored for evaluation.
type Version struct{ Major, Minor int }

// ParseVersion parses a Kubernetes version string. Accepted forms:
// "1.34", "v1.34", "v1.34.2", "1.34.2", and — for versions observed on real
// clusters — MAJOR.MINOR.PATCH followed by a semver pre-release and/or build
// suffix, as vendors report in GitVersion: "v1.30.2-eks-1234abc" (EKS),
// "v1.29.4-gke.1043002" (GKE), "v1.28.5+k3s1" (k3s), "v1.27.3+rke2r1" (RKE2),
// "v1.29.5+29a0aa9" (OpenShift). The patch and suffix are validated but
// discarded — evaluation only cares about minors.
//
// The major version is not range-checked here: callers validating user
// input (a --target flag, a CRD spec target) additionally require major 1.
func ParseVersion(s string) (Version, error) {
	trimmed := strings.TrimPrefix(s, "v")
	if trimmed == "" {
		return Version{}, fmt.Errorf("invalid kubernetes version %q: empty", s)
	}
	core, suffix := trimmed, ""
	if i := strings.IndexAny(trimmed, "-+"); i >= 0 {
		core, suffix = trimmed[:i], trimmed[i:]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 2 && len(parts) != 3 {
		return Version{}, fmt.Errorf("invalid kubernetes version %q: want MAJOR.MINOR or MAJOR.MINOR.PATCH", s)
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := parseComponent(p)
		if err != nil {
			return Version{}, fmt.Errorf("invalid kubernetes version %q: %w", s, err)
		}
		nums[i] = n
	}
	if suffix != "" {
		if len(parts) != 3 {
			return Version{}, fmt.Errorf("invalid kubernetes version %q: a pre-release or build suffix needs MAJOR.MINOR.PATCH", s)
		}
		if !validSuffix(suffix) {
			return Version{}, fmt.Errorf("invalid kubernetes version %q: malformed pre-release or build suffix %q", s, suffix)
		}
	}
	return Version{Major: nums[0], Minor: nums[1]}, nil
}

// ParseTarget parses a user-supplied upgrade target (a --target flag, a
// ClusterReadiness spec target): ParseVersion plus major == 1, since
// Kubernetes has only ever shipped major 1 and "2.0" is a typo, not a target.
func ParseTarget(s string) (Version, error) {
	v, err := ParseVersion(s)
	if err != nil {
		return Version{}, err
	}
	if v.Major != 1 {
		return Version{}, fmt.Errorf("invalid kubernetes version %q: major version must be 1", s)
	}
	return v, nil
}

// validSuffix reports whether s (starting with "-" or "+") is a semver
// suffix: an optional "-" pre-release, then an optional "+" build, each a
// non-empty dot-separated list of non-empty [0-9A-Za-z-] identifiers.
func validSuffix(s string) bool {
	if rest, ok := strings.CutPrefix(s, "-"); ok {
		pre, build, hasBuild := strings.Cut(rest, "+")
		return validIdentifiers(pre) && (!hasBuild || validIdentifiers(build))
	}
	return validIdentifiers(strings.TrimPrefix(s, "+"))
}

func validIdentifiers(s string) bool {
	if s == "" {
		return false
	}
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		for _, r := range id {
			if (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '-' {
				return false
			}
		}
	}
	return true
}

// parseComponent parses one dot-separated component as a non-negative
// decimal integer. Unlike strconv.Atoi it rejects signs ("+1", "-1").
func parseComponent(p string) (int, error) {
	if p == "" {
		return 0, fmt.Errorf("empty component")
	}
	for _, r := range p {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("component %q is not a non-negative integer", p)
		}
	}
	n, err := strconv.Atoi(p)
	if err != nil { // only possible on overflow given the digit check above
		return 0, fmt.Errorf("component %q out of range", p)
	}
	return n, nil
}

// String renders the minor version, e.g. "1.34". Never includes a "v" prefix.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}

// MarshalJSON emits the canonical wire form, a string like "1.38" —
// matching String() and the camelCase report contract.
func (v Version) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.String())
}

// UnmarshalJSON accepts the canonical string form ("1.38", anything
// ParseVersion takes) and, for back-compat with datasets written before
// the string form existed, the legacy object form {"Major":1,"Minor":38}
// (strict: unknown keys rejected).
func (v *Version) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' { // legacy object form
		type legacy struct{ Major, Minor *int } // pointers so absence is detectable
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		var l legacy
		if err := dec.Decode(&l); err != nil {
			return fmt.Errorf("invalid kubernetes version object %s: %w", trimmed, err)
		}
		if l.Major == nil && l.Minor == nil {
			return fmt.Errorf("invalid kubernetes version object %s: missing Major and Minor", trimmed)
		}
		var maj, min int
		if l.Major != nil {
			maj = *l.Major
		}
		if l.Minor != nil {
			min = *l.Minor
		}
		if maj < 0 || min < 0 {
			return fmt.Errorf("invalid kubernetes version object %s: components must be non-negative", trimmed)
		}
		*v = Version{Major: maj, Minor: min}
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("kubernetes version must be a string like \"1.38\": %w", err)
	}
	parsed, err := ParseVersion(s)
	if err != nil {
		return err
	}
	*v = parsed
	return nil
}

// Compare returns -1 if v < o, 0 if equal, 1 if v > o.
func (v Version) Compare(o Version) int {
	if c := cmp.Compare(v.Major, o.Major); c != 0 {
		return c
	}
	return cmp.Compare(v.Minor, o.Minor)
}

// Next returns the following minor version: 1.34 → 1.35.
func (v Version) Next() Version {
	return Version{Major: v.Major, Minor: v.Minor + 1}
}
