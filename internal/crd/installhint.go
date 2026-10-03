package crd

import (
	"regexp"
	"strings"
)

// ManifestFile is the CRD's file name under the chart's crds/ directory.
const ManifestFile = CRDName + ".yaml"

// rawBase is where a release's files are served by tag.
const rawBase = "https://raw.githubusercontent.com/abd-ulbasit/upgradescope/"

// chartRef is the chart's OCI reference.
const chartRef = "oci://ghcr.io/abd-ulbasit/charts/upgradescope"

// releaseVersion matches a stamped release version, with or without the
// leading "v": MAJOR.MINOR.PATCH and an optional pre-release. A dev build's
// "dev" and "dev+<rev>", or a snapshot's "-SNAPSHOT", name no tag.
var releaseVersion = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// pseudoVersion matches a Go pseudo-version's tail: what "go install
// ...@main" stamps for a commit that is not at a tag, such as
// 0.2.1-0.20261004120000-abcdef123456 or
// 0.2.0-rc.2.0.20261004120000-abcdef123456. It fits the pre-release form
// but names no tag.
var pseudoVersion = regexp.MustCompile(`[-.]0\.\d{14}-[0-9a-f]{12}$`)

// isRelease reports whether version names a release tag. Snapshots and
// pseudo-versions match the pre-release form but are never published as
// tags.
func isRelease(version string) bool {
	return releaseVersion.MatchString(version) && !strings.Contains(version, "SNAPSHOT") &&
		!pseudoVersion.MatchString(version)
}

// InstallCommand is the command that installs the ClusterReadiness CRD from
// the release named by version, the binary's. Helm installs crds/ on first
// install only, so an upgrade needs it by hand. A version that names no
// release tag (a dev build) gets the placeholder <tag>, which InstallHint
// explains.
func InstallCommand(version string) string {
	tag := "<tag>"
	if isRelease(version) {
		tag = "v" + strings.TrimPrefix(version, "v")
	}
	return "kubectl apply -f " + rawBase + tag + "/deploy/chart/crds/" + ManifestFile
}

// InstallHint says how to install the CRD when the agent may not: the
// kubectl apply of the release's manifest, and the same from the chart's
// crds/ directory after helm pull. For a dev build it says which tag to use.
func InstallHint(version string) string {
	var b strings.Builder
	b.WriteString("install it with: ")
	b.WriteString(InstallCommand(version))
	b.WriteString(" (or helm pull " + chartRef)
	if isRelease(version) {
		b.WriteString(" --version " + strings.TrimPrefix(version, "v"))
	} else {
		b.WriteString(" --version <chart version>")
	}
	b.WriteString(" --untar, then kubectl apply -f upgradescope/crds/)")
	if !isRelease(version) {
		b.WriteString("; this is a dev build, so replace <tag> with the release tag of the chart you installed (v + its version)")
	}
	return b.String()
}
