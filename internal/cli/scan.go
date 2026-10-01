package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// ErrGateFailed signals findings at or above the --fail-on threshold.
// main maps it to exit code 2 — distinct from exit 1 (operational error),
// so CI can tell "scan worked, cluster not ready" from "scan broke".
var ErrGateFailed = errors.New("readiness gate failed: findings at or above --fail-on threshold")

// ErrIncomplete signals an unknown verdict under --fail-on blocker|warning:
// no blocker was found, but a required check could not run, so one may
// have been missed. Exit code 2 like ErrGateFailed; --allow-incomplete
// opts out.
var ErrIncomplete = errors.New("readiness gate failed: verdict unknown, required checks were not assessed (see NOT ASSESSED); pass --allow-incomplete to gate on findings alone")

// ExitCode maps an Execute error to the process exit code.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrGateFailed), errors.Is(err, ErrIncomplete):
		return 2
	default:
		return 1
	}
}

type scanOptions struct {
	target      string
	kubeconfig  string
	kubecontext string
	filesDir    string
	output      string
	teamLabel   string
	failOn      string

	allowIncomplete bool

	// targetVersion is opts.target parsed once by validateScanOptions;
	// runScan consumes it instead of re-parsing the raw string.
	targetVersion inventory.Version
}

// runScan is the real I/O pipeline: kb.Load → collect (cluster or files) →
// engine.Evaluate. A package var so tests can inject Reports without a cluster.
var runScan = func(opts scanOptions) (engine.Report, error) {
	kbData, err := kb.Load()
	if err != nil {
		return engine.Report{}, fmt.Errorf("load knowledge base: %w", err)
	}

	var inv inventory.Inventory
	if opts.filesDir != "" {
		inv, err = collect.CollectFiles(opts.filesDir)
		if err != nil {
			return engine.Report{}, fmt.Errorf("collect inventory: %w", err)
		}
	} else {
		clients, where, cerr := buildClients(opts.kubeconfig, opts.kubecontext)
		if cerr != nil {
			return engine.Report{}, cerr
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		inv = collect.Collect(ctx, clients, kbData, collect.Options{TeamLabel: opts.teamLabel})
		if err := unreadableCluster(inv, where); err != nil {
			return engine.Report{}, err
		}
	}

	return engine.Evaluate(inv, kbData, opts.targetVersion, time.Now()), nil
}

// buildClients uses clientcmd's standard loading rules ($KUBECONFIG, ~/.kube/config)
// with optional explicit path and context override. It also returns where
// the clients point (`context "x" (server https://...)`) for error messages.
// collect.NewClients(cfg) comes from the COLLECT section; this is its only call site.
func buildClients(kubeconfig, kubecontext string) (collect.Clients, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubecontext}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	cfg, err := loader.ClientConfig()
	if err != nil {
		return collect.Clients{}, "", fmt.Errorf("load kubeconfig: %w", err)
	}
	ctxName := kubecontext
	if ctxName == "" {
		if raw, rerr := loader.RawConfig(); rerr == nil {
			ctxName = raw.CurrentContext
		}
	}
	clients, err := collect.NewClients(cfg)
	return clients, fmt.Sprintf("context %q (server %s)", ctxName, cfg.Host), err
}

// unreadableCluster fails a live scan in which every capability failed:
// the cluster was never inspected (unreachable, expired credentials, RBAC
// denying everything), so a report would read "no findings". The message
// is one line, naming where the scan pointed and one capability's error,
// in collection order (versions makes the first request, so it is usually
// the most direct cause). client-go renders some refusals as just
// "unknown"; a more specific reason is preferred when one exists.
func unreadableCluster(inv inventory.Inventory, where string) error {
	if len(inv.Capabilities) == 0 {
		return nil
	}
	for _, st := range inv.Capabilities {
		if st.Available {
			return nil
		}
	}
	var reason string
	for _, c := range []inventory.Capability{inventory.CapVersions, inventory.CapHelm, inventory.CapAddOns, inventory.CapAPIUsage, inventory.CapDeprecatedCalls} {
		r := strings.Join(strings.Fields(inv.Capabilities[c].Reason), " ") // one line
		if reason == "" {
			reason = r
		}
		if r != "" && !strings.HasSuffix(r, ": unknown") {
			reason = r
			break
		}
	}
	return fmt.Errorf("cannot read the cluster at %s: %s; check the kubeconfig context, credentials and network", where, reason)
}

