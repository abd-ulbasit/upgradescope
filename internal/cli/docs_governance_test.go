package cli

// Tests that keep the README a short summary and the project's governance
// files present and linked: the README links every project file a visitor
// or the CNCF Sandbox form looks for, MAINTAINERS.md has the columns that
// form checks, and the roadmap links its milestones.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readmeWordBudget caps the README's length, counted as wc -w counts it
// (white-space separated fields, code blocks and badges included): #404 asks
// for under 1,200. The long material lives on the docs site; the README
// links it.
const readmeWordBudget = 1200

// TestReadmeStaysASummary: the README is under its word budget.
func TestReadmeStaysASummary(t *testing.T) {
	if n := len(strings.Fields(readDoc(t, "README.md"))); n >= readmeWordBudget {
		t.Errorf("README.md has %d words, not under the budget of %d: move the detail to a docs page and link it", n, readmeWordBudget)
	}
}

// movedDetail are the docs pages that hold what the README only summarises
// (#404): each must exist and the README must link it.
var movedDetail = map[string]string{
	"docs/operations/scale.md":                   "https://abd-ulbasit.github.io/upgradescope/operations/scale/",
	"docs/operations/security-model-and-rbac.md": "https://abd-ulbasit.github.io/upgradescope/operations/security-model-and-rbac/",
	"docs/operations/install.md":                 "https://abd-ulbasit.github.io/upgradescope/operations/install/",
	"docs/getting-started/ci-gate.md":            "https://abd-ulbasit.github.io/upgradescope/getting-started/ci-gate/",
	"docs/comparison.md":                         "https://abd-ulbasit.github.io/upgradescope/comparison/",
}

// TestReadmeLinksTheMovedDetail (#404): every docs page that received the
// README's detail (scale and cost, the measured rows in full, the agent's
// RBAC, the verification of every channel, the CI gate's fork and Dependabot
// cases, kube-upgrade-check) exists and is linked from the README.
func TestReadmeLinksTheMovedDetail(t *testing.T) {
	readme := readDoc(t, "README.md")
	for page, url := range movedDetail {
		if _, err := os.Stat(filepath.Join(repoRoot, page)); err != nil {
			t.Errorf("%s: %v", page, err)
		}
		if !strings.Contains(readme, "]("+url) {
			t.Errorf("README.md does not link %s (%s), where its detail moved", url, page)
		}
	}
	for _, anchor := range []string{"operations/scale/#the-readmes-measured-table", "comparison/#kube-upgrade-check"} {
		if !strings.Contains(readme, anchor+")") {
			t.Errorf("README.md does not link the section %s", anchor)
		}
	}
	for page, heading := range map[string]string{
		"docs/operations/scale.md": "\n## The README's Measured table\n",
		"docs/comparison.md":       "\n## kube-upgrade-check\n",
	} {
		if !strings.Contains(readDoc(t, page), heading) {
			t.Errorf("%s has no %q section, which the README links", page, strings.TrimSpace(heading))
		}
	}
}

// section returns doc from the line "## <heading>" to the next "## "
// heading, or "" when there is no such section.
func section(doc, heading string) string {
	_, rest, ok := strings.Cut(doc, "\n## "+heading+"\n")
	if !ok {
		return ""
	}
	rest, _, _ = strings.Cut(rest, "\n## ")
	return rest
}

// measuredFigures are the figures a Measured row can lose in shortening: the
// decimals, the thousands and the commits.
var measuredFigures = regexp.MustCompile("[0-9]+\\.[0-9]+|[0-9]{1,3}(?:,[0-9]{3})+|`[0-9a-f]{7,8}`")

// TestReadmeMeasuredRowsAreOnTheScalePage (#404): the README's Measured
// table is the short form of the one on Scale and cost. Every figure and
// commit in a README row is in that page's table, so a re-measured number
// cannot change in one place only, and each page has the four rows.
func TestReadmeMeasuredRowsAreOnTheScalePage(t *testing.T) {
	rows := func(page, heading string) []string {
		var out []string
		for _, l := range strings.Split(section(readDoc(t, page), heading), "\n") {
			if strings.HasPrefix(l, "| ") && !strings.HasPrefix(l, "| What |") {
				out = append(out, l)
			}
		}
		return out
	}
	readme := rows("README.md", "Measured")
	full := rows("docs/operations/scale.md", "The README's Measured table")
	if len(readme) != 4 || len(full) != 4 {
		t.Fatalf("the README's Measured table has %d rows and Scale and cost's copy %d; want 4 each", len(readme), len(full))
	}
	for i, row := range readme {
		for _, fig := range measuredFigures.FindAllString(row, -1) {
			if !strings.Contains(full[i], fig) {
				t.Errorf("README.md Measured row %d says %s, and row %d of Scale and cost's README table does not: %s", i+1, fig, i+1, full[i])
			}
		}
	}
}

// projectFiles are the files the README's Project section links, each one
// at the repository root (docs/claims.md under docs/).
var projectFiles = []string{
	"CONTRIBUTING.md", "GOVERNANCE.md", "MAINTAINERS.md", "ADOPTERS.md",
	"ROADMAP.md", "SECURITY.md", "docs/claims.md", "CODE_OF_CONDUCT.md",
}

