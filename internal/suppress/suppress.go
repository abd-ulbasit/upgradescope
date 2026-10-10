// Package suppress applies accepted findings to an engine report: ignore
// rules (from .upgradescope.yaml or ClusterReadiness spec.ignore) and the
// ignore object annotation (apigroup.IgnoreAnnotation) move findings, or single
// objects of a finding, into Report.Suppressed with the reason given, so
// they stop counting toward score and verdict but stay visible. It also
// compares a report with a baseline report (see Baseline).
package suppress

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/abd-ulbasit/upgradescope/internal/crd/apigroup"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// ConfigFile is the config file name scan discovers (see FindConfig).
const ConfigFile = ".upgradescope.yaml"

// AnnotationSource is SuppressedFinding.Source for annotation suppressions.
const AnnotationSource = "annotation"

// dateLayout is the expires format.
const dateLayout = "2006-01-02"

// categories are the finding categories a rule may name. A key's first
// segment must be one of them, so a typo fails loudly instead of silently
// suppressing nothing.
var categories = []engine.Category{
	engine.CatRemovedAPI, engine.CatDeprecatedAPI, engine.CatDeprecatedAPIInUse,
	engine.CatEOLAddon, engine.CatEOLApproaching, engine.CatVersionSkew,
	engine.CatChartIncompat, engine.CatKBStale, engine.CatAddOnNoData,
	engine.CatUnknownAPI, engine.CatCRDVersion, engine.CatSupportLifecycle, engine.CatVolumePlugin,
}

// Rule is one ignore entry. It names findings by Key (exact) or Category,
// optionally narrowed to objects by Namespace, Name and File globs; Reason
// is required and Expires (YYYY-MM-DD, inclusive, UTC) optional. The same
// schema is ClusterReadiness spec.ignore.
type Rule struct {
	Key       string `json:"key,omitempty" yaml:"key,omitempty"`
	Category  string `json:"category,omitempty" yaml:"category,omitempty"`
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Name      string `json:"name,omitempty" yaml:"name,omitempty"`
	File      string `json:"file,omitempty" yaml:"file,omitempty"`
	Reason    string `json:"reason" yaml:"reason"`
	Expires   string `json:"expires,omitempty" yaml:"expires,omitempty"`
}

// Validate reports the first problem with r.
func (r Rule) Validate() error {
	switch {
	case r.Key == "" && r.Category == "":
		return errors.New("set key or category")
	case r.Key != "" && r.Category != "":
		return errors.New("set only one of key and category")
	}
	if r.Category != "" && !slices.Contains(categories, engine.Category(r.Category)) {
		return fmt.Errorf("unknown category %q", r.Category)
	}
	if cat, _, _ := strings.Cut(r.Key, "/"); r.Key != "" && !slices.Contains(categories, engine.Category(cat)) {
		return fmt.Errorf("key %q does not start with a finding category", r.Key)
	}
	if strings.TrimSpace(r.Reason) == "" {
		return errors.New("reason is required")
	}
	if r.Expires != "" {
		if _, err := time.Parse(dateLayout, r.Expires); err != nil {
			return fmt.Errorf("expires %q is not a YYYY-MM-DD date", r.Expires)
		}
	}
	for _, g := range []struct{ name, pattern string }{{"namespace", r.Namespace}, {"name", r.Name}, {"file", r.File}} {
		for _, seg := range strings.Split(g.pattern, "/") {
			if _, err := path.Match(seg, ""); err != nil {
				return fmt.Errorf("invalid %s glob %q", g.name, g.pattern)
			}
		}
	}
	return nil
}

// expired reports whether now is past the expires day (UTC).
func (r Rule) expired(now time.Time) bool {
	if r.Expires == "" {
		return false
	}
	day, err := time.Parse(dateLayout, r.Expires)
	return err == nil && !now.UTC().Before(day.AddDate(0, 0, 1))
}

// String names the rule in warnings: "key eol-addon/ingress-nginx".
func (r Rule) String() string {
	if r.Key != "" {
		return "key " + r.Key
	}
	return "category " + r.Category
}

// Config is the .upgradescope.yaml document.
type Config struct {
	Ignore []Rule `yaml:"ignore"`
}

// LoadConfig reads and validates a config file. Unknown fields and invalid
// rules are errors naming the file.
func LoadConfig(file string) (Config, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return ParseConfig(raw, "config "+file)
}

