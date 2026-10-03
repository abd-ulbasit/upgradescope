package inventory

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Limits on what an inventory carries beyond its identifiers
// (ValidateIdentifiers). They are what a collector records (the object
// and image caps, one entry per group/version/kind), or far above any
// genuine value; the free text a collector copies whole from what it
// reads, which Kubernetes lets be longer (a capability's reason and
// skipped entries, the ignore annotations), it cuts to them
// (CutFreeText), and so does the server, for agents that did not. The
// server refuses an inventory beyond one (422) before evaluating it,
// because the engine repeats these values in the findings it builds: a
// kubeVersion of 20 MB of quotes was a 84 MB report, a release per
// finding with a short one eight times its push.
const (
	// MaxStringBytes caps every string in an inventory, map keys
	// included (versions, chart names, image repositories, kubeVersion
	// constraints, file paths and annotation values too), but the two
	// below, which are capped apart: what one finding can repeat of any
	// of them stays small.
	MaxStringBytes = 16 << 10
	// MaxReasonBytes caps a capability's Reason, which joins one error per
	// resource a collector could not read.
	MaxReasonBytes = 64 << 10
	// MaxManagerBytes caps an ObjectRef's Manager, a managedFields
	// manager: the apiserver refuses a longer one.
	MaxManagerBytes = 128
	// MaxCapabilities caps the capabilities an inventory reports (six
	// today): each one not assessed is a gap in every report and in every
	// evaluation summary the fleet-wide reads carry.
	MaxCapabilities = 32
)

// LimitError is a value in an inventory beyond what any collector
// records.
type LimitError struct {
	Field   string // JSON path within the inventory, e.g. "apiUsage[3].objects"
	Problem string
}

func (e *LimitError) Error() string { return e.Field + ": " + e.Problem }

// ValidateLimits checks what ValidateIdentifiers does not, and returns the
// first problem as a *LimitError:
//
//   - every string, map keys included, is at most MaxStringBytes, a
//     capability's reason at most MaxReasonBytes, and an object's
//     manager at most MaxManagerBytes of printable characters, as the
//     apiserver requires of a managedFields manager;
//   - an API usage entry lists at most MaxObjectRefs objects (collectors
//     count the rest in objectsOmitted), and a list of them (apiUsage,
//     apiAuthorshipUnknown, a Helm release's manifestApis, a CRD's usage) names each
//     group/version/kind once, as every collector counts them;
//   - unrecognizedImages lists at most MaxUnrecognizedImages, and
//     capabilities at most MaxCapabilities.
func (inv Inventory) ValidateLimits() error {
	if err := validateUsages(func() string { return "apiUsage" }, inv.APIUsage); err != nil {
		return err
	}
	if err := validateUsages(func() string { return "apiAuthorshipUnknown" }, inv.APIAuthorshipUnknown); err != nil {
		return err
	}
	for i, r := range inv.HelmReleases {
		if err := validateUsages(func() string { return fmt.Sprintf("helmReleases[%d].manifestApis", i) }, r.ManifestAPIs); err != nil {
			return err
		}
	}
	for i, c := range inv.CRDs {
		if err := validateUsages(func() string { return fmt.Sprintf("crds[%d].usage", i) }, c.Usage); err != nil {
			return err
		}
	}
	if n := len(inv.UnrecognizedImages); n > MaxUnrecognizedImages {
		return &LimitError{Field: "unrecognizedImages", Problem: fmt.Sprintf("%d images, over the %d a collector lists (it counts the rest in unrecognizedImagesOmitted)", n, MaxUnrecognizedImages)}
	}
	if n := len(inv.Capabilities); n > MaxCapabilities {
		return &LimitError{Field: "capabilities", Problem: fmt.Sprintf("%d capabilities, over the %d this server takes", n, MaxCapabilities)}
	}
	var w stringWalker
	return w.walk(reflect.ValueOf(inv), MaxStringBytes)
}

// cutMark ends a reason or an ignore-reason that CutFreeText cut.
const cutMark = " …(cut)"

