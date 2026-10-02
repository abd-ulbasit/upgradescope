package suppress

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

var now = time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)

func eolNginx() engine.Finding {
	return engine.Finding{
		Category: engine.CatEOLAddon, Severity: engine.SevBlocker,
		Key: "eol-addon/ingress-nginx", Title: "ingress-nginx 1.11 is past end of life",
		Namespaces: []string{"ingress-nginx"},
	}
}

func removedIngress(objs ...inventory.ObjectRef) engine.Finding {
	return engine.Finding{
		Category: engine.CatRemovedAPI, Severity: engine.SevBlocker,
		Key: "removed-api/networking.k8s.io/v1beta1/Ingress", Title: "networking.k8s.io/v1beta1 Ingress removed in 1.22 (3 objects)",
		Detail:  "3 manifest object(s) use this API.",
		Objects: objs,
	}
}

func staleKB() engine.Finding {
	return engine.Finding{Category: engine.CatKBStale, Severity: engine.SevWarning, Key: "kb-stale", Title: "knowledge base is stale"}
}

func report(fs ...engine.Finding) engine.Report {
	r := engine.Report{Findings: fs}
	r.Rescore()
	return r
}

var (
	shopWeb   = inventory.ObjectRef{Namespace: "shop", Name: "web", File: "app.yaml", Line: 3}
	shopAdmin = inventory.ObjectRef{Namespace: "shop", Name: "admin", File: "legacy/old.yaml", Line: 1}
	internal  = inventory.ObjectRef{Namespace: "internal", Name: "api", File: "app.yaml", Line: 9}
)

// The issue's strongest case: an EOL ingress-nginx blocker accepted by key
// leaves the report ready, with the finding listed as suppressed.
func TestApplyKeyRuleSuppressesWholeFinding(t *testing.T) {
	r := report(eolNginx(), staleKB())
	if r.Verdict != engine.VerdictBlocked {
		t.Fatalf("precondition: verdict %s", r.Verdict)
	}
	rules := []Rule{{Key: "eol-addon/ingress-nginx", Reason: "migrating to Gateway API in Q1", Expires: "2026-12-31"}}
	got, warnings := Apply(r, rules, Options{Now: now, Source: ".upgradescope.yaml"})
	if len(warnings) != 0 {
		t.Errorf("warnings = %v", warnings)
	}
	if !reflect.DeepEqual(got.Findings, []engine.Finding{staleKB()}) {
		t.Errorf("findings = %+v", got.Findings)
	}
	want := []engine.SuppressedFinding{{Finding: eolNginx(), Reason: "migrating to Gateway API in Q1", Source: ".upgradescope.yaml", Expires: "2026-12-31"}}
	if !reflect.DeepEqual(got.Suppressed, want) {
		t.Errorf("suppressed = %+v\nwant %+v", got.Suppressed, want)
	}
	if got.Verdict != engine.VerdictReady || !got.Ready || got.Score != 95 {
		t.Errorf("verdict %s ready %v score %d, want ready, 95", got.Verdict, got.Ready, got.Score)
	}
	if len(r.Findings) != 2 {
		t.Error("Apply modified its input report")
	}
}

func TestApplyCategoryRule(t *testing.T) {
	r := report(eolNginx(), staleKB())
	got, _ := Apply(r, []Rule{{Category: "kb-stale", Reason: "pinned binary"}}, Options{Now: now})
	if len(got.Findings) != 1 || got.Findings[0].Key != "eol-addon/ingress-nginx" || len(got.Suppressed) != 1 {
		t.Errorf("findings %+v suppressed %+v", got.Findings, got.Suppressed)
	}
}

