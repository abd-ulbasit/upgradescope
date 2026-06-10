package cli

import (
	"context"
	"fmt"
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
}

// runAgent is the real I/O pipeline behind `upgradescope agent`. A package
// var so command tests can stub it (same pattern as runScan).
var runAgent = func(ctx context.Context, opts agentOptions) error {
	kbData, err := kb.Load()
	if err != nil {
		return fmt.Errorf("load knowledge base: %w", err)
	}
	cfg, err := buildAgentRESTConfig(opts.kubeconfig, opts.kubecontext)
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
		Interval:       opts.interval,
		ServerURL:      opts.serverURL,
		ServerToken:    opts.serverToken,
		ClusterName:    opts.clusterName,
		CRName:         opts.crName,
		TeamLabel:      opts.teamLabel,
		ForceSyncEvery: opts.forceSyncEvery,
	})
}

// buildAgentRESTConfig prefers in-cluster config (the agent's normal home)
// and falls back to kubeconfig loading rules — the same rules as scan. An
// explicit --kubeconfig or --context skips the in-cluster attempt entirely.
var buildAgentRESTConfig = func(kubeconfig, kubecontext string) (*rest.Config, error) {
	if kubeconfig == "" && kubecontext == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
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
	return cfg, nil
}

func newAgentCmd() *cobra.Command {
	var opts agentOptions
	cmd := &cobra.Command{
		Use:           "agent",
		Short:         "Run the in-cluster continuous upgrade-readiness agent",
		Long:          "Continuously collects cluster inventory, evaluates upgrade readiness, writes the ClusterReadiness CRD status, and (optionally) pushes snapshots to an upgradescope server.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runAgent(ctx, opts)
		},
	}
	cmd.Flags().DurationVar(&opts.interval, "interval", 10*time.Minute, "evaluation interval (minimum 1m)")
	cmd.Flags().StringVar(&opts.serverURL, "server-url", "", "upgradescope server base URL (empty = CRD-only mode)")
	cmd.Flags().StringVar(&opts.serverToken, "server-token", "", "bearer token for snapshot pushes (required with --server-url)")
	cmd.Flags().StringVar(&opts.clusterName, "cluster-name", "", "cluster label sent to the server (default: cluster UID)")
	cmd.Flags().StringVar(&opts.crName, "cr-name", "cluster", "ClusterReadiness object name")
	cmd.Flags().StringVar(&opts.teamLabel, "team-label", "team", "namespace label used for team attribution")
	cmd.Flags().DurationVar(&opts.forceSyncEvery, "force-sync-every", time.Hour, "push a snapshot even if unchanged after this long")
	cmd.Flags().StringVar(&opts.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: in-cluster config, then standard loading rules)")
	cmd.Flags().StringVar(&opts.kubecontext, "context", "", "kubeconfig context to use")
	return cmd
}
