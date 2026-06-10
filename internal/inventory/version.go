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
// "1.34", "v1.34", "v1.34.2", "1.34.2". The patch component is validated
// but discarded — evaluation only cares about minors.
func ParseVersion(s string) (Version, error) {
	trimmed := strings.TrimPrefix(s, "v")
	if trimmed == "" {
		return Version{}, fmt.Errorf("invalid kubernetes version %q: empty", s)
	}
	parts := strings.Split(trimmed, ".")
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
	return Version{Major: nums[0], Minor: nums[1]}, nil
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
		type legacy struct{ Major, Minor int }
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		var l legacy
		if err := dec.Decode(&l); err != nil {
			return fmt.Errorf("invalid kubernetes version object %s: %w", trimmed, err)
		}
		*v = Version{Major: l.Major, Minor: l.Minor}
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