// ParseConfig parses and validates a config document, as LoadConfig does
// a file; errors start with name. The server gate reads its config
// parameter with it.
func ParseConfig(raw []byte, name string) (Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("%s: %w", name, err)
	}
	for i, r := range cfg.Ignore {
		if err := r.Validate(); err != nil {
			return Config{}, fmt.Errorf("%s: ignore[%d]: %w", name, i, err)
		}
	}
	return cfg, nil
}

// FindConfig returns the config file scan uses when none is named: the
// one in scanRoot, else the one at the root of the git repository holding
// scanRoot (the nearest directory upwards with a .git entry), else "".
func FindConfig(scanRoot string) (string, error) {
	abs, err := filepath.Abs(scanRoot)
	if err != nil {
		return "", err
	}
	dirs := []string{abs}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			if d != abs {
				dirs = append(dirs, d)
			}
			break
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	for _, d := range dirs {
		p := filepath.Join(d, ConfigFile)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("look for config: %w", err)
		}
	}
	return "", nil
}

// Options parameterize Apply.
type Options struct {
	Now time.Time // decides which rules have expired
	// Source labels rule suppressions and prefixes warnings: the config
	// file path, or "ClusterReadiness spec.ignore".
	Source string
	// FileBase is joined before object file paths when matching file
	// globs: the scanned root relative to the config file's directory, so
	// globs are written relative to the config file.
	FileBase string
}

// Apply returns r with the findings, or listed objects of findings, that
// rules or object annotations accept moved to Suppressed, and score and
// verdict recomputed; r itself is not modified, and is returned unchanged
// when nothing matches. Per finding, rules apply in order and then
// annotations; the first to match an object takes it.
//
// A rule without object selectors takes the whole finding (unlisted
// objects included). With selectors it takes the listed objects that match
// all of them; a finding with no listed objects (add-ons, skew) matches
// only a namespace selector, and only when every namespace it names
// matches. A finding all of whose objects are taken, none omitted, leaves
// Findings; otherwise it stays, with the remaining objects and a note.
//
// The deprecated-API callers folded into an API usage finding
// (Finding.Callers) are evidence about clients, not about its objects:
// only a rule without object selectors that takes the finding takes them
// too. When object selectors or annotations take every object, each
// caller stands as the deprecated-api-in-use finding it is on its own
// (engine.Caller.Finding), at its own severity, and is matched against the
// rules like any finding, so a rule for its key or category, without
// selectors, still suppresses it. Fail closed: the client calling the
// removed API may not be the one that wrote the suppressed objects.
//
// The returned warnings name expired rules (which do not apply), invalid
// rules (skipped), and annotations without a reason (not applied), in
// that order. Between the rule and the annotation warnings comes one
// warning for all the objects on the deprecated pre-v0.2.0 annotation
// keys that annotate a finding of r (see apigroup.LegacyIgnoreWarning),
// however many there are: a consumer that keeps only the first few
// warnings still keeps it.
func Apply(r engine.Report, rules []Rule, opts Options) (engine.Report, []string) {
	var warnings []string
	var active []Rule
	for i, rule := range rules {
		if err := rule.Validate(); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: ignore[%d]: skipped: %v", opts.Source, i, err))
			continue
		}
		if rule.expired(opts.Now) {
			warnings = append(warnings, fmt.Sprintf("%s: ignore[%d] (%s) expired on %s and no longer applies", opts.Source, i, rule, rule.Expires))
			continue
		}
		if legacySupportKey(rule.Key) {
			warnings = append(warnings, fmt.Sprintf("%s: ignore[%d] (%s) matches nothing: the key of a support-lifecycle finding names its phase now; write %s/ending, %s/extended or %s/ended for the phase you accept (see the support lifecycle page)",
				opts.Source, i, rule, rule.Key, rule.Key, rule.Key))
		}
		active = append(active, rule)
	}

	ruleWarnings := len(warnings)

	findings := make([]engine.Finding, 0, len(r.Findings))
	var suppressed []engine.SuppressedFinding
	warned := map[string]bool{}
	var legacy []string // objects on the old keys, each once, in report order
	reemitted := false
	queue := slices.Clone(r.Findings)
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		kept, taken, callers, warns, old := applyFinding(f, active, opts)
		for _, w := range warns {
			if !warned[w] {
				warned[w] = true
				warnings = append(warnings, w)
			}
		}
		for _, name := range old {
			if !slices.Contains(legacy, name) {
				legacy = append(legacy, name)
			}
		}
		if kept != nil {
			findings = append(findings, *kept)
		}
		suppressed = append(suppressed, taken...)
		for _, c := range callers {
			reemitted = true
			queue = append(queue, c.Finding()) // judged by the rules in turn
		}
	}
	if len(legacy) > 0 {
		warnings = slices.Insert(warnings, ruleWarnings, apigroup.LegacyIgnoreWarning(legacy))
	}
	if len(suppressed) == 0 {
		return r, warnings
	}
	if reemitted {
		engine.SortFindings(findings)
	}
	r.Findings = findings
	r.Suppressed = append(slices.Clone(r.Suppressed), suppressed...)
	r.Rescore()
	return r, warnings
}

