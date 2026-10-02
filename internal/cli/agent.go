package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abd-ulbasit/upgradescope/internal/agent"
	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

type agentOptions struct {
	interval       time.Duration
	serverURL      string
	serverToken    string
	clusterName    string
	crName         string
	teamLabel      string
	forceSyncEvery time.Duration
	kubeconfig     string
	kubecontext    string
	requestTimeout time.Duration
	targets        []string
	manageCRD      bool
	healthAddr     string
	logFormat      string
	logLevel       string
}

// newAgentLogger builds the agent's slog logger: format text (logfmt) or
// json, level debug|info|warn|error.
func newAgentLogger(w io.Writer, format, level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("--log-level %q: want debug, info, warn or error", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("--log-format %q: want text or json", format)
	}
}

// runAgent is the real I/O pipeline behind `upgradescope agent`. A package
// var so command tests can stub it (same pattern as runScan).
var runAgent = func(ctx context.Context, opts agentOptions) error {
	logger, err := newAgentLogger(os.Stderr, opts.logFormat, opts.logLevel)
	if err != nil {
		return err
	}
	// Code that logs through slog's package-level functions shares the
	// format and level.
	slog.SetDefault(logger)
	if msg := agent.CleartextPushWarning(opts.serverURL, opts.serverToken); msg != "" {
		logger.Warn(msg)
	}
	if limit, ok := applyMemoryLimit(os.Getenv, cgroupRoot); ok {
		logger.Info("GOMEMLIMIT unset: Go memory limit set to 90% of the cgroup's memory limit", "bytes", limit)
	}
	kbData, err := kb.Load()
	if err != nil {
		return fmt.Errorf("load knowledge base: %w", err)
	}
	cfg, err := buildAgentRESTConfig(opts.kubeconfig, opts.kubecontext, opts.requestTimeout)
	if err != nil {
		return err
	}
	clients, err := collect.NewClients(cfg)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build dynamic client: %w", err)
	}
	apiext, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build apiextensions client: %w", err)
	}
	agent.AgentVersion = version
	return agent.Run(ctx, clients, dyn, apiext, kbData, agent.Config{
		Interval:          opts.interval,
		ServerURL:         opts.serverURL,
		ServerToken:       opts.serverToken,
		ClusterName:       opts.clusterName,
		CRName:            opts.crName,
		TeamLabel:         opts.teamLabel,
		ForceSyncEvery:    opts.forceSyncEvery,
		Targets:           opts.targets,
		SkipCRDManagement: !opts.manageCRD,
		HealthAddr:        opts.healthAddr,
		Logger:            logger,
	})
}

// buildAgentRESTConfig prefers in-cluster config (the agent's normal home)
// and falls back to kubeconfig loading rules — the same rules as scan. An
// explicit --kubeconfig or --context skips the in-cluster attempt entirely.
// requestTimeout becomes rest.Config.Timeout (see scanRESTConfig).
var buildAgentRESTConfig = func(kubeconfig, kubecontext string, requestTimeout time.Duration) (*rest.Config, error) {
	if kubeconfig == "" && kubecontext == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			cfg.Timeout = requestTimeout
			return cfg, nil
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubecontext}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig (not in-cluster, no kubeconfig found): %w", err)
	}
	cfg.Timeout = requestTimeout
	return cfg, nil
}

func newAgentCmd() *cobra.Command {
	var (
		opts        agentOptions
		serverToken *secretFlag
	)
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the in-cluster continuous upgrade-readiness agent",
		Long: `Continuously collect the cluster's inventory, evaluate upgrade readiness
every --interval, write the result to the status of the ClusterReadiness
object, and (with --server-url) push snapshots to an upgradescope server.

The Helm chart (deploy/chart) runs it in the cluster with read-only RBAC.`,
		Example: `  # In the cluster, CRD status only (what the Helm chart runs)
  upgradescope agent

  # Also push snapshots to a fleet server
  upgradescope agent --server-url https://upgradescope.example.com \
    --server-token-file /var/run/secrets/upgradescope/token

  # Try it from a laptop against a kubeconfig context
  upgradescope agent --context kind-dev --interval 1m --cr-name dev`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := serverToken.resolve(cmd); err != nil {
				return err
			}
			if err := validRequestTimeout(opts.requestTimeout); err != nil {
				return err
			}
			if _, err := newAgentLogger(io.Discard, opts.logFormat, opts.logLevel); err != nil {
				return err // a typo fails before any cluster access
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runAgent(ctx, opts)
		},
	}
	cmd.Flags().DurationVar(&opts.interval, "interval", 10*time.Minute, "evaluation interval (minimum 1m)")
	cmd.Flags().StringVar(&opts.serverURL, "server-url", "", "upgradescope server base URL (empty = CRD-only mode)")
	serverToken = addSecretFlag(cmd, &opts.serverToken, "server-token", "UPGRADESCOPE_SERVER_TOKEN",
		"bearer token for snapshot pushes (required with --server-url)")
	cmd.Flags().StringVar(&opts.clusterName, "cluster-name", "", "cluster label sent to the server (default: cluster UID)")
	cmd.Flags().StringVar(&opts.crName, "cr-name", "cluster", "ClusterReadiness object name")
	cmd.Flags().StringVar(&opts.teamLabel, "team-label", "team", "namespace label used for team attribution")
	cmd.Flags().DurationVar(&opts.forceSyncEvery, "force-sync-every", time.Hour, "push a snapshot even if unchanged after this long")
	cmd.Flags().StringSliceVar(&opts.targets, "targets", nil,
		"target minors, CSV, e.g. 1.37,1.38; when set, the ClusterReadiness spec.targets is reconciled to them every tick (overriding kubectl edits)")
	cmd.Flags().BoolVar(&opts.manageCRD, "manage-crd", true,
		"keep the ClusterReadiness CRD schema in step with this binary at startup (needs get/patch on that CRD); false = never touch the CRD")
	cmd.Flags().StringVar(&opts.healthAddr, "health-addr", ":8081",
		"listen address for /healthz, /readyz and /metrics (empty = disabled)")
	cmd.Flags().StringVar(&opts.logFormat, "log-format", "text", "log format: text (logfmt) or json")
	cmd.Flags().StringVar(&opts.logLevel, "log-level", "info", "log level: debug, info, warn or error")
	cmd.Flags().StringVar(&opts.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: in-cluster config, then standard loading rules)")
	cmd.Flags().StringVar(&opts.kubecontext, "context", "", "kubeconfig context to use")
	cmd.Flags().DurationVar(&opts.requestTimeout, "request-timeout", defaultRequestTimeout, "give up on a single API request after this long (0 = no per-request limit)")
	return cmd
}
