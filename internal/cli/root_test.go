package cli

import (
	"runtime/debug"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	buildInfo := func(mainVersion string, settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{
				Main:     debug.Module{Path: "github.com/abd-ulbasit/upgradescope", Version: mainVersion},
				Settings: settings,
			}, true
		}
	}
	noBuildInfo := func() (*debug.BuildInfo, bool) { return nil, false }
	rev := debug.BuildSetting{Key: "vcs.revision", Value: "4bca610f1e2d3c4b5a69788796a5b4c3d2e1f0a9"}
	dirty := debug.BuildSetting{Key: "vcs.modified", Value: "true"}
	clean := debug.BuildSetting{Key: "vcs.modified", Value: "false"}

	tests := []struct {
		name    string
		stamped string
		read    func() (*debug.BuildInfo, bool)
		want    string
	}{
		{"ldflags stamp wins", "0.2.0", buildInfo("v0.1.1"), "0.2.0"},
		// Same form as GoReleaser's {{ .Version }} stamp (no leading v), so
		// one release reads the same in SARIF and agent snapshots however it
		// was installed.
		{"go install @vX: module version without the v", "dev", buildInfo("v0.1.1"), "0.1.1"},
		{"go install @commit: pseudo-version without the v", "dev", buildInfo("v0.1.2-0.20261001120000-4bca610f1e2d"), "0.1.2-0.20261001120000-4bca610f1e2d"},
		{"local build without version: revision", "dev", buildInfo("(devel)", rev, clean), "dev+4bca610f1e2d"},
		{"local build with edits: revision marked dirty", "dev", buildInfo("(devel)", rev, dirty), "dev+4bca610f1e2d.dirty"},
		{"no module version, no vcs", "dev", buildInfo("(devel)"), "dev"},
		{"no build info at all", "dev", noBuildInfo, "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveVersion(tt.stamped, tt.read); got != tt.want {
				t.Errorf("resolveVersion(%q) = %q, want %q", tt.stamped, got, tt.want)
			}
		})
	}
}
