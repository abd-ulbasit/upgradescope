package cli

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/mcp"
	"github.com/abd-ulbasit/upgradescope/internal/server"
	"github.com/abd-ulbasit/upgradescope/internal/suppress"
)

// maxInventoryFileBytes bounds an inventory file the inventory_file input
// names (the path comes from an assistant): what a default upgradescope
// server takes as an agent's push, so any inventory an agent sends can be
// judged, and nothing much larger. Unlike a report, the inventory itself is
// not sent to the client, only the report it is judged into, whose size
// the tools check (mcp.MaxReportBytes says why that matters).
const maxInventoryFileBytes = server.DefaultMaxSnapshotBytes

type mcpOptions struct {
	kubeconfig     string
	kubecontext    string
	requestTimeout time.Duration

	httpAddr    string
	allowRemote bool
	httpToken   string

	serverURL string
	readToken string
}

func newMCPCmd() *cobra.Command {
	var (
		opts      mcpOptions
		readToken *secretFlag
		httpToken *secretFlag
	)
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve upgrade readiness to AI assistants over MCP (read-only)",
		Long: `Run a Model Context Protocol server so an AI assistant (Claude Code, Claude
Desktop, any MCP client) can ask about upgrade readiness. Every tool is
read-only; none changes a cluster, the server or a file.

Tools: scan (scan the cluster in the kubeconfig for one or more targets, as
'upgradescope scan --output json' does), list_findings and get_report (the
latest scan, a report file written by 'scan --output json', or an inventory
file judged at a target), registry_lookup (add-on end-of-life and
compatibility from the embedded registry) and, in fleet mode, fleet_summary.
Reports and findings follow the published schema, api/report.schema.json, so
they are the JSON the CLI and the REST API write.

The server speaks MCP on stdin and stdout, which an assistant client starts
as a subprocess. With --http ADDR it serves MCP over streamable HTTP at
http://ADDR/mcp instead, on 127.0.0.1 unless ADDR names another host (which
needs --allow-remote). The HTTP endpoint has no authentication unless
--http-token sets a bearer token every request must carry; without one, any
local user or process that can reach the port can run scans with your
kubeconfig.

The cluster a scan reads is the one --kubeconfig and --context name, else
$KUBECONFIG and the kubeconfig's current context, as for 'scan'; without
--context, the current context is read once at start and kept, so switching
contexts later does not move the server to another cluster. Nothing else
chooses it: an assistant names the target versions, never the cluster, and
no ignore file is looked up. With --server-url, get_report and list_findings
can read a cluster from an upgradescope server and fleet_summary summarises
the fleet, using the server's read token (--read-token, --read-token-file
or $UPGRADESCOPE_READ_TOKEN); a server that requires one rejects calls
without it, and the tool shows that error.`,
		Example: `  # What an MCP client starts (see docs/getting-started/mcp.md for its configuration)
  upgradescope mcp

  # The same, on a specific kubeconfig context
  upgradescope mcp --context staging

  # Fleet mode: also answer from an upgradescope server
  UPGRADESCOPE_READ_TOKEN=... upgradescope mcp --server-url https://upgradescope.example.com

  # Streamable HTTP on loopback, with a bearer token
  UPGRADESCOPE_MCP_HTTP_TOKEN=... upgradescope mcp --http 127.0.0.1:8808`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := readToken.resolve(cmd); err != nil {
				return err
			}
			if err := httpToken.resolve(cmd); err != nil {
				return err
			}
			if err := validateMCPOptions(cmd, &opts); err != nil {
				return err
			}
			stderr := cmd.ErrOrStderr()
			if opts.kubecontext == "" {
				// Pin the context: a scan reads the cluster the server
				// started on, whatever the kubeconfig says later.
				if opts.kubecontext = currentKubeContext(opts.kubeconfig); opts.kubecontext != "" {
					fmt.Fprintf(stderr, "upgradescope mcp: scans read kubeconfig context %q (its current context at start; --context names another)\n", opts.kubecontext)
				}
			}
			cfg := mcp.Config{
				Version:   version,
				Scan:      mcpScanner(opts, stderr),
				Inventory: mcpInventory,
			}
			if opts.serverURL != "" {
				fleet, err := mcp.NewFleet(opts.serverURL, opts.readToken)
				if err != nil {
					return err
				}
				if w := mcp.CleartextWarning(opts.serverURL, opts.readToken); w != "" {
					fmt.Fprintf(stderr, "warning: %s\n", esc(w))
				}
				cfg.Fleet = fleet
			}
			srv := mcp.New(cfg)
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if opts.httpAddr != "" {
				return serveMCPHTTP(ctx, srv, opts.httpAddr, opts.httpToken, stderr)
			}
			return serveMCPStdio(ctx, srv, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&opts.kubeconfig, "kubeconfig", "", "path to kubeconfig for scan (default: standard loading rules)")
	cmd.Flags().StringVar(&opts.kubecontext, "context", "", "kubeconfig context for scan (default: the kubeconfig's current context when the server starts)")
	cmd.Flags().DurationVar(&opts.requestTimeout, "request-timeout", defaultRequestTimeout, "give up on a single API request of a scan after this long (0 = no per-request limit)")
	cmd.Flags().StringVar(&opts.httpAddr, "http", "", "serve MCP over streamable HTTP at http://ADDR/mcp instead of stdio; a bare port or :PORT binds 127.0.0.1")
	cmd.Flags().BoolVar(&opts.allowRemote, "allow-remote", false, "with --http, accept an address that is not loopback")
	httpToken = addSecretFlag(cmd, &opts.httpToken, "http-token", "UPGRADESCOPE_MCP_HTTP_TOKEN",
		"with --http: a bearer token every request must carry (Authorization: Bearer TOKEN); without it the endpoint has no authentication")
	cmd.Flags().StringVar(&opts.serverURL, "server-url", "", "fleet mode: base URL of an upgradescope server, e.g. https://upgradescope.example.com")
	readToken = addSecretFlag(cmd, &opts.readToken, "read-token", "UPGRADESCOPE_READ_TOKEN",
		"with --server-url: the server's read token; omit it for an open read API")
	return cmd
}

