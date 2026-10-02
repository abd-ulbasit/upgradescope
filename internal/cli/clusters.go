package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// clusterAdminTimeout bounds each request to a server.
const clusterAdminTimeout = 30 * time.Second

func newClustersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clusters",
		Short: "List, delete and rename the clusters a server knows",
		Long: "List, delete and rename the clusters an upgradescope server knows.\n" +
			"With --server, the commands call the server's API: list needs the read token (or the\n" +
			"admin token), delete and rename the admin token (serve --admin-token). Without --server\n" +
			"they open the server's database directly (--db or --db-url), e.g. while it is stopped;\n" +
			"one of the two is required.",
	}
	cmd.AddCommand(newClustersListCmd())
	cmd.AddCommand(newClustersDeleteCmd())
	cmd.AddCommand(newClustersRenameCmd())
	return cmd
}

// clusterRow is one cluster as the commands print it. Stale is known only
// from a server (it depends on the server's --stale-after).
type clusterRow struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	ClusterUID string    `json:"clusterUid"`
	LastSeen   time.Time `json:"lastSeen"`
	Stale      *bool     `json:"stale"`
}

// clusterAdmin is what the subcommands need, from a server's API or
// straight from its database.
type clusterAdmin interface {
	list(ctx context.Context) ([]clusterRow, error)
	delete(ctx context.Context, name string) error
	rename(ctx context.Context, name, newName string) error
	close() error
}

// errNoCluster is a lookup of a name nobody registered.
type errNoCluster string

func (e errNoCluster) Error() string { return fmt.Sprintf("no cluster %q", string(e)) }

// clusterTarget is the shared --server/--db selection plus the token the
// subcommand needs in server mode.
type clusterTarget struct {
	db        dbFlags
	server    string
	token     string
	tokenFlag *secretFlag
}

// register adds the flags; tokenName/tokenEnv name the token flag
// ("read-token"/"admin-token").
func (c *clusterTarget) register(cmd *cobra.Command, tokenName, tokenEnv, tokenUsage string) {
	c.db.register(cmd)
	cmd.Flags().StringVar(&c.server, "server", "", "base URL of a running upgradescope server, e.g. https://upgradescope.example.com (instead of --db/--db-url)")
	c.tokenFlag = addSecretFlag(cmd, &c.token, tokenName, tokenEnv, tokenUsage)
	for _, f := range []string{"db", "db-url", "db-url-file"} {
		cmd.MarkFlagsMutuallyExclusive("server", f)
	}
}

// open returns the server or database client the flags select. There is
// no default: --db's default path would open (and create) an empty
// ./upgradescope.db, and list would print an empty fleet as if it were the
// real one.
func (c *clusterTarget) open(cmd *cobra.Command) (clusterAdmin, error) {
	if c.server == "" {
		if err := c.db.resolve(cmd); err != nil {
			return nil, err
		}
		if !cmd.Flags().Changed("db") && c.db.dbURL == "" {
			return nil, errors.New("name the fleet: --server URL for a running server, or its database with --db PATH, --db-url, --db-url-file or $UPGRADESCOPE_DB_URL")
		}
		st, err := c.db.openStore()
		if err != nil {
			return nil, err
		}
		return storeAdmin{st}, nil
	}
	if err := c.tokenFlag.resolve(cmd); err != nil {
		return nil, err
	}
	u, err := url.Parse(c.server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("--server %q: want an http(s) URL such as https://upgradescope.example.com", c.server)
	}
	return &serverAdmin{base: strings.TrimSuffix(c.server, "/"), token: c.token, client: &http.Client{Timeout: clusterAdminTimeout}}, nil
}

func newClustersListCmd() *cobra.Command {
	var target clusterTarget
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List clusters: id, name, UID, last push and staleness",
		Long: "List the clusters a server (or its database) knows: id, name, cluster UID, the last push\n" +
			"and, when asked through a server, whether the cluster is stale (no push within --stale-after).",
		Example: "  upgradescope clusters list --server https://upgradescope.example.com\n" +
			"  upgradescope clusters list --db upgradescope.db",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			admin, err := target.open(cmd)
			if err != nil {
				return err
			}
			defer admin.close() //nolint:errcheck // read-only
			rows, err := admin.list(cmd.Context())
			if err != nil {
				return fmt.Errorf("list clusters: %w", err)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tCLUSTER UID\tLAST SEEN\tSTALE")
			for _, r := range rows {
				uid, stale := r.ClusterUID, "-"
				if uid == "" {
					uid = "-"
				}
				if r.Stale != nil {
					stale = fmt.Sprint(*r.Stale)
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", r.ID, r.Name, uid, r.LastSeen.UTC().Format(time.RFC3339), stale)
			}
			return tw.Flush()
		},
	}
	target.register(cmd, "read-token", "UPGRADESCOPE_READ_TOKEN",
		"with --server: the server's read token (or its admin token); omit for an open read API")
	return cmd
}

