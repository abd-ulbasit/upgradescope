package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
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
	adminToken   string
	slackWebhook string
	webhook      string
	webhookKey   string
	targets      string
	teamMap      string

	maxSnapshotBytes   int64
	maxGateBytes       int64
	allowAnonymousRead bool
	tlsCertFile        string
	tlsKeyFile         string
	staleAfter         time.Duration
	retention          string

	// parsedRetention is --retention parsed by validateServeOptions.
	parsedRetention time.Duration

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
	if limit, ok := applyMemoryLimit(os.Getenv, cgroupRoot); ok {
		log.Printf("serve: GOMEMLIMIT unset: Go memory limit set to %d bytes, 90%% of the cgroup's memory limit", limit)
	}
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
		hook := notify.NewGenericWebhook(opts.webhook)
		hook.Secret = opts.webhookKey
		notifiers = append(notifiers, hook)
	}

	extraTargets := make([]string, 0, len(opts.parsedTargets))
	for _, v := range opts.parsedTargets {
		extraTargets = append(extraTargets, v.String())
	}

	srv, err := server.New(server.Config{
		Listen:             opts.listen,
		Store:              st,
		KB:                 kbData,
		Notifier:           notify.Multi(notifiers...), // zero notifiers → harmless no-op
		IngestToken:        opts.ingestToken,
		ReadToken:          opts.readToken,
		AdminToken:         opts.adminToken,
		ExtraTargets:       extraTargets,
		TeamMap:            opts.parsedTeamMap,
		AllowAnonymousRead: opts.allowAnonymousRead,
		Version:            version,

		MaxSnapshotBytes: opts.maxSnapshotBytes,
		MaxGateBytes:     opts.maxGateBytes,
		TLSCertFile:      opts.tlsCertFile,
		TLSKeyFile:       opts.tlsKeyFile,
		StaleAfter:       opts.staleAfter,
		Retention:        opts.parsedRetention,
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
		// in the outbox and is delivered after the next start, if that start
		// comes before the message has been queued for 8 hours.
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
		Use:   "serve",
		Short: "Run the upgradescope server: snapshot ingest, REST API, history, notifications",
		Long: `Run the upgradescope server. It accepts snapshots that agents push,
evaluates them against every target, keeps the history, serves the REST API
and the dashboard at /, and sends Slack or webhook notifications when a
cluster's readiness changes.

It listens on loopback by default. On any other address, the read API needs
--read-token, or an explicit --allow-anonymous-read.`,
		Example: `  # Local dashboard at http://127.0.0.1:8080/, SQLite in ./upgradescope.db
  upgradescope serve

  # Fleet server: all interfaces, Postgres, tokens from mounted Secrets
  upgradescope serve --listen :8080 \
    --db-url-file /secrets/db-url --read-token-file /secrets/read-token \
    --targets 1.37,1.38`,
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
		addSecretFlag(cmd, &opts.adminToken, "admin-token", "UPGRADESCOPE_ADMIN_TOKEN",
			"bearer token for cluster administration: DELETE and PATCH (rename) /api/v1/clusters/{id}, 'upgradescope clusters delete|rename --server'; it also reads (empty = administration refused)"),
		addSecretFlag(cmd, &opts.slackWebhook, "slack-webhook", "UPGRADESCOPE_SLACK_WEBHOOK",
			"Slack incoming-webhook URL for delta notifications"),
		addSecretFlag(cmd, &opts.webhook, "webhook", "UPGRADESCOPE_WEBHOOK_URL",
			"generic webhook URL: POSTed one versioned JSON notification per cluster and evaluation pass (schema in api/webhook.schema.json)"),
		addSecretFlag(cmd, &opts.webhookKey, "webhook-secret", "UPGRADESCOPE_WEBHOOK_SECRET",
			"sign generic webhook requests: X-Upgradescope-Signature: sha256=<hex HMAC-SHA256 of the body with this key>"),
	}
	cmd.Flags().BoolVar(&opts.allowAnonymousRead, "allow-anonymous-read", false, "serve the read API and /api/v1/gate without a read token on a non-loopback --listen address")
	cmd.Flags().StringVar(&opts.targets, "targets", "", "extra target versions evaluated on every snapshot, CSV, e.g. 1.37,1.38")
	cmd.Flags().StringVar(&opts.teamMap, "team-map", "", "YAML file of {pattern, team} namespace globs overriding team labels (first match wins)")
	cmd.Flags().Int64Var(&opts.maxSnapshotBytes, "max-snapshot-bytes", server.DefaultMaxSnapshotBytes, "largest accepted snapshot push body, in bytes (also applied after gzip decompression); "+
		"the body must arrive within the 60s read timeout (~350 KiB/s at the 20 MiB default) or the push gets 408, and a push that decodes to too many JSON values gets 413 whatever its size; "+
		"it also caps every report the server evaluates, stores or exports (a push whose report would be larger gets 413)")
	cmd.Flags().Int64Var(&opts.maxGateBytes, "max-gate-bytes", server.DefaultMaxGateBytes, "largest accepted /api/v1/gate manifest stream, in bytes; "+
		"the body must arrive within the 60s read timeout or the request gets 408, and a stream of too many YAML nodes gets 413 whatever its size "+
		"(the 400k-node budget is about 4.4 MiB of typical kubectl YAML, so it, not this cap, limits a realistic stream)")
	cmd.Flags().StringVar(&opts.tlsCertFile, "tls-cert-file", "", "PEM certificate (chain) to serve HTTPS directly; requires --tls-key-file (read at startup)")
	cmd.Flags().StringVar(&opts.tlsKeyFile, "tls-key-file", "", "PEM private key for --tls-cert-file")
	cmd.Flags().DurationVar(&opts.staleAfter, "stale-after", server.DefaultStaleAfter, "mark a cluster stale (API, dashboard data, /metrics) when its agent has not pushed for this long; agents push at least about every 70m by default")
	cmd.Flags().StringVar(&opts.retention, "retention", "90d", "prune snapshots and evaluations older than this, in days (90d) or a Go duration (2160h), at startup and daily; each cluster's latest snapshot and its evaluations are always kept; 0 keeps everything")
	cmd.MarkFlagsRequiredTogether("tls-cert-file", "tls-key-file")
	return cmd
}

