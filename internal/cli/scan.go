package cli

import (
	"bytes"
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
	"github.com/abd-ulbasit/upgradescope/internal/suppress"
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

	configFile    string // --config; "" = discover (suppress.FindConfig)
	baselineFile  string
	writeBaseline string

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
		Use:   "scan",
		Short: "Scan a cluster (or rendered manifests) for upgrade readiness",
		Long:  scanLong,
		Example: `  # The current kubeconfig context's cluster against Kubernetes 1.37
  upgradescope scan --target 1.37

  # Another context, as JSON
  upgradescope scan --context prod --target 1.37 --output json

  # Rendered manifests in CI: SARIF for code scanning, exit 2 on a blocker
  upgradescope scan --files rendered/ --target 1.37 --output sarif > upgradescope.sarif

  # Fail only on findings that are new since an accepted scan
  upgradescope scan --files rendered/ --target 1.37 --write-baseline baseline.json
  upgradescope scan --files rendered/ --target 1.37 --baseline baseline.json`,
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
			// Config and baseline are read before scanning: a typo there
			// fails in a second, not after a five-minute cluster scan.
			ignore, err := loadIgnore(opts)
			if err != nil {
				return err
			}
			var baseline *suppress.Baseline
			if opts.baselineFile != "" {
				b, err := readBaseline(opts.baselineFile)
				if err != nil {
					return err
				}
				baseline = &b
			}
			report, err := runScan(opts)
			if err != nil {
				return err
			}
			report, warnings := suppress.Apply(report, ignore.rules, suppress.Options{Now: time.Now(), Source: ignore.source, FileBase: ignore.fileBase})
			for _, w := range warnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
			}
			if opts.writeBaseline != "" {
				if err := writeBaselineFile(opts, report); err != nil {
					return err
				}
			}
			if baseline != nil {
				report = baseline.Mark(report)
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
			return gate(report, opts.failOn, opts.allowIncomplete)
		},
	}

	cmd.Flags().StringVar(&opts.target, "target", "", "target Kubernetes minor version, e.g. 1.36 (required)")
	cmd.Flags().StringVar(&opts.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: standard loading rules)")
	cmd.Flags().StringVar(&opts.kubecontext, "context", "", "kubeconfig context to use")
	cmd.Flags().StringVar(&opts.filesDir, "files", "", "scan rendered manifests in this file or directory (*.yaml, *.yml, *.json) instead of a live cluster")
	cmd.Flags().StringVar(&opts.output, "output", "table", "output format: table|json|sarif|markdown")
	cmd.Flags().StringVar(&opts.teamLabel, "team-label", "team", "namespace label used for team attribution")
	cmd.Flags().StringVar(&opts.failOn, "fail-on", "blocker", "exit 2 if findings at/above this severity, or the verdict is unknown: blocker|warning|never")
	cmd.Flags().BoolVar(&opts.allowIncomplete, "allow-incomplete", false, "with --fail-on blocker|warning, do not fail when the verdict is unknown (required checks not assessed)")
	cmd.Flags().StringVar(&opts.configFile, "config", "", "config file with ignore rules (default: "+suppress.ConfigFile+" in the scan root, else at the git repository root)")
	cmd.Flags().StringVar(&opts.baselineFile, "baseline", "", "JSON report of an earlier scan (--output json or --write-baseline): the gate fails only on findings that are new since")
	cmd.Flags().StringVar(&opts.writeBaseline, "write-baseline", "", "also write this scan's JSON report to this path, for a later --baseline")
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

// withFileBase returns a copy of r whose finding (and suppressed finding)
// object paths are prefixed with base (see manifestBase); r itself is not
// modified.
func withFileBase(r engine.Report, base string) engine.Report {
	if base == "" {
		return r
	}
	prefix := func(f engine.Finding) engine.Finding {
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
		return f
	}
	findings := make([]engine.Finding, len(r.Findings))
	for i, f := range r.Findings {
		findings[i] = prefix(f)
	}
	r.Findings = findings
	if len(r.Suppressed) > 0 {
		suppressed := make([]engine.SuppressedFinding, len(r.Suppressed))
		for i, s := range r.Suppressed {
			s.Finding = prefix(s.Finding)
			suppressed[i] = s
		}
		r.Suppressed = suppressed
	}
	return r
}

