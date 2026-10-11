package cli

// Tests that keep the project's governance files present and linked:
// MAINTAINERS.md has the columns the CNCF Sandbox form checks, CONTRIBUTING
// points at the governance, and the roadmap links its milestones.

import (
	"regexp"
	"strings"
	"testing"
)

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
