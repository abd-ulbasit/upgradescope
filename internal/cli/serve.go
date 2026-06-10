package cli

import (
	"context"
	"fmt"
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

	// parsedTargets is opts.targets parsed once by validateServeOptions;
	// runServe consumes it instead of re-parsing the raw CSV.
	parsedTargets []inventory.Version
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
		// In-flight ingests finish their synchronous notification fan-out
		// during this drain. That delivery is bounded — 2s client timeout
		// per notifier — and runs on the request's WithoutCancel evaluation
		// context, so it neither holds the drain past shutdownTimeout nor
		// gets cut off mid-delivery. Acceptable at current scale; an async
		// delivery queue is the upgrade path if notifier counts grow.
		shCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return <-errCh // nil after a clean Shutdown
	}
}

func newServeCmd() *cobra.Command {
	var opts serveOptions
	cmd := &cobra.Command{
		Use:           "serve",
		Short:         "Run the upgradescope server: snapshot ingest, REST API, history, notifications",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
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

	cmd.Flags().StringVar(&opts.listen, "listen", ":8080", "address to listen on")
	cmd.Flags().StringVar(&opts.db, "db", "upgradescope.db", "path to the SQLite database (parent directory is created)")
	cmd.Flags().StringVar(&opts.dbURL, "db-url", "", "Postgres URL (postgres://user:pass@host:5432/db); mutually exclusive with --db")
	cmd.MarkFlagsMutuallyExclusive("db", "db-url")
	cmd.Flags().StringVar(&opts.ingestToken, "ingest-token", "", "shared bearer token agents present to push snapshots (required; per-cluster tokens via 'upgradescope tokens create' are also accepted on ingest)")
	cmd.Flags().StringVar(&opts.readToken, "read-token", "", "bearer token for the read API (empty = OPEN read access)")
	cmd.Flags().StringVar(&opts.slackWebhook, "slack-webhook", "", "Slack incoming-webhook URL for delta notifications")
	cmd.Flags().StringVar(&opts.webhook, "webhook", "", "generic webhook URL (POSTed the raw event JSON)")
	cmd.Flags().StringVar(&opts.targets, "targets", "", "extra target versions evaluated on every snapshot, CSV, e.g. 1.37,1.38")
	_ = cmd.MarkFlagRequired("ingest-token")

	return cmd
}

// validateServeOptions parses --targets once into opts.parsedTargets
// (single parse site — runServe never sees the raw CSV).
func validateServeOptions(opts *serveOptions) error {
	if opts.targets == "" {
		return nil
	}
	for _, raw := range strings.Split(opts.targets, ",") {
		v, err := inventory.ParseVersion(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid --targets entry %q: %w", raw, err)
		}
		opts.parsedTargets = append(opts.parsedTargets, v)
	}
	return nil
}