func newClustersDeleteCmd() *cobra.Command {
	var target clusterTarget
	cmd := &cobra.Command{
		Use:   "delete <cluster>",
		Short: "Delete a cluster with its history, queued notifications and ingest tokens",
		Long: "Delete a cluster record with its snapshot and evaluation history, its queued\n" +
			"notifications and the per-cluster ingest tokens minted for its name. Use it to\n" +
			"decommission a cluster, or when a cluster was rebuilt: a cluster name is bound to the\n" +
			"cluster UID it first pushed, and the server answers 409 to a push from another UID.\n" +
			"Delete the old record and the next push registers the new one; an agent that used a\n" +
			"per-cluster token needs a new one ('upgradescope tokens create').\n" +
			"An agent that keeps pushing under the name registers it again.",
		Example: "  upgradescope clusters delete prod-eu --server https://upgradescope.example.com\n" +
			"  upgradescope clusters delete prod-eu --db upgradescope.db",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster := args[0]
			admin, err := target.open(cmd)
			if err != nil {
				return err
			}
			defer admin.close() //nolint:errcheck // the delete has committed or failed
			if err := admin.delete(cmd.Context(), cluster); err != nil {
				var none errNoCluster
				if errors.As(err, &none) {
					return none
				}
				return fmt.Errorf("delete cluster: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted cluster %q and its history\n", cluster)
			return nil
		},
	}
	target.register(cmd, "admin-token", "UPGRADESCOPE_ADMIN_TOKEN", "with --server: the server's admin token (serve --admin-token)")
	return cmd
}

func newClustersRenameCmd() *cobra.Command {
	var target clusterTarget
	cmd := &cobra.Command{
		Use:   "rename <cluster> <new-name>",
		Short: "Rename a cluster; its history and ingest tokens move with it",
		Long: "Rename a cluster; its history and its per-cluster ingest tokens move to the new name.\n" +
			"The agent sends its own --cluster-name with every push, so change that too (chart value\n" +
			"agent.clusterName). Until then its pushes are refused when it uses a per-cluster token\n" +
			"(now bound to the new name), or register the old name again when it uses the shared one.",
		Example:       "  upgradescope clusters rename prod-eu prod-eu-1 --server https://upgradescope.example.com",
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cluster, newName := args[0], args[1]
			admin, err := target.open(cmd)
			if err != nil {
				return err
			}
			defer admin.close() //nolint:errcheck // the rename has committed or failed
			if err := admin.rename(cmd.Context(), cluster, newName); err != nil {
				var none errNoCluster
				if errors.As(err, &none) {
					return none
				}
				return fmt.Errorf("rename cluster: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "renamed cluster %q to %q\n", cluster, newName)
			return nil
		},
	}
	target.register(cmd, "admin-token", "UPGRADESCOPE_ADMIN_TOKEN", "with --server: the server's admin token (serve --admin-token)")
	return cmd
}

// storeAdmin works on the server's database directly.
type storeAdmin struct{ st store.Store }

func (a storeAdmin) close() error { return a.st.Close() }

func (a storeAdmin) list(ctx context.Context) ([]clusterRow, error) {
	clusters, err := a.st.ListClusters(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]clusterRow, 0, len(clusters))
	for _, c := range clusters {
		rows = append(rows, clusterRow{ID: c.ID, Name: c.Name, ClusterUID: c.ClusterUID, LastSeen: c.LastSeen})
	}
	return rows, nil
}

func (a storeAdmin) delete(ctx context.Context, name string) error {
	err := a.st.DeleteCluster(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return errNoCluster(name)
	}
	return err
}

func (a storeAdmin) rename(ctx context.Context, name, newName string) error {
	err := a.st.RenameCluster(ctx, name, newName)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errNoCluster(name)
	case errors.Is(err, store.ErrClusterNameTaken):
		return fmt.Errorf("cluster name %q is already registered", newName)
	}
	return err
}

// serverAdmin works through a server's API. Names are resolved to ids
// with the cluster list.
type serverAdmin struct {
	base   string
	token  string
	client *http.Client
}

func (a *serverAdmin) close() error { return nil }

// do sends one request and decodes a 2xx JSON body into out (when non-nil);
// any other status is an error carrying the server's message.
func (a *serverAdmin) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("%s %s: reading response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, e.Error)
		}
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s %s: decoding response: %w", method, path, err)
		}
	}
	return nil
}

func (a *serverAdmin) list(ctx context.Context) ([]clusterRow, error) {
	var rows []clusterRow
	if err := a.do(ctx, http.MethodGet, "/api/v1/clusters", nil, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// id resolves a cluster name to its id.
func (a *serverAdmin) id(ctx context.Context, name string) (int64, error) {
	rows, err := a.list(ctx)
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if r.Name == name {
			return r.ID, nil
		}
	}
	return 0, errNoCluster(name)
}

func (a *serverAdmin) delete(ctx context.Context, name string) error {
	id, err := a.id(ctx, name)
	if err != nil {
		return err
	}
	return a.do(ctx, http.MethodDelete, fmt.Sprintf("/api/v1/clusters/%d", id), nil, nil)
}

func (a *serverAdmin) rename(ctx context.Context, name, newName string) error {
	id, err := a.id(ctx, name)
	if err != nil {
		return err
	}
	return a.do(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/clusters/%d", id), map[string]string{"name": newName}, nil)
}
