package cli

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/validate/content"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abd-ulbasit/upgradescope/internal/agent"
	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

type agentOptions struct {
	interval       time.Duration
	serverURL      string
	serverToken    string
	serverCAFile   string
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
	registryDir    string // --registry-dir: extra add-on registry entries
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
	var serverRoots *x509.CertPool
	if opts.serverCAFile != "" {
		if serverRoots, err = agent.LoadServerCAs(opts.serverCAFile); err != nil {
			return fmt.Errorf("--server-ca-file: %w", err)
		}
	}
	if limit, ok := applyMemoryLimit(os.Getenv, cgroupRoot); ok {
		logger.Info("GOMEMLIMIT unset: Go memory limit set to 90% of the cgroup's memory limit", "bytes", limit)
	}
	kbData, err := kb.LoadWithRegistry(opts.registryDir)
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
		ServerRootCAs:     serverRoots,
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

// validAgentNames checks, before any cluster access, the names an agent
// sends: --cluster-name, which the server refuses pushes under unless it
// is an RFC 1123 subdomain (empty: the cluster UID, which is one), and
// --cr-name, which the apiserver refuses to create the ClusterReadiness
// object under (a 422 on every tick) unless it is an RFC 1123 subdomain
// (empty: the default, "cluster"), and --team-label, which must be a label
// key to name any label.
func validAgentNames(opts agentOptions) error {
	if opts.clusterName != "" {
		if err := inventory.ValidateClusterName(opts.clusterName); err != nil {
			return fmt.Errorf("invalid --cluster-name: %w", err)
		}
	}
	if opts.crName != "" {
		if p := content.IsDNS1123Subdomain(opts.crName); len(p) > 0 {
			return fmt.Errorf("invalid --cr-name %.64q: not an RFC 1123 subdomain, as an object name must be (%s)", opts.crName, strings.Join(p, "; "))
		}
	}
	if p := content.IsLabelKey(opts.teamLabel); len(p) > 0 {
		return fmt.Errorf("invalid --team-label %q: not a label key (%s)", opts.teamLabel, strings.Join(p, "; "))
	}
	return nil
}

// validPushSettings refuses, before any cluster access, push settings that
// could never work (#238): a --server-url that is not an http or https
// URL with a host, a token with whitespace inside it (surrounding
// whitespace, such as the trailing newline of a Secret made from a file,
// is trimmed from every source first), and a --cluster-name set to "".
func validPushSettings(cmd *cobra.Command, opts *agentOptions) error {
	if cmd.Flags().Changed("cluster-name") && opts.clusterName == "" {
		return fmt.Errorf("invalid --cluster-name: empty; leave the flag out to name the cluster by its UID")
	}
	opts.serverToken = strings.TrimSpace(opts.serverToken)
	if opts.serverURL == "" {
		return nil
	}
	if err := agent.ValidateServerURL(opts.serverURL); err != nil {
		return fmt.Errorf("invalid --server-url: %w", err)
	}
	if err := agent.ValidateServerToken(opts.serverToken); err != nil {
		return fmt.Errorf("invalid --server-token ($UPGRADESCOPE_SERVER_TOKEN, --server-token-file or the flag): %w", err)
	}
	return nil
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
			if err := agent.ValidateInterval(opts.interval); err != nil {
				return err // 0 is below the minimum too, not "the default"
			}
			if err := agent.ValidateForceSyncEvery(opts.forceSyncEvery); err != nil {
				return fmt.Errorf("invalid --force-sync-every: %w", err) // 0 is refused too, not "the default"
			}
			if err := validPushSettings(cmd, &opts); err != nil {
				return err
			}
			if _, err := newAgentLogger(io.Discard, opts.logFormat, opts.logLevel); err != nil {
				return err // a typo fails before any cluster access
			}
			if err := validAgentNames(opts); err != nil {
				return err
			}
			if opts.serverCAFile != "" && opts.serverURL == "" {
				return fmt.Errorf("--server-ca-file needs --server-url: it only verifies the server snapshots are pushed to")
			}
			if opts.serverCAFile != "" && !strings.HasPrefix(strings.ToLower(opts.serverURL), "https://") {
				return fmt.Errorf("--server-ca-file needs an https --server-url: over plain http there is no certificate to verify")
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
	cmd.Flags().StringVar(&opts.serverCAFile, "server-ca-file", "",
		"PEM bundle of CA certificates trusted for an https --server-url, on top of the system roots (a server behind a private CA); read at startup")
	cmd.Flags().StringVar(&opts.clusterName, "cluster-name", "", "cluster label sent to the server, an RFC 1123 subdomain of at most 253 bytes (default: cluster UID)")
	cmd.Flags().StringVar(&opts.crName, "cr-name", "cluster", "ClusterReadiness object name, an RFC 1123 subdomain of at most 253 bytes (changing it leaves the old object behind: kubectl delete ucr <old-name>)")
	cmd.Flags().StringVar(&opts.teamLabel, "team-label", "team", "namespace label used for team attribution")
	cmd.Flags().DurationVar(&opts.forceSyncEvery, "force-sync-every", time.Hour,
		"push a snapshot even if unchanged after this long; must be positive, and a value below --interval means every tick")
	cmd.Flags().StringSliceVar(&opts.targets, "targets", nil,
		"target minors, CSV, e.g. 1.37,1.38, at most 8 distinct minors (the ClusterReadiness spec.targets cap; more is refused at start); when set, the ClusterReadiness spec.targets is reconciled to them every tick (overriding kubectl edits)")
	cmd.Flags().BoolVar(&opts.manageCRD, "manage-crd", true,
		"keep the ClusterReadiness CRD schema in step with this binary at startup, a failed check retried every tick until it succeeds (needs get/patch on that CRD); false = never touch the CRD")
	cmd.Flags().StringVar(&opts.healthAddr, "health-addr", ":8081",
		"listen address for /healthz, /readyz and /metrics (empty = disabled)")
	cmd.Flags().StringVar(&opts.logFormat, "log-format", "text", "log format: text (logfmt) or json")
	cmd.Flags().StringVar(&opts.logLevel, "log-level", "info", "log level: debug, info, warn or error")
	cmd.Flags().StringVar(&opts.registryDir, "registry-dir", "", registryDirUsage)
	cmd.Flags().StringVar(&opts.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: in-cluster config, then standard loading rules)")
	cmd.Flags().StringVar(&opts.kubecontext, "context", "", "kubeconfig context to use")
	cmd.Flags().DurationVar(&opts.requestTimeout, "request-timeout", defaultRequestTimeout, "give up on a single API request after this long (0 = no per-request limit)")
	return cmd
}
