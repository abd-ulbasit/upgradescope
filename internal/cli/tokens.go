package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// dbFlags is the shared --db / --db-url pair: SQLite path (default) or
// Postgres URL, mutually exclusive. serve and every tokens subcommand use
// the same selection rule via openStore.
type dbFlags struct {
	db    string
	dbURL string
}

// register adds the pair to cmd and marks them mutually exclusive
// (exclusivity triggers only when BOTH are explicitly set — the --db
// default plus --db-url is fine).
func (f *dbFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.db, "db", "upgradescope.db", "path to the SQLite database (parent directory is created)")
	cmd.Flags().StringVar(&f.dbURL, "db-url", "", "Postgres URL (postgres://user:pass@host:5432/db); mutually exclusive with --db")
	cmd.MarkFlagsMutuallyExclusive("db", "db-url")
}

// openStore opens the selected backend: Postgres when --db-url is set,
// otherwise SQLite at --db (creating parent directories).
func (f dbFlags) openStore() (store.Store, error) {
	if f.dbURL != "" {
		st, err := store.OpenPostgres(f.dbURL)
		if err != nil {
			return nil, fmt.Errorf("open postgres store: %w", err)
		}
		return st, nil
	}
	if dir := filepath.Dir(f.db); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}
	st, err := store.Open(f.db)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	return st, nil
}

// generateToken returns 32 bytes of crypto/rand as 64 hex chars — the
// plaintext shown exactly once; only its sha256 is persisted.
func generateToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func newTokensCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tokens",
		Short: "Manage per-cluster ingest tokens (the shared --ingest-token suits single-cluster/dev setups)",
	}
	cmd.AddCommand(newTokensCreateCmd())
	cmd.AddCommand(newTokensRevokeCmd())
	return cmd
}

func newTokensCreateCmd() *cobra.Command {
	var flags dbFlags
	cmd := &cobra.Command{
		Use:           "create <cluster>",
		Short:         "Mint an ingest token bound to one cluster; the plaintext is printed once to stdout",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster := args[0]
			token, err := generateToken()
			if err != nil {
				return err
			}
			st, err := flags.openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.CreateToken(cmd.Context(), cluster, token); err != nil {
				return fmt.Errorf("create token: %w", err)
			}
			// Token alone on stdout (script-friendly); context on stderr so
			// `tokens create prod > secret` captures only the secret.
			fmt.Fprintln(cmd.OutOrStdout(), token)
			fmt.Fprintf(cmd.ErrOrStderr(),
				"ingest token for cluster %q created — shown once, only its hash is stored\n", cluster)
			return nil
		},
	}
	flags.register(cmd)
	return cmd
}

func newTokensRevokeCmd() *cobra.Command {
	var flags dbFlags
	cmd := &cobra.Command{
		Use:           "revoke <cluster>",
		Short:         "Revoke ALL active ingest tokens of a cluster",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster := args[0]
			st, err := flags.openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.RevokeToken(cmd.Context(), cluster); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("no active tokens for cluster %q", cluster)
				}
				return fmt.Errorf("revoke tokens: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked all active ingest tokens for cluster %q\n", cluster)
			return nil
		},
	}
	flags.register(cmd)
	return cmd
}
