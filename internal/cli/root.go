package cli

import "github.com/spf13/cobra"

var version = "dev" // set via -ldflags

func Root() *cobra.Command {
	root := &cobra.Command{
		Use:           "upgradescope",
		Short:         "Continuous Kubernetes upgrade-readiness scanner",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newScanCmd())
	return root
}