func newScanCmd() *cobra.Command {
	var opts scanOptions
	cmd := &cobra.Command{
		Use:           "scan",
		Short:         "Scan a cluster (or rendered manifests) for upgrade readiness",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateScanOptions(&opts); err != nil {
				return err
			}
			report, err := runScan(opts)
			if err != nil {
				return err
			}
			if err := writeReport(cmd.OutOrStdout(), opts.output, report); err != nil {
				return err
			}
			return gate(report, opts.failOn, opts.allowIncomplete)
		},
	}

	cmd.Flags().StringVar(&opts.target, "target", "", "target Kubernetes minor version, e.g. 1.36 (required)")
	cmd.Flags().StringVar(&opts.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: standard loading rules)")
	cmd.Flags().StringVar(&opts.kubecontext, "context", "", "kubeconfig context to use")
	cmd.Flags().StringVar(&opts.filesDir, "files", "", "scan rendered manifests in this directory instead of a live cluster")
	cmd.Flags().StringVar(&opts.output, "output", "table", "output format: table|json|sarif")
	cmd.Flags().StringVar(&opts.teamLabel, "team-label", "team", "namespace label used for team attribution")
	cmd.Flags().StringVar(&opts.failOn, "fail-on", "blocker", "exit 2 if findings at/above this severity, or the verdict is unknown: blocker|warning|never")
	cmd.Flags().BoolVar(&opts.allowIncomplete, "allow-incomplete", false, "with --fail-on blocker|warning, do not fail when the verdict is unknown (required checks not assessed)")
	_ = cmd.MarkFlagRequired("target")
	cmd.MarkFlagsMutuallyExclusive("files", "kubeconfig")
	cmd.MarkFlagsMutuallyExclusive("files", "context")
	cmd.MarkFlagsMutuallyExclusive("files", "team-label")

	return cmd
}

// validateScanOptions checks flag values and stores the parsed --target into
// opts.targetVersion (the single parse site — runScan does not re-parse).
func validateScanOptions(opts *scanOptions) error {
	target, err := inventory.ParseTarget(opts.target)
	if err != nil {
		return fmt.Errorf("invalid --target %q: %w", opts.target, err)
	}
	opts.targetVersion = target
	switch opts.output {
	case "table", "json", "sarif":
	default:
		return fmt.Errorf("invalid --output %q (want table, json, or sarif)", opts.output)
	}
	switch opts.failOn {
	case "blocker", "warning", "never":
	default:
		return fmt.Errorf("invalid --fail-on %q (want blocker, warning, or never)", opts.failOn)
	}
	return nil
}

func writeReport(w io.Writer, format string, r engine.Report) error {
	switch format {
	case "json":
		return WriteJSON(w, r)
	case "sarif":
		return WriteSARIF(w, r)
	default: // "table", already validated
		WriteTable(w, r)
		return nil
	}
}

// gate applies --fail-on: ErrGateFailed when findings reach the threshold,
// else ErrIncomplete when the verdict is unknown (unless allowIncomplete).
// "never" never fails.
func gate(r engine.Report, failOn string, allowIncomplete bool) error {
	if failOn == "never" {
		return nil
	}
	if findingsReach(r, failOn) {
		return ErrGateFailed
	}
	if r.Verdict == engine.VerdictUnknown && !allowIncomplete {
		return ErrIncomplete
	}
	return nil
}

func findingsReach(r engine.Report, failOn string) bool {
	var blockers, warnings int
	for _, f := range r.Findings {
		switch f.Severity {
		case engine.SevBlocker:
			blockers++
		case engine.SevWarning:
			warnings++
		}
	}
	switch failOn {
	case "warning":
		return blockers+warnings > 0
	default: // "blocker"
		return blockers > 0
	}
}
