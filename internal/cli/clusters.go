package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

func newClustersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clusters",
		Short: "Manage clusters registered on the server database",
	}
	cmd.AddCommand(newClustersDeleteCmd())
	return cmd
}

func newClustersDeleteCmd() *cobra.Command {
	var flags dbFlags
	cmd := &cobra.Command{
		Use:   "delete <cluster>",
		Short: "Delete a cluster with its history, queued notifications and ingest tokens",
		Long: "Delete a cluster record with its snapshot and evaluation history, its queued\n" +
			"notifications and the per-cluster ingest tokens minted for its name.\n" +
			"A cluster name is bound to the cluster UID it first pushed; the server answers 409 to a\n" +
			"push from another UID. When the cluster was rebuilt (new UID, same name), delete the old\n" +
			"record and the next push registers the new one; an agent that used a per-cluster token\n" +
			"needs a new one ('upgradescope tokens create').",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster := args[0]
			if err := flags.resolve(cmd); err != nil {
				return err
			}
			st, err := flags.openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.DeleteCluster(cmd.Context(), cluster); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("no cluster %q", cluster)
				}
				return fmt.Errorf("delete cluster: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted cluster %q and its history\n", cluster)
			return nil
		},
	}
	flags.register(cmd)
	return cmd
}