// TestReadmeLinksTheProjectFiles: the README links each project file, and
// each exists.
func TestReadmeLinksTheProjectFiles(t *testing.T) {
	readme := readDoc(t, "README.md")
	for _, f := range projectFiles {
		if _, err := os.Stat(filepath.Join(repoRoot, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
		if !strings.Contains(readme, "]("+f+")") {
			t.Errorf("README.md does not link %s", f)
		}
	}
}

// TestContributingLinksGovernance: CONTRIBUTING.md's opening, before its
// first section, points at GOVERNANCE.md and MAINTAINERS.md.
func TestContributingLinksGovernance(t *testing.T) {
	opening, _, _ := strings.Cut(readDoc(t, "CONTRIBUTING.md"), "\n## ")
	for _, f := range []string{"GOVERNANCE.md", "MAINTAINERS.md"} {
		if !strings.Contains(opening, "]("+f+")") {
			t.Errorf("CONTRIBUTING.md's opening does not link %s", f)
		}
	}
}

// TestMaintainersTable: MAINTAINERS.md has a table with the Name, GitHub,
// Company and Email columns, and at least one row with an @handle and an
// email address.
func TestMaintainersTable(t *testing.T) {
	doc := readDoc(t, "MAINTAINERS.md")
	if !strings.Contains(doc, "| Name | GitHub | Company | Email |") {
		t.Fatal("MAINTAINERS.md has no | Name | GitHub | Company | Email | table")
	}
	row := regexp.MustCompile(`(?m)^\| [^|]+ \| [^|]*@[A-Za-z0-9-]+[^|]* \| [^|]+ \| [^|@ ]+@[^|@ ]+\.[a-z]+ \|$`)
	if !row.MatchString(doc) {
		t.Error("MAINTAINERS.md has no maintainer row with a GitHub @handle and an email")
	}
	if !strings.Contains(doc, "](GOVERNANCE.md") {
		t.Error("MAINTAINERS.md does not say, with a link to GOVERNANCE.md, how maintainers are added")
	}
}

// TestRoadmapLinksMilestones: ROADMAP.md links the GitHub milestones page,
// where the plan's issues are kept.
func TestRoadmapLinksMilestones(t *testing.T) {
	const milestones = "https://github.com/abd-ulbasit/upgradescope/milestones"
	if !strings.Contains(readDoc(t, "ROADMAP.md"), milestones) {
		t.Errorf("ROADMAP.md does not link %s", milestones)
	}
}

// TestRoadmapRegistryCounts: the add-on counts in ROADMAP.md's "Where it
// stands" match registry/data, counted as TestDocsRegistryCounts counts
// them for the registry page, so the roadmap cannot quote a stale
// breakdown.
func TestRoadmapRegistryCounts(t *testing.T) {
	s := loadRegistryStats(t)
	roadmap := strings.Join(strings.Fields(readDoc(t, "ROADMAP.md")), " ")
	for _, want := range []string{
		fmt.Sprintf("%d add-ons in the registry", len(s.ids)),
		fmt.Sprintf("%d synced from endoflife.date", s.eolSynced),
		fmt.Sprintf("%d with Kubernetes ranges", s.k8sRanges),
		fmt.Sprintf("%d with no lifecycle data", s.noData),
	} {
		if !strings.Contains(roadmap, want) {
			t.Errorf("ROADMAP.md: want %q (counted from registry/data)", want)
		}
	}
}

// TestReadmeLeadsWithTheWedge: the README's first screen, before its first
// section, names what upgradescope does that other scanners do not, in the
// order #404 sets (add-on end of life, unserved CRD versions, stored Helm
// releases and chart kubeVersion, pull-request gating), and leaves
// managedFields and the deprecated-API metric, which kube-upgrade-check
// reads too, to the sections below. How it compares names
// kube-upgrade-check and the preflight upgradescope lacks (#397), and the
// README says when to use something else.
func TestReadmeLeadsWithTheWedge(t *testing.T) {
	readme := readDoc(t, "README.md")
	first, _, _ := strings.Cut(readme, "\n## ")
	first = strings.ToLower(strings.Join(strings.Fields(first), " "))
	at := -1
	for _, want := range []string{
		"add-on end of life and compatibility",
		"crd versions no longer served",
		"stored helm release manifests and chart `kubeversion`",
		"pull-request and ci gating",
		"sarif, junit and gitlab",
	} {
		i := strings.Index(first, want)
		if i < 0 {
			t.Errorf("README.md's first screen does not name %q", want)
			continue
		}
		if i < at {
			t.Errorf("README.md's first screen names %q out of #404's order", want)
		}
		at = i
	}
	for _, feature := range []string{"managedfields", "apiserver_requested_deprecated_apis"} {
		if strings.Contains(first, feature) {
			t.Errorf("README.md's first screen headlines %q, which kube-upgrade-check reads too: keep it a feature below", feature)
		}
	}
	_, compares, _ := strings.Cut(readme, "\n## How it compares")
	compares, _, _ = strings.Cut(compares, "\n## ")
	compares = strings.Join(strings.Fields(compares), " ")
	for _, want := range []string{"kube-upgrade-check", "preflight", "issues/397", "**Use something else when**"} {
		if !strings.Contains(compares, want) {
			t.Errorf("README.md's How it compares does not say %q", want)
		}
	}
}
