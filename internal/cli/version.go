package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// Build metadata stamped by release builds (GoReleaser's ldflags), next to
// version in root.go. Like version, they must stay constant initializers.
//   - commit: the full commit hash;
//   - date: the commit time (not the build clock, so a rebuild of a tag is
//     byte-identical);
//   - registryDate: the date of the last commit to registry/data, i.e. how
//     fresh the embedded add-on EOL data is.
//
// Unstamped builds fall back to the VCS stamp the go command records in a
// git checkout; `go install …@vX` has none, so they read "unknown".
var (
	commit       = ""
	date         = ""
	registryDate = ""
)

// resolveBuild returns the stamped commit and date, else the vcs.revision
// and vcs.time build settings, else "unknown".
func resolveBuild(stampedCommit, stampedDate string, readBuildInfo func() (*debug.BuildInfo, bool)) (string, string) {
	c, d := stampedCommit, stampedDate
	if info, ok := readBuildInfo(); ok && info != nil {
		for _, s := range info.Settings {
			switch {
			case s.Key == "vcs.revision" && c == "":
				c = s.Value
			case s.Key == "vcs.time" && d == "":
				d = s.Value
			}
		}
	}
	if c == "" {
		c = "unknown"
	}
	if d == "" {
		d = "unknown"
	}
	return c, d
}

// versionInfo is `upgradescope version --output json`. Field names are
// stable; new fields may be added.
type versionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
	// KBVersion is the knowledge base label reports carry (the k8s.io/api
	// release plus dataset digests); KBHorizon is the newest Kubernetes
	// minor its lifecycle data covers: --target beyond it warns kb-stale.
	KBVersion    string `json:"kbVersion"`
	KBHorizon    string `json:"kbHorizon"`
	RegistryDate string `json:"registryDate"`
}

func currentVersionInfo() versionInfo {
	c, d := resolveBuild(commit, date, debug.ReadBuildInfo)
	v := versionInfo{
		Version:      version,
		Commit:       c,
		BuildDate:    d,
		GoVersion:    runtime.Version(),
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
		RegistryDate: registryDate,
	}
	if v.RegistryDate == "" {
		v.RegistryDate = "unknown"
	}
	// A KB that fails to load is a broken build; version still answers,
	// naming the error, since it is what a bug report starts from.
	if k, err := kb.Load(); err != nil {
		v.KBVersion, v.KBHorizon = "error: "+err.Error(), "unknown"
	} else {
		v.KBVersion, v.KBHorizon = k.Version, k.MaxKnownK8s.String()
	}
	return v
}

func writeVersionText(w io.Writer, v versionInfo) {
	fmt.Fprintf(w, "upgradescope %s\n", v.Version)
	fmt.Fprintf(w, "  commit:        %s\n", v.Commit)
	fmt.Fprintf(w, "  built:         %s\n", v.BuildDate)
	fmt.Fprintf(w, "  go:            %s %s\n", v.GoVersion, v.Platform)
	fmt.Fprintf(w, "  kb:            %s\n", v.KBVersion)
	fmt.Fprintf(w, "  kb horizon:    Kubernetes %s\n", v.KBHorizon)
	fmt.Fprintf(w, "  registry date: %s\n", v.RegistryDate)
}

func newVersionCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version, build and knowledge base details",
		Long: `Print the upgradescope version, the commit and date it was built from, the Go
toolchain, and the embedded knowledge base: the k8s.io/api release its API
lifecycle data comes from, the newest Kubernetes minor it covers (the
horizon: --target beyond it reports a kb-stale warning), and the date the
add-on registry was last updated. --version prints the same text.`,
		Example: `  upgradescope version
  upgradescope version --output json | jq -r .kbHorizon`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch output {
			case "text":
				writeVersionText(cmd.OutOrStdout(), currentVersionInfo())
				return nil
			case "json":
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(currentVersionInfo())
			default:
				return fmt.Errorf("invalid --output %q (want text or json)", output)
			}
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text|json")
	return cmd
}