func validateMCPOptions(cmd *cobra.Command, opts *mcpOptions) error {
	if err := validRequestTimeout(opts.requestTimeout); err != nil {
		return err
	}
	if opts.serverURL == "" && (cmd.Flags().Changed("read-token") || cmd.Flags().Changed("read-token-file")) {
		return errors.New("--read-token needs --server-url")
	}
	if opts.httpAddr == "" {
		if opts.allowRemote {
			return errors.New("--allow-remote needs --http")
		}
		if cmd.Flags().Changed("http-token") || cmd.Flags().Changed("http-token-file") {
			return errors.New("--http-token needs --http")
		}
		opts.httpToken = "" // from the environment, for a stdio server: unused
		return nil
	}
	addr, remote, err := normalizeMCPAddr(opts.httpAddr)
	if err != nil {
		return err
	}
	if remote && !opts.allowRemote {
		return fmt.Errorf("--http %s is not a loopback address, and scan reads your cluster with your kubeconfig: bind 127.0.0.1, or pass --allow-remote (with --http-token, or with the network in front of it as the access control)", opts.httpAddr)
	}
	opts.httpAddr = addr
	return nil
}

// normalizeMCPAddr completes the --http address ("8808" and ":8808" are on
// 127.0.0.1) and reports whether it is reachable from other machines.
func normalizeMCPAddr(s string) (addr string, remote bool, err error) {
	if _, err := strconv.Atoi(s); err == nil {
		s = ":" + s
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", false, fmt.Errorf("invalid --http %q (want HOST:PORT, :PORT or a port): %w", s, err)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 0 || p > 65535 {
		return "", false, fmt.Errorf("invalid --http %q: bad port %q", s, port)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if host == "localhost" {
		return net.JoinHostPort(host, port), false, nil
	}
	ip := net.ParseIP(host)
	return net.JoinHostPort(host, port), ip == nil || !ip.IsLoopback(), nil
}

// currentKubeContext is the kubeconfig's current context, by the loading
// rules a scan uses ($KUBECONFIG, ~/.kube/config, or kubeconfig), or ""
// when there is no kubeconfig to read; a scan then fails with the reason.
// A package var so tests never read the user's kubeconfig.
var currentKubeContext = func(kubeconfig string) string {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	raw, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).RawConfig()
	if err != nil {
		return ""
	}
	return raw.CurrentContext
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// serveMCPStdio speaks MCP on in and out until the client closes its end or
// the context ends. Nothing else may be written to out.
func serveMCPStdio(ctx context.Context, srv *mcpsdk.Server, in io.Reader, out io.Writer) error {
	rc, ok := in.(io.ReadCloser)
	if !ok {
		rc = io.NopCloser(in)
	}
	err := srv.Run(ctx, &mcpsdk.IOTransport{Reader: rc, Writer: nopWriteCloser{out}})
	if ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil // a client that hangs up, or ctrl-C, is not a failure
	}
	return err
}

