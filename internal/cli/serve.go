package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
)

// shutdownTimeout bounds the graceful drain after SIGINT/SIGTERM.
const shutdownTimeout = 10 * time.Second

type serveOptions struct {
	listen       string
	db           string
	dbURL        string
	ingestToken  string
	readToken    string
	slackWebhook string
	webhook      string
	targets      string
	teamMap      string

	maxSnapshotBytes   int64
	maxGateBytes       int64
	allowAnonymousRead bool
	tlsCertFile        string
	tlsKeyFile         string

	// parsedTargets is opts.targets parsed once by validateServeOptions;
	// runServe consumes it instead of re-parsing the raw CSV.
	parsedTargets []inventory.Version
	// parsedTeamMap is opts.teamMap loaded once by validateServeOptions.
	parsedTeamMap server.TeamMap
}

// runServe is the real wiring: openStore (SQLite --db or Postgres
// --db-url) → kb.Load → notify.Multi → server.New → Start (blocks until
// ctx is cancelled, then graceful stop).
// A package var so command tests can stub it, same seam as runScan.
var runServe = func(ctx context.Context, opts serveOptions) error {
	st, err := dbFlags{db: opts.db, dbURL: opts.dbURL}.openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	kbData, err := kb.Load()
	if err != nil {
		return fmt.Errorf("load knowledge base: %w", err)
	}

	var notifiers []notify.Notifier
	if opts.slackWebhook != "" {
		notifiers = append(notifiers, notify.NewSlack(opts.slackWebhook))
	}
	if opts.webhook != "" {
		notifiers = append(notifiers, notify.NewGenericWebhook(opts.webhook))
	}

	extraTargets := make([]string, 0, len(opts.parsedTargets))
	for _, v := range opts.parsedTargets {
		extraTargets = append(extraTargets, v.String())
	}

	srv, err := server.New(server.Config{
		Listen:       opts.listen,
		Store:        st,
		KB:           kbData,
		Notifier:     notify.Multi(notifiers...), // zero notifiers → harmless no-op
		IngestToken:  opts.ingestToken,
		ReadToken:    opts.readToken,
		ExtraTargets: extraTargets,
		TeamMap:      opts.parsedTeamMap,
		Version:      version,

		MaxSnapshotBytes: opts.maxSnapshotBytes,
		MaxGateBytes:     opts.maxGateBytes,
		TLSCertFile:      opts.tlsCertFile,
		TLSKeyFile:       opts.tlsKeyFile,
	})
	if err != nil {
		return err
	}

	// Start blocks until Shutdown; drive the graceful stop from ctx.
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()
	select {
	case err := <-errCh:
		return err // listen/serve failed before any signal
	case <-ctx.Done():
		// In-flight ingests finish during this drain; each commits its
		// snapshot, evaluations and notifications in one transaction and
		// never waits on a notifier. Notifications are delivered after
		// commit by the server's outbox worker, which Shutdown stops once
		// the drain ends: an undelivered or mid-delivery notification stays
		// in the outbox and is delivered after the next start.
		shCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return <-errCh // nil after a clean Shutdown
	}
}