// CutFreeText cuts the free text a collector copies whole from what it
// reads, and that Kubernetes lets be longer than the limits, to them, and
// reports whether it cut anything. None of it is needed whole:
//
//   - a capability's Reason (one error per resource it could not read: a
//     CRD collector without RBAC for custom resources adds ~200 bytes per
//     CRD at a deprecated version) to MaxReasonBytes, and each Skipped
//     entry to MaxStringBytes, ending in cutMark;
//   - an object's Ignore and IgnoreReason, the upgradescope.dev/ignore
//     and ignore-reason annotations (up to 256 KiB), to MaxStringBytes:
//     Ignore to the comma-separated tokens that fit whole, IgnoreReason
//     ending in cutMark;
//   - a GitOps chart's Chart, Version and Repo, copied from a custom
//     resource's spec, to MaxStringBytes, ending in cutMark.
//
// Each cut keeps whole UTF-8 characters. Collect calls it, and the server
// calls it on every push before ValidateLimits, so older agents' pushes
// are cut rather than refused.
func (inv *Inventory) CutFreeText() bool {
	cut := false
	for c, st := range inv.Capabilities {
		changed := false
		if r, ok := cutText(st.Reason, MaxReasonBytes); ok {
			st.Reason, changed = r, true
		}
		for i, s := range st.Skipped {
			if r, ok := cutText(s, MaxStringBytes); ok {
				if !changed {
					st.Skipped = slices.Clone(st.Skipped)
				}
				st.Skipped[i], changed = r, true
			}
		}
		if changed {
			inv.Capabilities[c], cut = st, true
		}
	}
	refs := func(us []APIUsage) {
		for i := range us {
			for k := range us[i].Objects {
				o := &us[i].Objects[k]
				if s, ok := cutTokens(o.Ignore, MaxStringBytes); ok {
					o.Ignore, cut = s, true
				}
				if s, ok := cutText(o.IgnoreReason, MaxStringBytes); ok {
					o.IgnoreReason, cut = s, true
				}
			}
		}
	}
	refs(inv.APIUsage)
	refs(inv.APIAuthorshipUnknown)
	for i := range inv.HelmReleases {
		refs(inv.HelmReleases[i].ManifestAPIs)
	}
	for i := range inv.GitOpsCharts {
		c := &inv.GitOpsCharts[i]
		for _, s := range []*string{&c.Chart, &c.Version, &c.Repo} { // a custom resource's free text
			if r, ok := cutText(*s, MaxStringBytes); ok {
				*s, cut = r, true
			}
		}
	}
	for i := range inv.CRDs {
		refs(inv.CRDs[i].Usage)
	}
	return cut
}

// cutText cuts s to at most max bytes ending in cutMark, at a character
// boundary; ok is false when s fits.
func cutText(s string, max int) (string, bool) {
	if len(s) <= max {
		return "", false
	}
	return utf8Prefix(s, max-len(cutMark)) + cutMark, true
}

// cutTokens cuts a comma-separated list to the tokens that fit whole in
// max bytes; ok is false when s fits.
func cutTokens(s string, max int) (string, bool) {
	if len(s) <= max {
		return "", false
	}
	i := strings.LastIndexByte(s[:max+1], ',')
	if i < 0 {
		return "", true
	}
	return strings.TrimRight(s[:i], ", "), true
}

// utf8Prefix is the longest prefix of s of at most n bytes that ends at a
// character boundary.
func utf8Prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// validateUsages checks one list of API usage entries; at names it.
func validateUsages(at func() string, us []APIUsage) error {
	seen := make(map[[3]string]int, len(us))
	for i, u := range us {
		gvk := [3]string{u.Group, u.Version, u.Kind}
		if j, dup := seen[gvk]; dup {
			return &LimitError{Field: fmt.Sprintf("%s[%d]", at(), i), Problem: fmt.Sprintf(
				"%s is listed again (first at %s[%d]): a collector counts each group/version/kind once", quoteShort(gvString(u.Group, u.Version)+" "+u.Kind), at(), j)}
		}
		seen[gvk] = i
		if n := len(u.Objects); n > MaxObjectRefs {
			return &LimitError{Field: fmt.Sprintf("%s[%d].objects", at(), i), Problem: fmt.Sprintf(
				"%d objects, over the %d a collector records (it counts the rest in objectsOmitted)", n, MaxObjectRefs)}
		}
		for k, o := range u.Objects {
			if p := managerProblem(o.Manager); p != "" {
				return &LimitError{Field: fmt.Sprintf("%s[%d].objects[%d].manager", at(), i, k), Problem: p}
			}
		}
	}
	return nil
}