// serveMCPHTTP serves MCP over streamable HTTP at /mcp until ctx ends.
// Cross-origin browser requests are refused here; the SDK refuses a
// request whose Host header is not loopback when it arrives on a loopback
// address (DNS rebinding), which covers the default binding but not an
// --allow-remote one. With token, every request must carry it as a bearer
// token.
func serveMCPHTTP(ctx context.Context, srv *mcpsdk.Server, addr, token string, stderr io.Writer) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mcp: listen: %w", err)
	}
	var h http.Handler = http.NewCrossOriginProtection().Handler(
		mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil))
	auth := "no authentication: any local user or process that reaches it can run scans with your kubeconfig"
	if token != "" {
		h = requireBearer(token, h)
		auth = "bearer token required"
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", h)
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(stderr, "upgradescope mcp: serving MCP on http://%s/mcp (%s)\n", ln.Addr(), auth)
	errCh := make(chan error, 1)
	go func() { errCh <- hs.Serve(ln) }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := hs.Shutdown(shCtx); err != nil {
			_ = hs.Close()
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// requireBearer passes on only requests that carry token as a bearer
// token, compared in constant time.
func requireBearer(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="upgradescope mcp"`)
			http.Error(w, "missing or invalid bearer token (start the client with the token of 'upgradescope mcp --http-token')", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mcpScanner is the scan tool: `upgradescope scan --output json` for the
// call's targets, through the same pieces the command uses (option
// validation, the cluster read, the evaluation, the JSON writer), so an
// assistant gets the report a person at the terminal gets. The cluster is
// read once per call and judged at each target. The cluster read is what
// base carries, --kubeconfig and --context (pinned at start when it was not
// given), and what clientcmd takes from the environment; a call names
// targets and nothing else, and nothing is defaulted beyond that. In
// particular no ignore file is discovered from the working directory,
// which an MCP client chooses; the suppressions that live in the cluster
// (the upgradescope.dev/ignore annotations) apply, as in a scan with no
// ignore file. --request-timeout is the one scan setting besides: it
// bounds a request, it does not choose what is read.
func mcpScanner(base mcpOptions, stderr io.Writer) func(context.Context, mcp.ScanRequest) ([]json.RawMessage, error) {
	return func(ctx context.Context, req mcp.ScanRequest) ([]json.RawMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(req.Targets) == 0 {
			return nil, errors.New("no targets")
		}
		all := make([]scanOptions, 0, len(req.Targets))
		for _, t := range req.Targets {
			opts := scanOptions{
				target:         t,
				kubeconfig:     base.kubeconfig,
				kubecontext:    base.kubecontext,
				requestTimeout: base.requestTimeout,
				output:         "json",
				failOn:         "never",
				stderr:         stderr,
			}
			if err := validateScanOptions(&opts); err != nil {
				return nil, err
			}
			all = append(all, opts)
		}
		reports, err := runMCPScan(ctx, all)
		if err != nil {
			return nil, err
		}
		if len(reports) != len(all) {
			return nil, fmt.Errorf("%d reports for %d targets", len(reports), len(all))
		}
		now := time.Now()
		docs := make([]json.RawMessage, len(reports))
		for i, r := range reports {
			r, warnings := suppress.Apply(r, nil, suppress.Options{Now: now})
			if i == 0 { // the same objects at every target: warn once
				for _, w := range warnings {
					fmt.Fprintf(stderr, "warning: %s\n", esc(w))
				}
			}
			if docs[i], err = reportDocument(r, nil); err != nil {
				return nil, err
			}
		}
		return docs, nil
	}
}

// runMCPScan reads the live cluster opts[0] names once and judges it at
// each opts' target, one report each, in order; every opts names the same
// cluster. It stops when ctx ends: a call the client cancelled reads no
// further and reports nothing. A package var so tests can stand in for the
// cluster, as runScan is for the scan command.
var runMCPScan = func(ctx context.Context, opts []scanOptions) ([]engine.Report, error) {
	kbData, err := kb.Load()
	if err != nil {
		return nil, fmt.Errorf("load knowledge base: %w", err)
	}
	inv, cluster, err := collectCluster(ctx, kbData, opts[0])
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err // the inventory is partial: judging it would mislead
	}
	now := time.Now()
	reports := make([]engine.Report, len(opts))
	for i, o := range opts {
		r := evaluateScan(inv, kbData, o, now)
		r.KubeContext, r.APIServer = cluster.context, cluster.server
		reports[i] = r
	}
	return reports, nil
}

// mcpInventory judges an inventory file (the JSON an agent pushes to a
// server) at target, as the server would for a what-if target: what the
// server refuses at ingest (inventory.Admit) is refused, and the free text
// it cuts is cut, so a file is judged here exactly when a default server
// would judge it pushed. A refusal quotes nothing of the file
// (inventoryRefusal).
func mcpInventory(ctx context.Context, path, target string) (json.RawMessage, error) {
	tv, err := inventory.ParseTarget(target)
	if err != nil {
		return nil, fmt.Errorf("invalid target %q: %w", target, err)
	}
	raw, err := mcp.ReadFile(ctx, path, maxInventoryFileBytes)
	if err != nil {
		return nil, err
	}
	notInventory := fmt.Errorf("%s is not an upgradescope inventory (want schemaVersion 1 with clusterId and capabilities, as an agent pushes); for a report use report_file", path)
	var inv inventory.Inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		// encoding/json's reason quotes the file, which may be any file.
		return nil, notInventory
	}
	// A report or any other JSON would decode into an empty inventory and
	// score 100; an inventory names its schema, cluster and capabilities.
	if inv.SchemaVersion != inventory.SupportedSchemaVersion || inv.ClusterID == "" || len(inv.Capabilities) == 0 {
		return nil, notInventory
	}
	if err := inv.Admit(); err != nil {
		return nil, inventoryRefusal(path, err)
	}
	kbData, err := kb.Load()
	if err != nil {
		return nil, fmt.Errorf("load knowledge base: %w", err)
	}
	return reportDocument(engine.Evaluate(inv, kbData, tv, time.Now()), nil)
}

// inventoryRefusal is the tool error for an inventory file that
// inventory.Admit refused. Admit's own messages quote the value at fault
// (map keys included), and the file is whatever an assistant named, so
// this names the field and the rule and nothing of the file.
func inventoryRefusal(path string, err error) error {
	const lead = "%s is not an inventory upgradescope judges (a server refuses it, 422): inventory.%s"
	var (
		ie *inventory.IdentifierError
		le *inventory.LimitError
		ve *inventory.ServerVersionError
		ce *inventory.CollectorSchemaError
	)
	switch {
	case errors.As(err, &ie):
		return fmt.Errorf(lead, path, ie.Unquoted())
	case errors.As(err, &le):
		return fmt.Errorf(lead, path, le.Unquoted())
	case errors.As(err, &ve):
		return fmt.Errorf(lead, path, "serverVersion is not a Kubernetes 1.x version")
	case errors.As(err, &ce):
		// An integer, so safe to quote; schemaVersion was checked before Admit.
		return fmt.Errorf(lead, path, fmt.Sprintf("collectorSchema %d is not one this build knows (it judges %d, or none from a collector that predates it)", ce.Got, inventory.CurrentCollectorSchema))
	default:
		return fmt.Errorf("%s is not an upgradescope inventory of schemaVersion %d", path, inventory.SupportedSchemaVersion)
	}
}

// reportDocument renders r as `scan --output json` writes it.
func reportDocument(r engine.Report, filesBase *string) (json.RawMessage, error) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, r, filesBase); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}
