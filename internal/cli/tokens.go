package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
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
// flag itself was set (even to ""). Either source is trimmed of surrounding
// whitespace, and one that holds nothing else is refused.
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
	// Surrounding whitespace is trimmed as from a file: a Secret written
	// with a trailing newline reaches the environment too, and a token
	// "tok\n" no client can present.
	if raw := os.Getenv(s.env); raw != "" {
		v := strings.TrimSpace(raw)
		if v == "" {
			return fmt.Errorf("$%s holds only whitespace", s.env)
		}
		*s.dst = v
	}
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
		// The owner's only, like the database (store.Open makes it 0600).
		if err := os.MkdirAll(dir, 0o700); err != nil {
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
// plaintext shown exactly once. The store keeps its sha256 and its first 8
// characters (store.TokenPrefix), which `tokens list` prints so tokens can
// be told apart; 8 of 64 hex characters leave 224 bits unknown.
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
		Short: "Manage per-cluster ingest tokens and team-scoped read tokens",
		Long: `Manage the tokens the server keeps in its database.

Ingest tokens (the default) authenticate snapshot pushes for one cluster
name only, so a leaked token cannot write another cluster's history.

Read tokens (--read) authenticate the read API, the dashboard's data,
/api/v1/gate and /metrics, each for a set of teams (--teams a --teams b) or for the
whole fleet (--teams '*'). A team-scoped token reads only the clusters its
teams own a namespace in, and of those only its teams' findings and team
scores: any other cluster answers 404, as an unknown one does, and is left
out of every list and rollup. /metrics takes a fleet-wide token only. Once
one read token has been minted, the read API needs a credential.

The server database keeps each token's sha256 hash and its first 8
characters (which "tokens list" shows), never the token. The server reads
the tokens from the database on every request: one minted or revoked
here takes effect without a restart.`,
	}
	cmd.AddCommand(newTokensCreateCmd())
	cmd.AddCommand(newTokensListCmd())
	cmd.AddCommand(newTokensRevokeCmd())
	return cmd
}

func newTokensCreateCmd() *cobra.Command {
	var (
		flags dbFlags
		read  bool
		teams []string
	)
	cmd := &cobra.Command{
		Use:   "create (<cluster> | --read --teams <team|*> [--teams <team>]...)",
		Short: "Mint an ingest token bound to one cluster, or a read token scoped to teams",
		Long: `Mint an ingest token bound to one cluster, or with --read a read token
scoped to the teams the --teams flags name, one team per flag, taken as
written ('*' alone: the whole fleet). The
plaintext token is printed once, to stdout. The server stores its sha256
hash and its first 8 characters (which "tokens list" shows), never the
token. Give an ingest token to that cluster's agent (--server-token-file,
or the chart's agent.existingSecret), and a read token to the team's
dashboard users or CI.`,
		Example: `  upgradescope tokens create prod-eu --db upgradescope.db
  upgradescope tokens create prod-eu --db-url-file /secrets/db-url > prod-eu.token
  upgradescope tokens create --read --teams payments --teams checkout > payments.token
  upgradescope tokens create --read --teams 'Platform Team' > platform.token
  upgradescope tokens create --read --teams '*' > fleet.token`,
		Args: func(cmd *cobra.Command, args []string) error {
			if read {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if read {
				return createReadToken(cmd, flags, teams)
			}
			if cmd.Flags().Changed("teams") {
				return errors.New("--teams scopes a read token: pass --read too")
			}
			cluster := args[0]
			// The server refuses pushes under any other name, so a token
			// bound to one could never be used.
			if err := inventory.ValidateClusterName(cluster); err != nil {
				return fmt.Errorf("cluster name: %w", err)
			}
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
				"ingest token id %d (prefix %s) for cluster %q created — shown once: the server stores its sha256 hash and its first 8 characters, never the token\n",
				id, store.TokenPrefix(token), cluster)
			return nil
		},
	}
	cmd.Flags().BoolVar(&read, "read", false, "mint a read token for the read API, dashboard, /api/v1/gate and /metrics instead of an ingest token; needs --teams")
	cmd.Flags().StringArrayVar(&teams, "teams", nil, "with --read: a team the token reads, as the team label or --team-map names it (repeat the flag for more; the value is never split), or '*' alone for the whole fleet")
	flags.register(cmd)
	return cmd
}

