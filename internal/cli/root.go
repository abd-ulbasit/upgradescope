package cli

import (
	"runtime/debug"
	"strings"
	"sync"

	"github.com/spf13/cobra"
)

// version is stamped via -ldflags "-X …/internal/cli.version=…" by release
// builds (GoReleaser, the Dockerfile). Builds that skip the stamp — notably
// `go install …@vX` — fall back to the Go build info, so --version, SARIF and
// agent snapshots name a real version instead of "dev". It must stay a
// constant initializer: -X has no effect on a variable initialized by a call.
var version = "dev"

func init() { version = resolveVersion(version, debug.ReadBuildInfo) }

// resolveVersion returns stamped unless it is the "dev" placeholder;
// otherwise the main module version recorded by the go command (set for
// `go install …@vX` and, since Go 1.24, for builds inside a tagged git
// checkout) without its leading "v", matching GoReleaser's {{ .Version }}
// stamp; then the VCS revision ("dev+<12 hex>[.dirty]"), then "dev".
func resolveVersion(stamped string, readBuildInfo func() (*debug.BuildInfo, bool)) string {
	if stamped != "" && stamped != "dev" {
		return stamped
	}
	info, ok := readBuildInfo()
	if !ok || info == nil {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return strings.TrimPrefix(v, "v")
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		return "dev+" + rev + ".dirty"
	}
	return "dev+" + rev
}

// registerVersionTemplate makes --version print what `upgradescope version`
// does. The text is computed when --version is used, not when the command
// tree is built, so other commands never pay for loading the KB.
var registerVersionTemplate = sync.OnceFunc(func() {
	cobra.AddTemplateFunc("upgradescopeVersion", func() string {
		var b strings.Builder
		writeVersionText(&b, currentVersionInfo())
		return b.String()
	})
})

func Root() *cobra.Command {
	registerVersionTemplate()
	root := &cobra.Command{
		Use:   "upgradescope",
		Short: "Continuous Kubernetes upgrade-readiness scanner",
		Long: `upgradescope finds what blocks a Kubernetes upgrade before you start it:
deprecated and removed APIs (stored objects and live callers), end-of-life
add-ons, version skew and chart compatibility, rolled up into a readiness
score and verdict.

Run it once with 'scan', continuously in the cluster with 'agent', and across
a fleet with 'serve'.`,
		Example: `  # Is the current kubeconfig context's cluster ready for Kubernetes 1.37?
  upgradescope scan --target 1.37

  # Gate a pull request on rendered manifests
  helm template ./chart --output-dir rendered
  upgradescope scan --files rendered --target 1.37 --output sarif > upgradescope.sarif`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate(`{{ upgradescopeVersion }}`)
	root.AddCommand(newScanCmd())
	root.AddCommand(newAgentCmd())
	root.AddCommand(newServeCmd())
	root.AddCommand(newTokensCmd())
	root.AddCommand(newClustersCmd())
	root.AddCommand(newVersionCmd())
	return root
}