// legacySupportKey reports whether key is a support-lifecycle key without
// the phase, support-lifecycle/<provider>/<minor>, which every phase used
// before the phase was part of the key (engine.SupportKey). Such a rule
// matches no finding now, which fails closed (the finding is no longer
// accepted), so Apply says so instead of leaving it silent.
func legacySupportKey(key string) bool {
	rest, ok := strings.CutPrefix(key, string(engine.CatSupportLifecycle)+"/")
	return ok && strings.Count(rest, "/") == 1 && !strings.HasPrefix(rest, "/") && !strings.HasSuffix(rest, "/")
}

// group collects what one rule (or one annotation reason) took.
type group struct {
	reason, source, expires string
	objects                 []inventory.ObjectRef
	whole                   bool // also took unlisted objects (or an objectless finding)
	all                     bool // a rule without object selectors: it takes the callers too
}

// applyFinding splits f into what stays (nil when nothing does), what is
// suppressed, and the callers folded into f that stand on their own
// because object selectors or annotations took all of f (see Apply).
// legacy names the objects whose annotation of f uses the pre-v0.2.0 keys.
func applyFinding(f engine.Finding, rules []Rule, opts Options) (kept *engine.Finding, out []engine.SuppressedFinding, callers []engine.Caller, warnings, legacy []string) {
	var groups []*group
	remaining := f.Objects
	whole := false
	for _, rule := range rules {
		if whole {
			break
		}
		if rule.Key != "" && rule.Key != f.Key || rule.Category != "" && rule.Category != string(f.Category) {
			if base, ok := engine.BaseOfUnservedKey(f.Key); ok && rule.Key == base {
				warnings = append(warnings, fmt.Sprintf("%s: ignore rule (%s) matches nothing for the API of %q: the key of a finding for an API the target does not serve yet ends in /unserved now, so that a rule for it never accepts the removal that follows; write %s to accept it (the bare key stays the removal's)",
					opts.Source, rule, f.Key, f.Key))
			}
			continue
		}
		g := &group{reason: rule.Reason, source: opts.Source, expires: rule.Expires}
		switch {
		case rule.Namespace == "" && rule.Name == "" && rule.File == "":
			g.objects, remaining, g.whole, g.all = remaining, nil, true, true
		case len(f.Objects) == 0:
			// Namespaces the finding does not list (NamespacesOmitted),
			// and installs in no named namespace (Unnamespaced: a manifest
			// without metadata.namespace, an IngressClass), cannot be
			// shown to match, so a namespace rule takes it whole only
			// when it lists them all and the finding has none.
			g.whole = rule.Name == "" && rule.File == "" && f.NamespacesOmitted == 0 && !f.Unnamespaced && allMatch(rule.Namespace, f.Namespaces)
		default:
			g.objects, remaining = take(remaining, func(o inventory.ObjectRef) bool { return rule.matches(o, opts.FileBase) })
		}
		whole = g.whole
		if g.whole || len(g.objects) > 0 {
			groups = append(groups, g)
		}
	}

	if !whole {
		byReason := map[string]*group{}
		_, remaining = take(remaining, func(o inventory.ObjectRef) bool {
			if !annotated(o, f) {
				return false
			}
			if o.LegacyIgnore() {
				legacy = append(legacy, objectName(o))
			}
			reason := strings.TrimSpace(o.IgnoreReason)
			if reason == "" {
				// Name the keys the object carries, each by its own
				// generation: an object can mix the new ignore key with the
				// old ignore-reason key. An absent reason follows the ignore
				// key's generation.
				ignoreKey, reasonKey := apigroup.IgnoreAnnotation, apigroup.IgnoreReasonAnnotation
				if o.IgnoreLegacyKey {
					ignoreKey = apigroup.LegacyIgnoreAnnotation
				}
				if o.IgnoreReasonLegacyKey || o.IgnoreLegacyKey && o.IgnoreReason == "" {
					reasonKey = apigroup.LegacyIgnoreReasonAnnotation
				}
				warnings = append(warnings, fmt.Sprintf("object %s: %s annotation without %s is not applied", objectName(o), ignoreKey, reasonKey))
				return false
			}
			g, ok := byReason[reason]
			if !ok {
				g = &group{reason: reason, source: AnnotationSource}
				byReason[reason] = g
				groups = append(groups, g)
			}
			g.objects = append(g.objects, o)
			return true
		})
	}

	if len(groups) == 0 {
		return &f, nil, nil, warnings, legacy
	}
	out = make([]engine.SuppressedFinding, 0, len(groups))
	listed := 0
	callersTaken := false
	for _, g := range groups {
		sf := engine.SuppressedFinding{Finding: f, Reason: g.reason, Source: g.source, Expires: g.expires}
		sf.Objects = g.objects
		sf.ObjectsOmitted = 0
		if g.whole {
			sf.ObjectsOmitted = f.ObjectsOmitted
		}
		// The callers go with the rule without selectors that took them,
		// and stay with f (or on their own) otherwise.
		sf.Callers = nil
		if g.all {
			sf.Callers, callersTaken = f.Callers, true
		}
		listed += len(g.objects)
		out = append(out, sf)
	}
	if whole || len(remaining) == 0 && f.ObjectsOmitted == 0 {
		if callersTaken {
			return nil, out, nil, warnings, legacy
		}
		return nil, out, f.Callers, warnings, legacy
	}
	f.Objects = remaining
	// listed counts what the rules and annotations took from the refs f
	// holds here, which a /gate's capObjects cuts to MaxObjectRefs only
	// afterwards: the sentence says how many objects were suppressed, not
	// how many of them the answer lists.
	f.Detail = strings.TrimSpace(fmt.Sprintf("%s %d object(s) suppressed (see suppressed).", f.Detail, listed))
	return &f, out, nil, warnings, legacy
}