func newServeCmd() *cobra.Command {
	var (
		opts    serveOptions
		db      dbFlags
		secrets []*secretFlag
	)
	cmd := &cobra.Command{
		Use:           "serve",
		Short:         "Run the upgradescope server: snapshot ingest, REST API, history, notifications",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, s := range secrets {
				if err := s.resolve(cmd); err != nil {
					return err
				}
			}
			if err := db.resolve(cmd); err != nil {
				return err
			}
			opts.db, opts.dbURL = db.db, db.dbURL
			if err := validateServeOptions(&opts); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// Restore default signal handling the moment the first signal
			// cancels ctx: runServe then drains for up to shutdownTimeout,
			// and a second SIGINT during that window must force-kill the
			// process instead of being swallowed by the still-registered
			// handler.
			context.AfterFunc(ctx, stop)
			return runServe(ctx, opts)
		},
	}

	cmd.Flags().StringVar(&opts.listen, "listen", "127.0.0.1:8080", "address to listen on (loopback by default; use :8080 for all interfaces)")
	db.register(cmd)
	secrets = []*secretFlag{
		addSecretFlag(cmd, &opts.ingestToken, "ingest-token", "UPGRADESCOPE_INGEST_TOKEN",
			"optional shared bearer token that may push snapshots as ANY cluster; omit it to accept only per-cluster tokens from 'upgradescope tokens create' (serve warns at startup, not later, when both are in use)"),
		addSecretFlag(cmd, &opts.readToken, "read-token", "UPGRADESCOPE_READ_TOKEN",
			"bearer token for the read API and /api/v1/gate (empty = OPEN read access; refused on non-loopback --listen without --allow-anonymous-read)"),
		addSecretFlag(cmd, &opts.slackWebhook, "slack-webhook", "UPGRADESCOPE_SLACK_WEBHOOK",
			"Slack incoming-webhook URL for delta notifications"),
		addSecretFlag(cmd, &opts.webhook, "webhook", "UPGRADESCOPE_WEBHOOK_URL",
			"generic webhook URL (POSTed the raw event JSON)"),
	}
	cmd.Flags().BoolVar(&opts.allowAnonymousRead, "allow-anonymous-read", false, "serve the read API and /api/v1/gate without a read token on a non-loopback --listen address")
	cmd.Flags().StringVar(&opts.targets, "targets", "", "extra target versions evaluated on every snapshot, CSV, e.g. 1.37,1.38")
	cmd.Flags().StringVar(&opts.teamMap, "team-map", "", "YAML file of {pattern, team} namespace globs overriding team labels (first match wins)")
	cmd.Flags().Int64Var(&opts.maxSnapshotBytes, "max-snapshot-bytes", server.DefaultMaxSnapshotBytes, "largest accepted snapshot push body, in bytes (also applied after gzip decompression)")
	cmd.Flags().Int64Var(&opts.maxGateBytes, "max-gate-bytes", server.DefaultMaxGateBytes, "largest accepted /api/v1/gate manifest stream, in bytes")
	cmd.Flags().StringVar(&opts.tlsCertFile, "tls-cert-file", "", "PEM certificate (chain) to serve HTTPS directly; requires --tls-key-file (read at startup)")
	cmd.Flags().StringVar(&opts.tlsKeyFile, "tls-key-file", "", "PEM private key for --tls-cert-file")
	cmd.MarkFlagsRequiredTogether("tls-cert-file", "tls-key-file")
	return cmd
}

// isLoopbackListen reports whether a --listen address binds only loopback:
// "localhost" or a loopback IP. An empty host (":8080"), a wildcard IP and
// any other hostname count as exposed — fail closed rather than resolve.
func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateServeOptions parses --targets and loads --team-map once into
// opts.parsedTargets/parsedTeamMap (single parse site — runServe never sees
// the raw values).
func validateServeOptions(opts *serveOptions) error {
	if opts.maxSnapshotBytes <= 0 {
		return fmt.Errorf("--max-snapshot-bytes must be positive, got %d", opts.maxSnapshotBytes)
	}
	if opts.maxGateBytes <= 0 {
		return fmt.Errorf("--max-gate-bytes must be positive, got %d", opts.maxGateBytes)
	}
	if opts.readToken == "" && !opts.allowAnonymousRead && !isLoopbackListen(opts.listen) {
		return fmt.Errorf("refusing to serve the read API and /api/v1/gate without a token on %q: "+
			"set --read-token, listen on loopback, or pass --allow-anonymous-read to accept open reads", opts.listen)
	}
	if opts.teamMap != "" {
		tm, err := server.LoadTeamMap(opts.teamMap)
		if err != nil {
			return fmt.Errorf("invalid --team-map: %w", err)
		}
		opts.parsedTeamMap = tm
	}
	if opts.targets == "" {
		return nil
	}
	for _, raw := range strings.Split(opts.targets, ",") {
		v, err := inventory.ParseTarget(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid --targets entry %q: %w", raw, err)
		}
		opts.parsedTargets = append(opts.parsedTargets, v)
	}
	return nil
}