// cgroupRoot is where the container's own cgroup is mounted.
const cgroupRoot = "/sys/fs/cgroup"

// applyMemoryLimit sets the Go runtime's soft memory limit to 90% of the
// cgroup's memory limit, unless GOMEMLIMIT is set (the runtime has applied
// that already) or there is no limit. The runtime does not read the
// container's limit itself: without one, the collector lets garbage grow
// to as much as the live heap again, and a process whose live heap fits
// is OOM-killed anyway. The chart sets GOMEMLIMIT; this covers docker run
// -m and other cgroups. It returns the limit it set.
func applyMemoryLimit(getenv func(string) string, root string) (int64, bool) {
	if getenv("GOMEMLIMIT") != "" {
		return 0, false
	}
	limit, ok := cgroupMemoryLimit(root)
	if !ok {
		return 0, false
	}
	soft := limit * 9 / 10
	debug.SetMemoryLimit(soft)
	return soft, true
}

// cgroupMemoryLimit reads the memory limit of the cgroup mounted at root:
// memory.max (cgroup v2, "max" = none) or memory/memory.limit_in_bytes
// (v1, which reports "none" as a value near 2^63).
func cgroupMemoryLimit(root string) (int64, bool) {
	for _, f := range []string{"memory.max", "memory/memory.limit_in_bytes"} {
		raw, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || n <= 0 || n >= 1<<60 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// exposedListen reports whether a --listen address is certainly not
// loopback: an empty host (":8080", every interface) or an IP outside
// 127.0.0.0/8 and ::1. That is refused here, with the flags to fix it. A
// hostname, "localhost" included, is decided by the server on the address
// it actually binds (server.Config.AllowAnonymousRead), so neither a
// literal match nor /etc/hosts can open the read API by accident.
func exposedListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && !ip.IsLoopback()
}

// validateServeOptions parses --targets and loads --team-map once into
// opts.parsedTargets/parsedTeamMap (single parse site — runServe never sees
// the raw values).
func validateServeOptions(opts *serveOptions) error {
	if opts.maxSnapshotBytes <= 0 {
		return fmt.Errorf("--max-snapshot-bytes must be positive, got %d", opts.maxSnapshotBytes)
	}
	r, err := parseRetention(opts.retention)
	if err != nil {
		return fmt.Errorf("invalid --retention %q: %w", opts.retention, err)
	}
	opts.parsedRetention = r
	if opts.staleAfter <= 0 {
		return fmt.Errorf("--stale-after must be positive, got %s", opts.staleAfter)
	}
	if opts.maxGateBytes <= 0 {
		return fmt.Errorf("--max-gate-bytes must be positive, got %d", opts.maxGateBytes)
	}
	if opts.webhookKey != "" && opts.webhook == "" {
		return fmt.Errorf("--webhook-secret signs the generic webhook: set --webhook too")
	}
	if opts.adminToken != "" && (opts.adminToken == opts.readToken || opts.adminToken == opts.ingestToken) {
		return fmt.Errorf("--admin-token must differ from --read-token and --ingest-token: " +
			"whoever holds those must not be able to delete or rename clusters")
	}
	if opts.readToken == "" && !opts.allowAnonymousRead && exposedListen(opts.listen) {
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

// minRetention is the shortest non-zero --retention: a window under a day
// would prune the score history the dashboard and exports exist to show.
const minRetention = 24 * time.Hour

// maxRetentionDays (100 years) bounds --retention well below
// time.Duration's ~292 years, so a large "Nd" cannot wrap to a short or
// negative window. The day count is clamped before it is multiplied; 0
// keeps everything.
const (
	maxRetentionDays = 36500
	maxRetention     = maxRetentionDays * 24 * time.Hour
)

// parseRetention parses --retention: whole days ("90d") or a Go duration
// ("2160h"); "0" (or "0d") keeps everything.
func parseRetention(s string) (time.Duration, error) {
	const want = "want whole days such as 90d, a duration such as 2160h, or 0"
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, errors.New(want)
		}
		d = time.Duration(min(n, maxRetentionDays+1)) * 24 * time.Hour // over the cap: refused below
	} else if s != "0" {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, errors.New(want)
		}
	}
	if d > maxRetention {
		return 0, fmt.Errorf("must be at most %dd (use 0 to keep everything)", maxRetentionDays)
	}
	if d != 0 && d < minRetention {
		return 0, fmt.Errorf("must be 0 (keep everything) or at least %s", minRetention)
	}
	return d, nil
}
