package cli

import (
	"bytes"
	"context"
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

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/mcp"
	"github.com/abd-ulbasit/upgradescope/internal/suppress"
)

// maxInventoryFileBytes bounds an inventory file the inventory_file input
// names: the path comes from an assistant.
const maxInventoryFileBytes = 64 << 20

type mcpOptions struct {
	kubeconfig     string
	kubecontext    string
	requestTimeout time.Duration

	httpAddr    string
	allowRemote bool

	serverURL string
	readToken string
}

func newMCPCmd() *cobra.Command {
	var (
		opts      mcpOptions
		readToken *secretFlag
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
needs --allow-remote: the HTTP endpoint has no authentication, and scan reads
your cluster with your kubeconfig).

The cluster a scan reads is the one --kubeconfig and --context name, else
$KUBECONFIG and the kubeconfig's current context, as for 'scan', and nothing
else: an assistant names the target versions, never the cluster, and no
ignore file is looked up. With --server-url, get_report and list_findings can
read a cluster from an upgradescope server and fleet_summary summarises the
fleet, using the server's read token (--read-token, --read-token-file or
$UPGRADESCOPE_READ_TOKEN); a server that requires one rejects calls without
it, and the tool shows that error.`,
		Example: `  # What an MCP client starts (see docs/getting-started/mcp.md for its configuration)
  upgradescope mcp

  # The same, on a specific kubeconfig context
  upgradescope mcp --context staging

  # Fleet mode: also answer from an upgradescope server
  UPGRADESCOPE_READ_TOKEN=... upgradescope mcp --server-url https://upgradescope.example.com

  # Streamable HTTP on loopback
  upgradescope mcp --http 127.0.0.1:8808`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := readToken.resolve(cmd); err != nil {
				return err
			}
			if err := validateMCPOptions(cmd, &opts); err != nil {
				return err
			}
			cfg := mcp.Config{
				Version:   version,
				Scan:      mcpScanner(opts, cmd.ErrOrStderr()),
				Inventory: mcpInventory,
			}
			if opts.serverURL != "" {
				fleet, err := mcp.NewFleet(opts.serverURL, opts.readToken)
				if err != nil {
					return err
				}
				cfg.Fleet = fleet
			}
			srv := mcp.New(cfg)
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if opts.httpAddr != "" {
				return serveMCPHTTP(ctx, srv, opts.httpAddr, cmd.ErrOrStderr())
			}
			return serveMCPStdio(ctx, srv, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&opts.kubeconfig, "kubeconfig", "", "path to kubeconfig for scan (default: standard loading rules)")
	cmd.Flags().StringVar(&opts.kubecontext, "context", "", "kubeconfig context for scan (default: the kubeconfig's current context)")
	cmd.Flags().DurationVar(&opts.requestTimeout, "request-timeout", defaultRequestTimeout, "give up on a single API request of a scan after this long (0 = no per-request limit)")
	cmd.Flags().StringVar(&opts.httpAddr, "http", "", "serve MCP over streamable HTTP at http://ADDR/mcp instead of stdio; a bare port or :PORT binds 127.0.0.1")
	cmd.Flags().BoolVar(&opts.allowRemote, "allow-remote", false, "with --http, accept an address that is not loopback; the endpoint has no authentication")
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
		return nil
	}
	addr, remote, err := normalizeMCPAddr(opts.httpAddr)
	if err != nil {
		return err
	}
	if remote && !opts.allowRemote {
		return fmt.Errorf("--http %s is not a loopback address: the endpoint has no authentication and scan reads your cluster with your kubeconfig; bind 127.0.0.1, or pass --allow-remote if the network in front of it is the access control", opts.httpAddr)
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

// serveMCPHTTP serves MCP over streamable HTTP at /mcp until ctx ends. The
// SDK refuses requests whose Host header is not loopback when the listener is
// (DNS rebinding), and cross-origin browser requests are refused here.
func serveMCPHTTP(ctx context.Context, srv *mcpsdk.Server, addr string, stderr io.Writer) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mcp: listen: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.NewCrossOriginProtection().Handler(
		mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)))
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(stderr, "upgradescope mcp: serving MCP on http://%s/mcp\n", ln.Addr())
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

// mcpScanner is the scan tool: `upgradescope scan --output json` for one
// target, through the same pieces the command uses (option validation,
// runScan, the JSON writer), so an assistant gets the report a person at the
// terminal gets. The cluster read is what base carries, --kubeconfig and
// --context, and what clientcmd takes from the environment ($KUBECONFIG, the
// kubeconfig's current context); a call names a target and nothing else, and
// nothing is defaulted beyond that. In particular no ignore file is
// discovered from the working directory, which an MCP client chooses; the
// suppressions that live in the cluster (the upgradescope.dev/ignore
// annotations) apply, as in a scan with no ignore file. --request-timeout
// is the one scan setting besides: it bounds a request, it does not choose
// what is read.
func mcpScanner(base mcpOptions, stderr io.Writer) func(context.Context, mcp.ScanRequest) (json.RawMessage, error) {
	return func(ctx context.Context, req mcp.ScanRequest) (json.RawMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		opts := scanOptions{
			target:         req.Target,
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
		report, err := runScan(opts)
		if err != nil {
			return nil, err
		}
		report, warnings := suppress.Apply(report, nil, suppress.Options{Now: time.Now()})
		for _, w := range warnings {
			fmt.Fprintf(stderr, "warning: %s\n", w)
		}
		return reportDocument(report, nil)
	}
}

// mcpInventory judges an inventory file (the JSON an agent pushes to a
// server) at target, as the server would for a what-if target.
func mcpInventory(path, target string) (json.RawMessage, error) {
	tv, err := inventory.ParseTarget(target)
	if err != nil {
		return nil, fmt.Errorf("invalid target %q: %w", target, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxInventoryFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxInventoryFileBytes {
		return nil, fmt.Errorf("%s is larger than %d MiB", path, maxInventoryFileBytes>>20)
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		return nil, fmt.Errorf("%s is not an inventory: %w", path, err)
	}
	// A report or any other JSON would decode into an empty inventory and
	// score 100; an inventory names its schema, cluster and capabilities.
	if inv.SchemaVersion != 1 || inv.ClusterID == "" || len(inv.Capabilities) == 0 {
		return nil, fmt.Errorf("%s is not an upgradescope inventory (want schemaVersion 1 with clusterId and capabilities, as an agent pushes); for a report use report_file", path)
	}
	if err := inv.ValidateLimits(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	kbData, err := kb.Load()
	if err != nil {
		return nil, fmt.Errorf("load knowledge base: %w", err)
	}
	return reportDocument(engine.Evaluate(inv, kbData, tv, time.Now()), nil)
}

// reportDocument renders r as `scan --output json` writes it.
func reportDocument(r engine.Report, filesBase *string) (json.RawMessage, error) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, r, filesBase); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}