// Expiry is inclusive of the named day (UTC); the day after, the rule
// stops applying and says so.
func TestApplyExpiredRuleWarnsAndDoesNotSuppress(t *testing.T) {
	r := report(eolNginx())
	today := []Rule{{Key: "eol-addon/ingress-nginx", Reason: "r", Expires: "2026-10-02"}}
	if got, w := Apply(r, today, Options{Now: now}); len(got.Suppressed) != 1 || len(w) != 0 {
		t.Errorf("rule expiring today: suppressed %d, warnings %v", len(got.Suppressed), w)
	}
	yesterday := []Rule{{Key: "eol-addon/ingress-nginx", Reason: "r", Expires: "2026-10-01"}}
	got, w := Apply(r, yesterday, Options{Now: now, Source: "cfg.yaml"})
	if len(got.Suppressed) != 0 || len(got.Findings) != 1 || got.Verdict != engine.VerdictBlocked {
		t.Errorf("expired rule still suppressed: %+v", got)
	}
	if len(w) != 1 || !strings.Contains(w[0], "cfg.yaml: ignore[0]") || !strings.Contains(w[0], "expired on 2026-10-01") {
		t.Errorf("warnings = %v", w)
	}
}

// Object selectors take only the matching objects: a finding with other
// objects left stays (and still scores), naming how many were accepted.
func TestApplyObjectSelectorsPartial(t *testing.T) {
	r := report(removedIngress(shopWeb, internal, shopAdmin))
	rules := []Rule{
		{Key: "removed-api/networking.k8s.io/v1beta1/Ingress", Namespace: "shop", Name: "w*", Reason: "web is replaced"},
		{Category: "removed-api", File: "legacy/**", Reason: "legacy dir is not deployed"},
	}
	got, _ := Apply(r, rules, Options{Now: now, Source: "c"})
	if len(got.Findings) != 1 {
		t.Fatalf("findings = %+v", got.Findings)
	}
	f := got.Findings[0]
	if !reflect.DeepEqual(f.Objects, []inventory.ObjectRef{internal}) {
		t.Errorf("remaining objects = %+v", f.Objects)
	}
	if !strings.HasSuffix(f.Detail, "2 listed object(s) suppressed (see suppressed).") {
		t.Errorf("detail = %q", f.Detail)
	}
	if got.Verdict != engine.VerdictBlocked {
		t.Errorf("verdict = %s, want blocked (one object left)", got.Verdict)
	}
	if len(got.Suppressed) != 2 ||
		!reflect.DeepEqual(got.Suppressed[0].Objects, []inventory.ObjectRef{shopWeb}) || got.Suppressed[0].Reason != "web is replaced" ||
		!reflect.DeepEqual(got.Suppressed[1].Objects, []inventory.ObjectRef{shopAdmin}) || got.Suppressed[1].Reason != "legacy dir is not deployed" {
		t.Errorf("suppressed = %+v", got.Suppressed)
	}
}

// When every listed object is taken and none were omitted, the finding is
// gone from the scored list.
func TestApplyObjectSelectorsTakeAll(t *testing.T) {
	r := report(removedIngress(shopWeb, shopAdmin))
	got, _ := Apply(r, []Rule{{Category: "removed-api", Namespace: "shop", Reason: "shop is retired"}}, Options{Now: now})
	if len(got.Findings) != 0 || got.Verdict != engine.VerdictReady {
		t.Errorf("findings = %+v verdict %s", got.Findings, got.Verdict)
	}
	if got.Findings == nil {
		t.Error("findings must stay non-nil (JSON renders [])")
	}

	// Objects beyond the recorded cap may not match: the finding stays.
	omitted := removedIngress(shopWeb, shopAdmin)
	omitted.ObjectsOmitted = 4
	got, _ = Apply(report(omitted), []Rule{{Category: "removed-api", Namespace: "shop", Reason: "r"}}, Options{Now: now})
	if len(got.Findings) != 1 || len(got.Findings[0].Objects) != 0 || got.Findings[0].ObjectsOmitted != 4 {
		t.Errorf("findings = %+v", got.Findings)
	}
}

// File globs match the path relative to the config file's directory.
func TestApplyFileBase(t *testing.T) {
	r := report(removedIngress(shopWeb))
	rule := []Rule{{Category: "removed-api", File: "rendered/app.yaml", Reason: "r"}}
	if got, _ := Apply(r, rule, Options{Now: now}); len(got.Suppressed) != 0 {
		t.Error("matched without the file base")
	}
	if got, _ := Apply(r, rule, Options{Now: now, FileBase: "rendered"}); len(got.Suppressed) != 1 {
		t.Error("did not match with the file base")
	}
}

