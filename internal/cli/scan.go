package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/sarif"
)

// ErrGateFailed signals findings at or above the --fail-on threshold.
// main maps it to exit code 2 — distinct from exit 1 (operational error),
// so CI can tell "scan worked, cluster not ready" from "scan broke".
var ErrGateFailed = errors.New("readiness gate failed: findings at or above --fail-on threshold")

// ExitCode maps an Execute error to the process exit code.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrGateFailed):
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

	// targetVersion is opts.target parsed once by validateScanOptions;
	// runScan consumes it instead of re-parsing the raw string.
	targetVersion inventory.Version
	// fileBase is the directory files-mode object paths are relative to,
	// as seen from the working directory (see manifestBase).
	fileBase string
	// stderr receives files-mode warnings; nil discards them.
	stderr io.Writer
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
		var sum collect.FilesSummary
		inv, sum, err = collect.CollectFiles(opts.filesDir)
		if err != nil {
			return engine.Report{}, fmt.Errorf("collect inventory: %w", err)
		}
		stderr := opts.stderr
		if stderr == nil {
			stderr = io.Discard
		}
		for _, w := range sum.Warnings {
			fmt.Fprintf(stderr, "warning: skipped %s:%d: %v\n", path.Join(opts.fileBase, w.File), w.Line, w.Err)
		}
		// Nothing scanned is not "nothing to fix": an empty render, a wrong
		// path or an unexpected extension must not report 100/100.
		if sum.Objects == 0 {
			return engine.Report{}, fmt.Errorf("no Kubernetes manifests found under %s (%d files skipped)", opts.filesDir, sum.Skipped)
		}
	} else {
		clients, cerr := buildClients(opts.kubeconfig, opts.kubecontext)
		if cerr != nil {
			return engine.Report{}, cerr
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		inv = collect.Collect(ctx, clients, kbData, collect.Options{TeamLabel: opts.teamLabel})
	}

	return engine.Evaluate(inv, kbData, opts.targetVersion, time.Now()), nil
}

// buildClients uses clientcmd's standard loading rules ($KUBECONFIG, ~/.kube/config)
// with optional explicit path and context override.
// collect.NewClients(cfg) comes from the COLLECT section; this is its only call site.
func buildClients(kubeconfig, kubecontext string) (collect.Clients, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubecontext}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return collect.Clients{}, fmt.Errorf("load kubeconfig: %w", err)
	}
	return collect.NewClients(cfg)
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
			opts.stderr = cmd.ErrOrStderr()
			if opts.filesDir != "" {
				opts.fileBase = manifestBase(opts.filesDir)
			}
			report, err := runScan(opts)
			if err != nil {
				return err
			}
			// JSON keeps object paths relative to the scanned root (the
			// inventory contract) and records that root as filesBase;
			// people and SARIF consumers resolve them from the working
			// directory.
			out := report
			var filesBase *string
			if opts.output != "json" {
				out = withFileBase(report, opts.fileBase)
			} else if opts.filesDir != "" {
				filesBase = &opts.fileBase
			}
			if err := writeReport(cmd.OutOrStdout(), opts.output, out, filesBase); err != nil {
				return err
			}
			if opts.output == "sarif" && filepath.IsAbs(filepath.FromSlash(opts.fileBase)) {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: --files %s is outside the working directory, so SARIF locations are absolute file:// URIs that GitHub code scanning cannot place in the repository; run from the repository root\n", opts.filesDir)
			}
			if n := sarif.Unanchored(report); opts.output == "sarif" && n > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: %d finding(s) have no file location, so they are not SARIF results (GitHub rejects results without one); the SARIF lists them as tool execution notifications, and --output table or json shows them in full\n", n)
			}
			if gateFailed(report, opts.failOn) {
				return ErrGateFailed
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&opts.target, "target", "", "target Kubernetes minor version, e.g. 1.36 (required)")
	cmd.Flags().StringVar(&opts.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: standard loading rules)")
	cmd.Flags().StringVar(&opts.kubecontext, "context", "", "kubeconfig context to use")
	cmd.Flags().StringVar(&opts.filesDir, "files", "", "scan rendered manifests in this file or directory (*.yaml, *.yml, *.json) instead of a live cluster")
	cmd.Flags().StringVar(&opts.output, "output", "table", "output format: table|json|sarif")
	cmd.Flags().StringVar(&opts.teamLabel, "team-label", "team", "namespace label used for team attribution")
	cmd.Flags().StringVar(&opts.failOn, "fail-on", "blocker", "exit 2 if findings at/above this severity: blocker|warning|never")
	_ = cmd.MarkFlagRequired("target")
	cmd.MarkFlagsMutuallyExclusive("files", "kubeconfig")
	cmd.MarkFlagsMutuallyExclusive("files", "context")
	cmd.MarkFlagsMutuallyExclusive("files", "team-label")

	return cmd
}

// manifestBase returns the directory that --files object paths
// (collect.CollectFiles: relative to the scanned root; a single file's base
// name) resolve against, slash-separated and relative to the working
// directory when it lies inside it and absolute otherwise (also for a
// relative "../x": SARIF then carries an unambiguous file:// URI instead of
// a path that escapes the repository). CI runs from the repository root,
// and SARIF URIs must be repository-relative for GitHub to place
// annotations.
func manifestBase(filesDir string) string {
	base := filesDir
	if fi, err := os.Stat(filesDir); err == nil && !fi.IsDir() {
		base = filepath.Dir(filesDir)
	}
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
		if wd, err := os.Getwd(); err == nil {
			if rel, err := filepath.Rel(wd, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				base = rel
			}
		}
	}
	if base = filepath.ToSlash(filepath.Clean(base)); base == "." {
		return ""
	}
	return base
}

// withFileBase returns a copy of r whose finding object paths are prefixed
// with base (see manifestBase); r itself is not modified.
func withFileBase(r engine.Report, base string) engine.Report {
	if base == "" {
		return r
	}
	findings := make([]engine.Finding, len(r.Findings))
	for i, f := range r.Findings {
		if len(f.Objects) > 0 {
			objs := make([]inventory.ObjectRef, len(f.Objects))
			for j, o := range f.Objects {
				if o.File != "" {
					o.File = path.Join(base, o.File)
				}
				objs[j] = o
			}
			f.Objects = objs
		}
		findings[i] = f
	}
	r.Findings = findings
	return r
}

// validateScanOptions checks flag values and stores the parsed --target into
// opts.targetVersion (the single parse site — runScan does not re-parse).
func validateScanOptions(opts *scanOptions) error {
	target, err := inventory.ParseVersion(opts.target)
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

// writeReport renders r; filesBase is the JSON filesBase (nil outside
// --files mode).
func writeReport(w io.Writer, format string, r engine.Report, filesBase *string) error {
	switch format {
	case "json":
		return writeJSON(w, r, filesBase)
	case "sarif":
		return WriteSARIF(w, r)
	default: // "table", already validated
		WriteTable(w, r)
		return nil
	}
}

func gateFailed(r engine.Report, failOn string) bool {
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
	case "never":
		return false
	case "warning":
		return blockers+warnings > 0
	default: // "blocker"
		return blockers > 0
	}
}
