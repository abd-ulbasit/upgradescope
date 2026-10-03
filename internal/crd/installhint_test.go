package crd

import (
	"strings"
	"testing"
)

// The command names the binary's release, so the manifest it fetches is the
// one this binary's schema matches. Both the stamped form ("0.2.0") and the
// tag form ("v0.2.0") name the same tag.
func TestInstallCommandUsesTheBinarysRelease(t *testing.T) {
	for _, v := range []string{"0.2.0", "v0.2.0"} {
		want := "kubectl apply -f https://raw.githubusercontent.com/abd-ulbasit/upgradescope/v0.2.0/deploy/chart/crds/" + ManifestFile
		if got := InstallCommand(v); got != want {
			t.Errorf("InstallCommand(%q) = %q, want %q", v, got, want)
		}
	}
	if got := InstallCommand("0.2.0-rc.2"); !strings.Contains(got, "/v0.2.0-rc.2/deploy/") {
		t.Errorf("InstallCommand(rc) = %q, want the rc tag", got)
	}
}

// A dev build has no release tag: the hint carries a placeholder and says
// what goes there instead of a URL that 404s.
func TestInstallHintDevBuildSaysWhatTagToUse(t *testing.T) {
	for _, v := range []string{"dev", "dev+0123456789ab.dirty", "0.2.0-SNAPSHOT-abc", "",
		// Go pseudo-versions (go install ...@main, or a VCS-stamped build not
		// at a tag) look like releases but name no tag.
		"0.2.1-0.20261004120000-abcdef123456", "v0.2.1-0.20261004120000-abcdef123456",
		"0.2.0-rc.2.0.20261004120000-abcdef123456"} {
		h := InstallHint(v)
		if !strings.Contains(h, "/<tag>/deploy/chart/crds/"+ManifestFile) || !strings.Contains(h, "dev build") ||
			!strings.Contains(h, "replace <tag>") {
			t.Errorf("InstallHint(%q) = %q, want the <tag> placeholder explained", v, h)
		}
	}
}

func TestInstallHintRelease(t *testing.T) {
	h := InstallHint("0.2.0")
	for _, want := range []string{
		"kubectl apply -f https://raw.githubusercontent.com/abd-ulbasit/upgradescope/v0.2.0/deploy/chart/crds/" + ManifestFile,
		"helm pull oci://ghcr.io/abd-ulbasit/charts/upgradescope --version 0.2.0 --untar",
		"kubectl apply -f upgradescope/crds/",
		"install it with: kubectl apply",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("InstallHint = %q\nwant %q", h, want)
		}
	}
	if strings.Contains(h, "<tag>") || strings.Contains(h, "dev build") {
		t.Errorf("InstallHint = %q, a release names its tag", h)
	}
}