// scanLong is scan's --help text: the gate's exit codes and how
// suppression and baselines change what it counts.
const scanLong = `Scan a cluster (or rendered manifests) for upgrade readiness.

Exit codes: 0 when the gate passes; 1 on an operational error, including an
invalid config file or baseline; 2 when the gate fails.

The gate (--fail-on) fails when a finding at or above the threshold remains,
or (unless --allow-incomplete) when a required check was not assessed, so a
blocker may have been missed.

Suppression: ignore rules in ` + suppress.ConfigFile + ` (found in the scan root,
i.e. the --files directory or else the working directory, then at the git
repository root; or named with --config) accept findings by key or category,
optionally only for objects matching namespace/name/file globs, with a
required reason and an optional expires date. Objects can opt out with the
upgradescope.dev/ignore and upgradescope.dev/ignore-reason annotations.
Suppressed findings do not count toward score, verdict or gate, and every
output lists them with their reason. An expired rule stops applying and
prints a warning.

Baseline: --baseline takes the JSON report of an earlier scan (--output json,
or --write-baseline). Findings whose key and listed objects it already had
are marked unchanged and do not fail the gate; anything else is new. Score
and verdict still count unchanged findings, and an unassessed required check
still fails the gate.`

// ignoreConfig holds the ignore rules in effect: source labels their
// suppressions and warnings (the config path), and fileBase is the
// scanned root relative to the config file's directory, which file globs
// are written against.
type ignoreConfig struct {
	rules    []suppress.Rule
	source   string
	fileBase string
}

// loadIgnore loads --config, or the discovered config file; no file means
// no rules.
func loadIgnore(opts scanOptions) (ignoreConfig, error) {
	root := "."
	if opts.filesDir != "" {
		root = opts.filesDir
		if fi, err := os.Stat(root); err == nil && !fi.IsDir() {
			root = filepath.Dir(root)
		}
	}
	file := opts.configFile
	if file == "" {
		found, err := suppress.FindConfig(root)
		if err != nil || found == "" {
			return ignoreConfig{}, err
		}
		file = workingPath(found)
	}
	cfg, err := suppress.LoadConfig(file)
	if err != nil {
		return ignoreConfig{}, err
	}
	ic := ignoreConfig{rules: cfg.Ignore, source: file}
	absRoot, rerr := filepath.Abs(root)
	absDir, derr := filepath.Abs(filepath.Dir(file))
	if opts.filesDir != "" && rerr == nil && derr == nil {
		if rel, err := filepath.Rel(absDir, absRoot); err == nil && rel != "." {
			ic.fileBase = filepath.ToSlash(rel)
		}
	}
	return ic, nil
}

// workingPath returns p relative to the working directory when it lies
// inside it, else p unchanged.
func workingPath(p string) string {
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return rel
		}
	}
	return p
}

func readBaseline(file string) (suppress.Baseline, error) {
	f, err := os.Open(file)
	if err != nil {
		return suppress.Baseline{}, fmt.Errorf("read --baseline: %w", err)
	}
	defer f.Close()
	b, err := suppress.ReadBaseline(f, reportSchemaVersion)
	if err != nil {
		return suppress.Baseline{}, fmt.Errorf("--baseline %s: %w", file, err)
	}
	return b, nil
}

// writeBaselineFile writes r as --output json would (object paths relative
// to the scanned root), for a later --baseline.
func writeBaselineFile(opts scanOptions, r engine.Report) error {
	var filesBase *string
	if opts.filesDir != "" {
		filesBase = &opts.fileBase
	}
	var buf bytes.Buffer
	if err := writeJSON(&buf, r, filesBase); err != nil {
		return err
	}
	if err := os.WriteFile(opts.writeBaseline, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("--write-baseline: %w", err)
	}
	return nil
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
	case "table", "json", "sarif", "markdown":
	default:
		return fmt.Errorf("invalid --output %q (want table, json, sarif, or markdown)", opts.output)
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
	case "markdown":
		WriteMarkdown(w, r)
		return nil
	default: // "table", already validated
		WriteTable(w, r)
		return nil
	}
}

// gate applies --fail-on: ErrGateFailed when findings that are not in the
// baseline reach the threshold, else ErrIncomplete when the assessment is
// incomplete (unless allowIncomplete). "never" never fails. Suppressed
// findings are no longer in r.Findings.
func gate(r engine.Report, failOn string, allowIncomplete bool) error {
	if failOn == "never" {
		return nil
	}
	if findingsReach(r, failOn) {
		return ErrGateFailed
	}
	if incomplete(r) && !allowIncomplete {
		return ErrIncomplete
	}
	return nil
}

// incomplete reports an unknown verdict, or a required check not assessed
// behind blockers that are all in the baseline: either way a new blocker
// may have gone unseen.
func incomplete(r engine.Report) bool {
	if r.Verdict == engine.VerdictUnknown {
		return true
	}
	for _, g := range r.NotAssessed {
		if g.Required {
			return true
		}
	}
	return false
}

func findingsReach(r engine.Report, failOn string) bool {
	var blockers, warnings int
	for _, f := range r.Findings {
		if f.BaselineState == engine.BaselineUnchanged {
			continue
		}
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
