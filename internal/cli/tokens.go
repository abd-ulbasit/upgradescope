package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// secretFlag is a secret-bearing string flag with two sources that stay
// out of argv, where anyone who can list processes reads it: an
// environment variable, and --<name>-file for a mounted Secret or systemd
// credential (env is in turn visible in /proc/<pid>/environ). An explicitly
// set flag wins, then the file, then the environment; the flag and its
// -file variant are mutually exclusive.
type secretFlag struct {
	name string  // flag name, e.g. "ingest-token"
	env  string  // environment variable, e.g. "UPGRADESCOPE_INGEST_TOKEN"
	dst  *string // the flag's value; resolve fills it from the file or env
	file string  // --<name>-file
}

// addSecretFlag registers --<name> and --<name>-file on cmd. Call resolve
// in RunE before reading *dst.
func addSecretFlag(cmd *cobra.Command, dst *string, name, env, usage string) *secretFlag {
	s := &secretFlag{name: name, env: env, dst: dst}
	cmd.Flags().StringVar(dst, name, "",
		fmt.Sprintf("%s (visible in process listings: prefer $%s or --%s-file)", usage, env, name))
	cmd.Flags().StringVar(&s.file, name+"-file", "",
		fmt.Sprintf("read --%s from this file, e.g. a mounted Secret (surrounding whitespace is trimmed)", name))
	cmd.MarkFlagsMutuallyExclusive(name, name+"-file")
	return s
}

// resolve fills the value from --<name>-file or the environment unless the
// flag itself was set (even to "").
func (s *secretFlag) resolve(cmd *cobra.Command) error {
	if cmd.Flags().Changed(s.name) {
		return nil
	}
	if s.file != "" {
		raw, err := os.ReadFile(s.file)
		if err != nil {
			return fmt.Errorf("--%s-file: %w", s.name, err)
		}
		v := strings.TrimSpace(string(raw))
		if v == "" {
			return fmt.Errorf("--%s-file %s is empty", s.name, s.file)
		}
		*s.dst = v
		return nil
	}
	*s.dst = os.Getenv(s.env)
	return nil
}

// dbFlags is the shared --db / --db-url pair: SQLite path (default) or
// Postgres URL, mutually exclusive. serve and every tokens subcommand use
// the same selection rule via openStore. The URL carries the Postgres
// password, so it is a secretFlag ($UPGRADESCOPE_DB_URL, --db-url-file).
type dbFlags struct {
	db    string
	dbURL string

	dbURLSecret *secretFlag
}

// register adds the pair to cmd and marks them mutually exclusive
// (exclusivity triggers only when BOTH are explicitly set — the --db
// default plus --db-url is fine).
func (f *dbFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.db, "db", "upgradescope.db", "path to the SQLite database (parent directory is created)")
	f.dbURLSecret = addSecretFlag(cmd, &f.dbURL, "db-url", "UPGRADESCOPE_DB_URL",
		"Postgres URL (postgres://user:pass@host:5432/db); mutually exclusive with --db")
	cmd.MarkFlagsMutuallyExclusive("db", "db-url")
	cmd.MarkFlagsMutuallyExclusive("db", "db-url-file")
}

// resolve applies --db-url-file / $UPGRADESCOPE_DB_URL. An explicit --db
// wins over the environment: a stray env var must not silently switch an
// explicit SQLite choice to Postgres.
func (f *dbFlags) resolve(cmd *cobra.Command) error {
	if cmd.Flags().Changed("db") {
		return nil
	}
	return f.dbURLSecret.resolve(cmd)
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
		Short: "Manage per-cluster ingest tokens (each authenticates pushes for one cluster only)",
	}
	cmd.AddCommand(newTokensCreateCmd())
	cmd.AddCommand(newTokensListCmd())
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
			if err := flags.resolve(cmd); err != nil {
				return err
			}
			st, err := flags.openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			id, err := st.CreateToken(cmd.Context(), cluster, token)
			if err != nil {
				return fmt.Errorf("create token: %w", err)
			}
			// Token alone on stdout (script-friendly); context on stderr so
			// `tokens create prod > secret` captures only the secret.
			fmt.Fprintln(cmd.OutOrStdout(), token)
			fmt.Fprintf(cmd.ErrOrStderr(),
				"ingest token id %d (prefix %s) for cluster %q created — shown once, only its hash is stored\n",
				id, store.TokenPrefix(token), cluster)
			return nil
		},
	}
	flags.register(cmd)
	return cmd
}

func newTokensListCmd() *cobra.Command {
	var (
		flags   dbFlags
		cluster string
	)
	cmd := &cobra.Command{
		Use:           "list",
		Short:         "List ingest tokens (id, cluster, prefix, created, revoked); never prints a token",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := flags.resolve(cmd); err != nil {
				return err
			}
			st, err := flags.openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			toks, err := st.ListTokens(cmd.Context(), cluster)
			if err != nil {
				return fmt.Errorf("list tokens: %w", err)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tCLUSTER\tPREFIX\tCREATED\tREVOKED")
			for _, tk := range toks {
				prefix, revoked := tk.Prefix, "-"
				if prefix == "" {
					prefix = "-" // minted before prefixes were stored
				}
				if tk.RevokedAt != nil {
					revoked = tk.RevokedAt.Format(time.RFC3339)
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
					tk.ID, tk.ClusterName, prefix, tk.CreatedAt.Format(time.RFC3339), revoked)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", "", "only list this cluster's tokens")
	flags.register(cmd)
	return cmd
}

func newTokensRevokeCmd() *cobra.Command {
	var (
		flags dbFlags
		id    int64
		all   bool
	)
	cmd := &cobra.Command{
		Use:   "revoke <cluster> (--id <id> | --all)",
		Short: "Revoke one ingest token of a cluster by id, or all of them with --all",
		Long: "Revoke one ingest token by id (see 'tokens list'), or every active token of the cluster with --all.\n" +
			"Zero-downtime rotation: 'tokens create <cluster>', roll the new token out to the agent, then\n" +
			"'tokens revoke <cluster> --id <old id>'.",
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
			if !all {
				if err := st.RevokeTokenID(cmd.Context(), cluster, id); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return fmt.Errorf("no active token %d for cluster %q", id, cluster)
					}
					return fmt.Errorf("revoke token: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "revoked ingest token %d of cluster %q\n", id, cluster)
				return nil
			}
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
	cmd.Flags().Int64Var(&id, "id", 0, "revoke only the token with this id (from 'tokens list' or 'tokens create')")
	cmd.Flags().BoolVar(&all, "all", false, "revoke every active token of the cluster")
	cmd.MarkFlagsOneRequired("id", "all")
	cmd.MarkFlagsMutuallyExclusive("id", "all")
	flags.register(cmd)
	return cmd
}