// matches reports whether object o satisfies every selector of r.
func (r Rule) matches(o inventory.ObjectRef, fileBase string) bool {
	if r.Namespace != "" && !globMatch(r.Namespace, o.Namespace) {
		return false
	}
	if r.Name != "" && !globMatch(r.Name, o.Name) {
		return false
	}
	if r.File != "" && (o.File == "" || !matchFile(r.File, path.Join(fileBase, o.File))) {
		return false
	}
	return true
}

// annotated reports whether o's ignore annotation names f's category or key.
func annotated(o inventory.ObjectRef, f engine.Finding) bool {
	for _, tok := range strings.Split(o.Ignore, ",") {
		if tok = strings.TrimSpace(tok); tok != "" && (tok == string(f.Category) || tok == f.Key) {
			return true
		}
	}
	return false
}

// take splits objs into those pred accepts and the rest, in order.
func take(objs []inventory.ObjectRef, pred func(inventory.ObjectRef) bool) (taken, rest []inventory.ObjectRef) {
	for _, o := range objs {
		if pred(o) {
			taken = append(taken, o)
		} else {
			rest = append(rest, o)
		}
	}
	return taken, rest
}

// allMatch reports whether names is non-empty and every name matches.
func allMatch(pattern string, names []string) bool {
	for _, n := range names {
		if !globMatch(pattern, n) {
			return false
		}
	}
	return len(names) > 0
}

func globMatch(pattern, s string) bool {
	ok, _ := path.Match(pattern, s)
	return ok
}

// matchFile matches a slash-separated path against a glob in which "**"
// spans any number of directories (path.Match semantics otherwise).
func matchFile(pattern, name string) bool {
	return matchSegments(strings.Split(path.Clean(pattern), "/"), strings.Split(path.Clean(name), "/"))
}

func matchSegments(pattern, segs []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pattern[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 || !globMatch(pattern[0], segs[0]) {
			return false
		}
		pattern, segs = pattern[1:], segs[1:]
	}
	return len(segs) == 0
}

// objectName is "namespace/name", "name" or "(unnamed)", with the file
// location when there is one.
func objectName(o inventory.ObjectRef) string {
	name := o.Name
	if name == "" {
		name = "(unnamed)"
	}
	if o.Namespace != "" {
		name = o.Namespace + "/" + name
	}
	if o.File != "" {
		name += fmt.Sprintf(" (%s:%d)", o.File, o.Line)
	}
	return name
}