func gvString(group, version string) string {
	if group == "" {
		return version
	}
	return group + "/" + version
}

// managerProblem says why m is not a managedFields manager the apiserver
// accepts, or "".
func managerProblem(m string) string {
	if len(m) > MaxManagerBytes {
		return fmt.Sprintf("%s is not a field manager: longer than the apiserver's %d bytes", quoteShort(m), MaxManagerBytes)
	}
	if i := strings.IndexFunc(m, func(r rune) bool { return !unicode.IsPrint(r) }); i >= 0 {
		return fmt.Sprintf("%s is not a field manager: a non-printable character at byte %d", quoteShort(m), i)
	}
	return ""
}

var (
	timeType             = reflect.TypeFor[time.Time]()
	capabilityStatusType = reflect.TypeFor[CapabilityStatus]()
)

// stringWalker checks every string in a value against its limit. It keeps
// the JSON path it is at as a stack, rendered only for an error, so a walk
// over a million values allocates nothing per value.
type stringWalker struct {
	path []pathStep
}

// pathStep is one step of a JSON path: a field name, a map key (keyed),
// or else an index.
type pathStep struct {
	name  string
	key   string
	keyed bool
	index int
}

func (w *stringWalker) at() string {
	var b strings.Builder
	for _, p := range w.path {
		switch {
		case p.keyed:
			b.WriteString("[" + quoteShort(p.key) + "]")
		case p.name != "":
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(p.name)
		default:
			fmt.Fprintf(&b, "[%d]", p.index)
		}
	}
	return b.String()
}

func (w *stringWalker) check(s string, max int, suffix string) error {
	if len(s) > max {
		return &LimitError{Field: w.at() + suffix, Problem: fmt.Sprintf("%s is over the %d-byte limit", quoteShort(s), max)}
	}
	return nil
}

// walk returns a *LimitError for the first string (or map key) in v
// longer than max, or than its field's own limit.
func (w *stringWalker) walk(v reflect.Value, max int) error {
	switch v.Kind() {
	case reflect.String:
		return w.check(v.String(), max, "")
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			w.path = append(w.path, pathStep{index: i})
			if err := w.walk(v.Index(i), max); err != nil {
				return err
			}
			w.path = w.path[:len(w.path)-1]
		}
	case reflect.Map:
		if counts, ok := v.Interface().(map[string]int); ok { // namespace counts, the large maps
			for k := range counts {
				if err := w.check(k, MaxStringBytes, " (a key)"); err != nil {
					return err
				}
			}
			return nil
		}
		it := v.MapRange()
		for it.Next() {
			k := it.Key().String()
			if err := w.check(k, MaxStringBytes, " (a key)"); err != nil {
				return err
			}
			w.path = append(w.path, pathStep{key: k, keyed: true})
			if err := w.walk(it.Value(), MaxStringBytes); err != nil {
				return err
			}
			w.path = w.path[:len(w.path)-1]
		}
	case reflect.Struct:
		t := v.Type()
		if t == timeType {
			return nil
		}
		for i := range t.NumField() {
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
			if name == "" {
				name = sf.Name
			}
			fieldMax := MaxStringBytes
			if t == capabilityStatusType && sf.Name == "Reason" {
				fieldMax = MaxReasonBytes
			}
			w.path = append(w.path, pathStep{name: name})
			if err := w.walk(v.Field(i), fieldMax); err != nil {
				return err
			}
			w.path = w.path[:len(w.path)-1]
		}
	}
	return nil
}