// readTokenTeams validates --teams: at least one team, none empty or
// over maxTeamName, and '*' only alone. Each flag is one team, taken as
// written: a team is free text ("Platform Team", "Équipe, Paris"), so a
// value is never split or trimmed. A team with a comma is most likely a
// list passed to one flag, so it is warned about on stderr; the token is
// minted for the team so named all the same. The result is sorted and
// deduplicated.
func readTokenTeams(teams []string, stderr io.Writer) ([]string, error) {
	out := make([]string, 0, len(teams))
	for _, t := range teams {
		switch {
		case t == "":
			return nil, errors.New("--teams: a team name must be non-empty")
		case len(t) > maxTeamName:
			return nil, fmt.Errorf("--teams: team %q is longer than %d characters", t, maxTeamName)
		case !utf8.ValidString(t) || strings.ContainsFunc(t, unicode.IsControl):
			return nil, fmt.Errorf("--teams: team %q is not printable UTF-8 text", t)
		case strings.Contains(t, ","):
			fmt.Fprintf(stderr, "note: --teams %q is one team whose name holds a comma; for several teams repeat the flag (--teams a --teams b)\n", t)
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--read needs --teams: the teams the token reads, or '%s' for the whole fleet", store.ReadScopeFleet)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if slices.Contains(out, store.ReadScopeFleet) && len(out) > 1 {
		return nil, fmt.Errorf("--teams: '%s' is the whole fleet and stands alone, got %s", store.ReadScopeFleet, teamList(out))
	}
	return out, nil
}

// teamList prints teams for a person: comma separated, each quoted when
// it holds a comma, a quote, a space or anything not printable, so a list
// of free-text team names reads unambiguously.
func teamList(teams []string) string {
	out := make([]string, len(teams))
	for i, t := range teams {
		out[i] = t
		if t == "" || strings.ContainsFunc(t, func(r rune) bool { return r == ',' || r == '"' || unicode.IsSpace(r) || !unicode.IsPrint(r) }) {
			out[i] = strconv.Quote(t)
		}
	}
	return strings.Join(out, ",")
}

// maxTeamName bounds a --teams entry: a team label value is at most 63
// characters, and a --team-map team is free text, so allow more.
const maxTeamName = 253

// createReadToken is `tokens create --read --teams ...`.
func createReadToken(cmd *cobra.Command, flags dbFlags, raw []string) error {
	teams, err := readTokenTeams(raw, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
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
	id, err := st.CreateReadToken(cmd.Context(), teams, token)
	if err != nil {
		return fmt.Errorf("create read token: %w", err)
	}
	scope := "teams " + teamList(teams)
	if teams[0] == store.ReadScopeFleet {
		scope = "the whole fleet"
	}
	fmt.Fprintln(cmd.OutOrStdout(), token)
	fmt.Fprintf(cmd.ErrOrStderr(),
		"read token id %d (prefix %s) for %s created — shown once: the server stores its sha256 hash and its first 8 characters, never the token; "+
			"from now on the server's read API needs a credential\n",
		id, store.TokenPrefix(token), scope)
	return nil
}

func newTokensListCmd() *cobra.Command {
	var (
		flags   dbFlags
		cluster string
		read    bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List ingest tokens, or read tokens with --read; never prints a token",
		Long: "List ingest tokens: id, cluster, prefix, created and revoked times; or with --read the read tokens:\n" +
			"id, teams ('*' = the whole fleet), prefix, created and revoked times. A token itself is never shown.",
		Example: `  upgradescope tokens list --db upgradescope.db
  upgradescope tokens list --cluster prod-eu
  upgradescope tokens list --read`,
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
			if read {
				return listReadTokens(cmd, st)
			}
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
					tk.ID, esc(tk.ClusterName), esc(prefix), tk.CreatedAt.Format(time.RFC3339), revoked)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", "", "only list this cluster's tokens")
	cmd.Flags().BoolVar(&read, "read", false, "list the read tokens instead of the ingest tokens")
	cmd.MarkFlagsMutuallyExclusive("cluster", "read")
	flags.register(cmd)
	return cmd
}

// listReadTokens is `tokens list --read`.
func listReadTokens(cmd *cobra.Command, st store.Store) error {
	toks, err := st.ListReadTokens(cmd.Context())
	if err != nil {
		return fmt.Errorf("list read tokens: %w", err)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTEAMS\tPREFIX\tCREATED\tREVOKED")
	for _, tk := range toks {
		revoked := "-"
		if tk.RevokedAt != nil {
			revoked = tk.RevokedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
			tk.ID, esc(teamList(tk.Teams)), esc(tk.Prefix), tk.CreatedAt.Format(time.RFC3339), revoked)
	}
	return tw.Flush()
}

func newTokensRevokeCmd() *cobra.Command {
	var (
		flags dbFlags
		id    int64
		all   bool
		read  bool
	)
	cmd := &cobra.Command{
		Use:   "revoke (<cluster> (--id <id> | --all) | --read --id <id>)",
		Short: "Revoke ingest tokens of a cluster (--id or --all), or a read token (--read --id)",
		Example: `  upgradescope tokens revoke prod-eu --id 3
  upgradescope tokens revoke prod-eu --all
  upgradescope tokens revoke --read --id 2`,
		Long: "Revoke one ingest token by id (see 'tokens list'), or every active token of the cluster with --all.\n" +
			"Zero-downtime rotation: 'tokens create <cluster>', roll the new token out to the agent, then\n" +
			"'tokens revoke <cluster> --id <old id>'. The agent reads its token at startup, so rolling it out\n" +
			"means restarting the agent after updating its Secret (with the chart, kubectl rollout restart deploy/<fullname>-agent).\n\n" +
			"With --read, revoke the read token --id names (see 'tokens list --read'). The server refuses it\n" +
			"from its next request on. Revoking the last read token does not open the read API again.",
		Args: func(cmd *cobra.Command, args []string) error {
			if read {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if read && all {
				return errors.New("--read revokes one read token: pass --id, not --all")
			}
			if err := flags.resolve(cmd); err != nil {
				return err
			}
			st, err := flags.openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			if read {
				if err := st.RevokeReadToken(cmd.Context(), id); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return fmt.Errorf("no active read token %d", id)
					}
					return fmt.Errorf("revoke read token: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "revoked read token %d\n", id)
				return nil
			}
			cluster := args[0]
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
	cmd.Flags().BoolVar(&read, "read", false, "revoke the read token --id names instead of an ingest token")
	cmd.MarkFlagsOneRequired("id", "all")
	cmd.MarkFlagsMutuallyExclusive("id", "all")
	flags.register(cmd)
	return cmd
}