// Findings without objects (add-ons) match a namespace selector only when
// every namespace they name matches; name and file selectors never match.
func TestApplyNamespaceSelectorOnObjectlessFinding(t *testing.T) {
	r := report(eolNginx())
	cases := map[string]struct {
		rule Rule
		want int
	}{
		"namespace matches": {Rule{Key: "eol-addon/ingress-nginx", Namespace: "ingress-*", Reason: "r"}, 1},
		"namespace differs": {Rule{Key: "eol-addon/ingress-nginx", Namespace: "kube-system", Reason: "r"}, 0},
		"name selector":     {Rule{Key: "eol-addon/ingress-nginx", Name: "*", Reason: "r"}, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, _ := Apply(r, []Rule{tc.rule}, Options{Now: now})
			if len(got.Suppressed) != tc.want {
				t.Errorf("suppressed = %d, want %d", len(got.Suppressed), tc.want)
			}
		})
	}
}

func TestApplyAnnotations(t *testing.T) {
	accepted := shopWeb
	accepted.Ignore, accepted.IgnoreReason = "deprecated-api, removed-api/networking.k8s.io/v1beta1/Ingress", "replaced by HTTPRoute"
	other := shopAdmin
	other.Ignore = "eol-addon" // a category this finding is not
	noReason := internal
	noReason.Ignore = "removed-api"

	r := report(removedIngress(accepted, other, noReason))
	got, warnings := Apply(r, nil, Options{Now: now})
	if len(got.Suppressed) != 1 || got.Suppressed[0].Source != "annotation" || got.Suppressed[0].Reason != "replaced by HTTPRoute" ||
		!reflect.DeepEqual(got.Suppressed[0].Objects, []inventory.ObjectRef{accepted}) {
		t.Errorf("suppressed = %+v", got.Suppressed)
	}
	if len(got.Findings) != 1 || len(got.Findings[0].Objects) != 2 {
		t.Errorf("findings = %+v", got.Findings)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "internal/api") || !strings.Contains(warnings[0], "ignore-reason") {
		t.Errorf("warnings = %v", warnings)
	}
}

// Nothing to suppress leaves the report exactly as it was, score included.
func TestApplyNoMatchLeavesReportAlone(t *testing.T) {
	r := engine.Report{Score: 42, Verdict: engine.VerdictBlocked, Findings: []engine.Finding{eolNginx()}}
	got, _ := Apply(r, []Rule{{Key: "kb-stale", Reason: "r"}}, Options{Now: now})
	if !reflect.DeepEqual(got, r) {
		t.Errorf("report changed: %+v", got)
	}
}

func TestApplySkipsInvalidRules(t *testing.T) {
	got, w := Apply(report(eolNginx()), []Rule{{Key: "eol-addon/ingress-nginx"}}, Options{Now: now, Source: "spec.ignore"})
	if len(got.Suppressed) != 0 || len(w) != 1 || !strings.Contains(w[0], "spec.ignore: ignore[0]") || !strings.Contains(w[0], "reason is required") {
		t.Errorf("suppressed %d, warnings %v", len(got.Suppressed), w)
	}
}

func TestRuleValidate(t *testing.T) {
	cases := []struct {
		rule Rule
		err  string
	}{
		{Rule{Key: "eol-addon/ingress-nginx", Reason: "r"}, ""},
		{Rule{Key: "kb-stale", Reason: "r", Expires: "2027-01-31", Namespace: "a*", Name: "[ab]", File: "x/**/*.yaml"}, ""},
		{Rule{Reason: "r"}, "set key or category"},
		{Rule{Key: "kb-stale", Category: "kb-stale", Reason: "r"}, "set only one of key and category"},
		{Rule{Key: "kb-stale"}, "reason is required"},
		{Rule{Key: "kb-stale", Reason: "  "}, "reason is required"},
		{Rule{Category: "removed-apis", Reason: "r"}, `unknown category "removed-apis"`},
		{Rule{Key: "eol/ingress-nginx", Reason: "r"}, `key "eol/ingress-nginx" does not start with a finding category`},
		{Rule{Key: "kb-stale", Reason: "r", Expires: "31-12-2026"}, `expires "31-12-2026" is not a YYYY-MM-DD date`},
		{Rule{Key: "kb-stale", Reason: "r", Namespace: "[a"}, "invalid namespace glob"},
		{Rule{Key: "kb-stale", Reason: "r", File: "a/[/b"}, "invalid file glob"},
	}
	for _, tc := range cases {
		err := tc.rule.Validate()
		if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("Validate(%+v) = %v, want %q", tc.rule, err, tc.err)
		}
	}
}

