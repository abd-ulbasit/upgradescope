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
		Short: "Delete a cluster with its snapshots and evaluations; its ingest tokens are kept",
		Long: "Delete a cluster record with its snapshot and evaluation history.\n" +
			"A cluster name is bound to the cluster UID it first pushed; the server answers 409 to a\n" +
			"push from another UID. When the cluster was rebuilt (new UID, same name), delete the old\n" +
			"record and the next push registers the new one. Ingest tokens are keyed by name and keep\n" +
			"working.",
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