func TestMatchFile(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"app.yaml", "app.yaml", true},
		{"*.yaml", "dir/app.yaml", false},
		{"**/*.yaml", "dir/sub/app.yaml", true},
		{"**/*.yaml", "app.yaml", true},
		{"legacy/**", "legacy/a/b.yaml", true},
		{"legacy/**", "legacyx/a.yaml", false},
		{"./rendered/*.yaml", "rendered/a.yaml", true},
		{"a/**/b/*.yaml", "a/x/y/b/c.yaml", true},
	}
	for _, tc := range cases {
		if got := matchFile(tc.pattern, tc.path); got != tc.want {
			t.Errorf("matchFile(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ConfigFile)
	writeFile(t, p, `ignore:
  - key: eol-addon/ingress-nginx
    reason: migrating to Gateway API
    expires: 2026-12-31
  - category: removed-api
    file: legacy/**
    reason: not deployed
`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Ignore: []Rule{
		{Key: "eol-addon/ingress-nginx", Reason: "migrating to Gateway API", Expires: "2026-12-31"},
		{Category: "removed-api", File: "legacy/**", Reason: "not deployed"},
	}}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("config = %+v\nwant %+v", cfg, want)
	}

	writeFile(t, p, "")
	if cfg, err := LoadConfig(p); err != nil || len(cfg.Ignore) != 0 {
		t.Errorf("empty file: %+v, %v", cfg, err)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	cases := map[string]string{
		"missing reason": "ignore:\n  - key: kb-stale\n",
		"unknown field":  "ignore:\n  - key: kb-stale\n    reason: r\n    expiry: 2026-01-01\n",
		"unknown top":    "ignores: []\n",
		"not yaml":       "ignore: [\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), ConfigFile)
			writeFile(t, p, content)
			_, err := LoadConfig(p)
			if err == nil || !strings.Contains(err.Error(), p) {
				t.Errorf("err = %v, want an error naming %s", err, p)
			}
		})
	}
	if err := func() error { _, err := LoadConfig(filepath.Join(t.TempDir(), "nope.yaml")); return err }(); err == nil {
		t.Error("missing file: want error")
	}
	p := filepath.Join(t.TempDir(), ConfigFile)
	writeFile(t, p, "ignore:\n  - key: kb-stale\n    reason: r\n  - key: kb-stale\n")
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "ignore[1]: reason is required") {
		t.Errorf("err = %v, want it to name ignore[1]", err)
	}
}

func TestFindConfig(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	scanRoot := filepath.Join(repo, "deploy", "rendered")
	if err := os.MkdirAll(scanRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	if got, err := FindConfig(scanRoot); err != nil || got != "" {
		t.Errorf("no config: %q, %v", got, err)
	}
	atRepo := filepath.Join(repo, ConfigFile)
	writeFile(t, atRepo, "ignore: []\n")
	if got, err := FindConfig(scanRoot); err != nil || got != atRepo {
		t.Errorf("repo root config: %q, %v; want %s", got, err, atRepo)
	}
	atRoot := filepath.Join(scanRoot, ConfigFile)
	writeFile(t, atRoot, "ignore: []\n")
	if got, err := FindConfig(scanRoot); err != nil || got != atRoot {
		t.Errorf("scan root config: %q, %v; want %s (nearest wins)", got, err, atRoot)
	}
}
