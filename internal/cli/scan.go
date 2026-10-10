package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/sarif"
	"github.com/abd-ulbasit/upgradescope/internal/suppress"
	"github.com/abd-ulbasit/upgradescope/internal/textsafe"
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

// ErrTargetNotUpgrade signals a --target at or below the minor the
// cluster's kube-apiserver already runs (a downgrade, a no-op, or a typo
// like 1.4). The verdict is unknown, and since this is a user error
// rather than a coverage limit, --allow-incomplete does not excuse it.
// Exit code 2 like ErrIncomplete.
var ErrTargetNotUpgrade = errors.New("readiness gate failed: verdict unknown, --target is not an upgrade of this cluster")

// ExitCode maps an Execute error to the process exit code.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrGateFailed), errors.Is(err, ErrIncomplete), errors.Is(err, ErrTargetNotUpgrade):
		return 2
	default:
		return 1
	}
}

// ErrorText is err as the process prints it on stderr. An error can quote
// a file name or a value from the manifests being scanned, so every control
// character in it is shown as an escape, a newline too: a name holding a
// newline must not start a second, forged line. The only newlines kept are
// those between the errors of a newline-joined multi-error (errors.Join's,
// or several %w joined by newlines alone), found by walking the error tree
// rather than by looking at the text.
func ErrorText(err error) string {
	var b strings.Builder
	writeError(&b, err)
	return b.String()
}

func writeError(b *strings.Builder, err error) {
	msg := err.Error()
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		kids := u.Unwrap()
		texts := make([]string, len(kids))
		for i, k := range kids {
			texts[i] = k.Error()
		}
		// Only a pure join (errors.Join) has a message that is its children's,
		// separated by newlines; fmt.Errorf with several %w does not.
		if len(kids) > 0 && msg == strings.Join(texts, "\n") {
			for i, k := range kids {
				if i > 0 {
					b.WriteByte('\n')
				}
				writeError(b, k)
			}
			return
		}
	case interface{ Unwrap() error }:
		// "context: %w" puts the wrapped error's text last; keep a join in it.
		if k := u.Unwrap(); k != nil {
			if kmsg := k.Error(); strings.HasSuffix(msg, kmsg) {
				b.WriteString(textsafe.Escape(msg[:len(msg)-len(kmsg)]))
				writeError(b, k)
				return
			}
		}
	}
	b.WriteString(textsafe.Escape(msg))
}

type scanOptions struct {
	target      string
	kubeconfig  string
	kubecontext string
	filesDir    string
	registryDir string // --registry-dir: extra add-on registry entries
	output      string
	teamLabel   string
	failOn      string

	allowIncomplete bool

	// plan (--plan) adds the upgrade plan up to the target; from (--from)
	// is where a --files plan starts, parsed into fromVersion.
	plan        bool
	from        string
	fromVersion inventory.Version

	// requestTimeout bounds each API request of a live scan
	// (rest.Config.Timeout); 0 = no per-request bound.
	requestTimeout time.Duration

	configFile    string // --config; "" = discover (suppress.FindConfig)
	baselineFile  string
	writeBaseline string

	// targetVersion is opts.target parsed once by validateScanOptions;
	// runScan consumes it instead of re-parsing the raw string.
	targetVersion inventory.Version
	// fileBase is the directory files-mode object paths are relative to,
	// as seen from the working directory (see manifestBase).
	fileBase string
	// stderr receives files-mode and --plan warnings; nil discards them.
	stderr io.Writer
	// ignore holds the ignore rules: RunE applies them to the report,
	// runScan to each hop of a --plan.
	ignore ignoreConfig
}

// registryDirUsage is the help of --registry-dir on scan, agent and serve.
const registryDirUsage = "extra add-on registry entries: one <id>.yaml file or a directory of them, in the schema of registry/CONTRIBUTING.md and validated like the embedded entries; an entry with an embedded id replaces it"

// runScan is the real I/O pipeline: kb.Load → collect (cluster or files) →
// engine.Evaluate. A package var so tests can inject Reports without a cluster.
var runScan = func(opts scanOptions) (engine.Report, error) {
	kbData, err := kb.LoadWithRegistry(opts.registryDir)
	if err != nil {
		return engine.Report{}, fmt.Errorf("load knowledge base: %w", err)
	}

	var inv inventory.Inventory
	if opts.filesDir != "" {
		var sum collect.FilesSummary
		inv, sum, err = collect.CollectFiles(opts.filesDir, kbData)
		if err != nil {
			return engine.Report{}, fmt.Errorf("collect inventory: %w", err)
		}
		stderr := opts.stderr
		if stderr == nil {
			stderr = io.Discard
		}
		for _, w := range sum.Warnings {
			w.File = path.Join(opts.fileBase, w.File)
			if w.Unassessed {
				fmt.Fprintf(stderr, "warning: skipped %s\n", esc(w.String()))
			} else {
				fmt.Fprintf(stderr, "warning: %s\n", esc(w.String()))
			}
		}
		// Nothing scanned is not "nothing to fix": an empty render, a wrong
		// path or an unexpected extension must not report 100/100.
		if sum.Objects == 0 {
			return engine.Report{}, fmt.Errorf("no Kubernetes manifests found under %s (%d files skipped)", opts.filesDir, sum.Skipped)
		}
	} else {
		inv, cluster, err := collectCluster(context.Background(), kbData, opts)
		if err != nil {
			return engine.Report{}, err
		}
		r := evaluateScan(inv, kbData, opts, time.Now())
		r.KubeContext, r.APIServer = cluster.context, cluster.server
		return r, nil
	}

	return evaluateScan(inv, kbData, opts, time.Now()), nil
}

// collectCluster reads the live cluster opts names (--kubeconfig,
// --context) into an inventory, within five minutes and ctx, and refuses a
// cluster it could read nothing of (unreadableCluster).
func collectCluster(ctx context.Context, kbData kb.KB, opts scanOptions) (inventory.Inventory, liveCluster, error) {
	clients, cluster, err := buildClients(opts.kubeconfig, opts.kubecontext, opts.requestTimeout)
	if err != nil {
		return inventory.Inventory{}, liveCluster{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	inv := collect.Collect(ctx, clients, kbData, collect.Options{TeamLabel: opts.teamLabel})
	if err := unreadableCluster(inv, cluster.String()); err != nil {
		return inventory.Inventory{}, liveCluster{}, err
	}
	return inv, cluster, nil
}

// evaluateScan judges inv at the target and, with --plan, adds the
// upgrade plan (planHops).
func evaluateScan(inv inventory.Inventory, k kb.KB, opts scanOptions, now time.Time) engine.Report {
	r := engine.Evaluate(inv, k, opts.targetVersion, now)
	if opts.plan {
		r.Hops = planHops(inv, k, opts, now)
	}
	return r
}

// planHops is the --plan upgrade plan, from --from in files mode, else
// from the cluster's oldest kube-apiserver (engine.PlanFrom). Each hop's
// report goes through the scan's ignore rules first, so a suppressed
// finding is in no hop, as it is not in the report. Nil, with a warning,
// when the cluster's version is unknown or the target is not an upgrade
// of it.
func planHops(inv inventory.Inventory, k kb.KB, opts scanOptions, now time.Time) []engine.Hop {
	warn := func(msg string) {
		if opts.stderr != nil {
			fmt.Fprintf(opts.stderr, "warning: --plan: %s, so there is no upgrade plan; the report judges the target alone\n", esc(msg))
		}
	}
	from := opts.fromVersion
	if opts.filesDir == "" {
		v, ok := engine.PlanFrom(inv)
		if !ok {
			warn("the cluster's kube-apiserver version is unknown")
			return nil
		}
		from = v
	}
	targets := engine.HopTargets(k, from, opts.targetVersion)
	if len(targets) == 0 {
		// --files plans check --from against --target up front.
		warn(fmt.Sprintf("--target %s is not an upgrade of the cluster's %s", opts.targetVersion, from))
		return nil
	}
	reports := make([]engine.Report, len(targets))
	// suppress.Apply's warnings (expired rules, annotations without a
	// reason) are dropped here: the scan applies the same rules to the
	// report and prints them once from there.
	for i, t := range targets {
		reports[i], _ = suppress.Apply(engine.Evaluate(inv, k, t, now), opts.ignore.rules,
			suppress.Options{Now: now, Source: opts.ignore.source, FileBase: opts.ignore.fileBase})
	}
	return engine.PlanReports(from, reports)
}

// liveCluster names the cluster a live scan reads: the kubeconfig context
// and the API server (see apiServerURL).
type liveCluster struct{ context, server string }

// String is where the scan pointed, for error messages.
func (c liveCluster) String() string {
	return fmt.Sprintf("context %q (server %s)", c.context, c.server)
}

// apiServerURL is the API server cfg points at, as client-go dials it
// (a host without a scheme gets the one client-go picks), reduced to
// scheme, host and port: kubeconfig servers can carry credentials, a
// proxy path or a token query, which a report must not repeat.
func apiServerURL(cfg *rest.Config) string {
	u, _, err := rest.DefaultServerUrlFor(cfg)
	if err != nil {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
}

// scanRESTConfig loads the rest.Config of a live scan by clientcmd's
// standard loading rules ($KUBECONFIG, ~/.kube/config), with optional
// explicit path and context override, and returns the name of the context
// it resolved to. requestTimeout becomes rest.Config.Timeout: client-go
// gives up on any single request after it, so an API server that accepts
// a request and never answers fails that request instead of the scan.
func scanRESTConfig(kubeconfig, kubecontext string, requestTimeout time.Duration) (*rest.Config, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubecontext}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	cfg, err := loader.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load kubeconfig: %w", err)
	}
	cfg.Timeout = requestTimeout
	ctxName := kubecontext
	if ctxName == "" {
		if raw, rerr := loader.RawConfig(); rerr == nil {
			ctxName = raw.CurrentContext
		}
	}
	return cfg, ctxName, nil
}

// buildClients builds the live-scan clients from scanRESTConfig. It also
// returns the cluster they point at, for the report and error messages.
// collect.NewClients(cfg) comes from the COLLECT section; this is its only call site.
func buildClients(kubeconfig, kubecontext string, requestTimeout time.Duration) (collect.Clients, liveCluster, error) {
	cfg, ctxName, err := scanRESTConfig(kubeconfig, kubecontext, requestTimeout)
	if err != nil {
		return collect.Clients{}, liveCluster{}, err
	}
	clients, err := collect.NewClients(cfg)
	return clients, liveCluster{context: ctxName, server: apiServerURL(cfg)}, err
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

  # What each control-plane upgrade on the way to 1.37 needs fixed first
  upgradescope scan --target 1.37 --plan

  # Rendered manifests in CI: SARIF for code scanning, exit 2 on a blocker
  upgradescope scan --files rendered/ --target 1.37 --output sarif > upgradescope.sarif

  # GitLab, Jenkins or Azure Pipelines: a Code Quality or JUnit report
  upgradescope scan --files rendered/ --target 1.37 --output gitlab-codequality > gl-code-quality-report.json
  upgradescope scan --files rendered/ --target 1.37 --output junit > upgradescope-junit.xml

  # Fail only on findings that are new since an accepted scan
  upgradescope scan --files rendered/ --target 1.37 --write-baseline baseline.json
  upgradescope scan --files rendered/ --target 1.37 --baseline baseline.json`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.filesDir != "" && cmd.Flags().Changed("team-label") {
				return errTeamLabelNeedsCluster
			}
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
			opts.ignore = ignore
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
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", esc(w))
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
			if err := writeReport(cmd.OutOrStdout(), opts, out, filesBase); err != nil {
				return err
			}
			if opts.output == "sarif" && filepath.IsAbs(filepath.FromSlash(opts.fileBase)) {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: --files %s is outside the working directory, so SARIF locations are absolute file:// URIs that GitHub code scanning cannot place in the repository; run from the repository root\n", opts.filesDir)
			}
			if opts.output == "gitlab-codequality" && filepath.IsAbs(filepath.FromSlash(opts.fileBase)) {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: --files %s is outside the working directory, so Code Quality paths are absolute and GitLab cannot place them in the repository; run from the repository root\n", opts.filesDir)
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
	cmd.Flags().DurationVar(&opts.requestTimeout, "request-timeout", defaultRequestTimeout, "give up on a single API request after this long (0 = no per-request limit)")
	cmd.Flags().StringVar(&opts.filesDir, "files", "", "scan rendered manifests in this file or directory (*.yaml, *.yml, *.json) instead of a live cluster")
	cmd.Flags().StringVar(&opts.registryDir, "registry-dir", "", registryDirUsage)
	cmd.Flags().StringVar(&opts.output, "output", "table", "output format: table|json|sarif|markdown|junit|gitlab-codequality")
	cmd.Flags().StringVar(&opts.teamLabel, "team-label", "team", "namespace label used for team attribution (live scans only: --files mode reads no Namespace objects, so all its findings are unattributed)")
	cmd.Flags().StringVar(&opts.failOn, "fail-on", "blocker", "exit 2 if findings at/above this severity, or the verdict is unknown: blocker|warning|never (never always exits 0, even for a --target that is not an upgrade)")
	cmd.Flags().BoolVar(&opts.allowIncomplete, "allow-incomplete", false, "with --fail-on blocker|warning, do not fail when the verdict is unknown (required checks not assessed); a --target that is not an upgrade still fails")
	cmd.Flags().StringVar(&opts.configFile, "config", "", "config file with ignore rules (default: "+suppress.ConfigFile+" in the scan root, else at the git repository root)")
	cmd.Flags().StringVar(&opts.baselineFile, "baseline", "", "JSON report of an earlier scan (--output json or --write-baseline): the gate fails only on findings that are new since")
	cmd.Flags().StringVar(&opts.writeBaseline, "write-baseline", "", "also write this scan's JSON report to this path, for a later --baseline")
	cmd.Flags().BoolVar(&opts.plan, "plan", false, "also judge each control-plane upgrade on the way to --target, one minor at a time, listing each finding at the first upgrade it affects (table, markdown, json)")
	cmd.Flags().StringVar(&opts.from, "from", "", "with --plan and --files: the minor the cluster runs now, where the plan starts (a live scan reads it from the cluster)")
	_ = cmd.MarkFlagRequired("target")
	cmd.MarkFlagsMutuallyExclusive("files", "kubeconfig")
	cmd.MarkFlagsMutuallyExclusive("files", "context")
	cmd.MarkFlagsMutuallyExclusive("files", "request-timeout")

	return cmd
}

// errTeamLabelNeedsCluster is the refusal of --team-label with --files.
var errTeamLabelNeedsCluster = errors.New("--team-label needs a live cluster: team attribution reads the labels of the cluster's Namespace objects, and --files mode reads none, so every finding of a files scan is unattributed (attribute teams with a live scan, the agent, or the server's gate with ?cluster=)")

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

// scanLong is scan's --help text: the gate's exit codes, how --files reads
// manifests, and how suppression and baselines change what it counts. The
// knowledge base's floor in it is inventory.OldestCovered, not a copy.
var scanLong = `Scan a cluster (or rendered manifests) for upgrade readiness.

Exit codes: 0 when the gate passes; 1 on an operational error, including an
invalid config file or baseline and a report that could not be written; 2
when the gate fails, which includes an unknown verdict.

The gate (--fail-on) fails when a finding at or above the threshold remains,
or (unless --allow-incomplete) when a required check was not assessed, so a
blocker may have been missed. A --target that is not an upgrade of the
cluster (at or below the minor its kube-apiserver runs) always fails it,
--allow-incomplete notwithstanding; only --fail-on never, which always exits
0, passes it. A --target below the oldest minor the knowledge base covers
(` + inventory.OldestCovered().String() + `) is an error (exit 1), not a verdict: quote it in YAML and workflow
files, where an unquoted 1.30 is the number 1.3.

CI report formats: the exit code is the gate's in every --output format.
--output junit writes JUnit XML, one test suite per finding category and one
test case per finding, whose outcomes follow --fail-on and --allow-incomplete:
a finding the gate fails on is a failure, one below it passes, suppressed and
baseline-unchanged findings are skipped, and a required check that was not
assessed is an error. --output gitlab-codequality writes a GitLab Code Quality
report: an entry per finding and file location (blocker critical, warning
minor, info info) with a fingerprint that survives line moves; a finding
without a file is placed on the virtual path upgradescope/<finding key>, and
suppressed findings are left out.

Upgrade plan (--plan): the control plane is upgraded one minor at a time, so
--plan also judges the cluster at each minor between the one it runs (a live
scan's oldest kube-apiserver; --from with --files) and --target, and lists
each finding at the first upgrade it affects, with the upgrade where its
severity changes. Table and markdown show the plan before the findings; JSON
adds hops. Ignore rules apply to every upgrade. The rest of the report, and
the gate, judge --target alone.

Files mode (--files): every *.yaml, *.yml and *.json file under the directory,
or the one file named, is decoded as kubectl apply -f decodes it: each
document of a YAML stream and each object of a JSON stream (NDJSON,
pretty-printed or adjacent), List items expanded, a duplicate key taking its
last value. Every document is also decoded by kubectl's own decoder: where it
finds an object the scan's line-tracking YAML reading did not, kubectl's
objects are counted (located at the document's first line), with a warning.
Documents that are not Kubernetes objects are skipped. A document
that cannot be decoded is skipped with a warning; when its text names an API
the knowledge base lists as removed, api-usage is not assessed, so the verdict
is at least unknown and the gate fails unless --allow-incomplete. VCS metadata,
node_modules and Go vendor/ directories (with modules.txt) are not walked, nor
are symlinked directories (kubectl apply -R does not follow them either); each
of these but VCS metadata is a warning. Files mode assesses API usage and
add-ons: the container and init-container images and labels of Pod, Deployment,
DaemonSet, StatefulSet, ReplicaSet, Job and CronJob pod templates, and
IngressClass controllers, matched as a live scan matches them. Images injected
at admission (a mesh sidecar) are not in the manifests. Version skew, Helm
releases and deprecated API callers need a cluster and are not assessed, and
so does team attribution: --files reads no Namespace objects, so every finding
is unattributed and --team-label is refused. A manifest at an API version the
target does not serve yet (introduced after it) is a blocker like a removed
one, since applying it fails the same way; a live scan never reports that.

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
	case "table", "json", "sarif", "markdown", "junit", "gitlab-codequality":
	default:
		return fmt.Errorf("invalid --output %q (want table, json, sarif, markdown, junit, or gitlab-codequality)", opts.output)
	}
	switch opts.failOn {
	case "blocker", "warning", "never":
	default:
		return fmt.Errorf("invalid --fail-on %q (want blocker, warning, or never)", opts.failOn)
	}
	if err := validatePlanOptions(opts); err != nil {
		return err
	}
	return validRequestTimeout(opts.requestTimeout)
}

// validatePlanOptions checks --plan and --from and stores the parsed
// --from into opts.fromVersion.
func validatePlanOptions(opts *scanOptions) error {
	switch {
	case opts.from != "" && !opts.plan:
		return errors.New("--from needs --plan")
	case !opts.plan:
		return nil
	case opts.output == "sarif" || opts.output == "junit" || opts.output == "gitlab-codequality":
		return fmt.Errorf("--plan renders in table, markdown and json; %s has no place for upgrade steps, so run the %s scan without --plan", opts.output, opts.output)
	case opts.filesDir == "" && opts.from != "":
		return errors.New("--from is for --files scans; a live scan plans from the version its kube-apiserver runs")
	case opts.filesDir == "":
		return nil
	case opts.from == "":
		return errors.New("--plan with --files needs --from, the minor the cluster runs now")
	}
	// A cluster's own version has no floor (unlike a target): a 1.15
	// cluster plans here as it does live.
	from, err := inventory.ParseClusterVersion(opts.from)
	if err != nil {
		return fmt.Errorf("invalid --from %q: %w", opts.from, err)
	}
	if from.Compare(opts.targetVersion) >= 0 {
		return fmt.Errorf("--from %s must be older than --target %s", from, opts.targetVersion)
	}
	opts.fromVersion = from
	return nil
}

// defaultRequestTimeout is the --request-timeout of scan and agent: long
// enough for a page of a large list or the apiserver's /metrics, short
// enough that a stalled request leaves the rest of the scan its budget.
const defaultRequestTimeout = 30 * time.Second

func validRequestTimeout(d time.Duration) error {
	if d < 0 {
		return fmt.Errorf("invalid --request-timeout %s (want 0 or more; 0 = no per-request limit)", d)
	}
	return nil
}

// writeReport renders r in opts.output; filesBase is the JSON filesBase
// (nil outside --files mode), and JUnit outcomes follow opts' gate. A
// failed write is returned in every format, so the scan exits 1 instead of
// passing or failing the gate with the report lost.
func writeReport(w io.Writer, opts scanOptions, r engine.Report, filesBase *string) error {
	switch opts.output {
	case "json":
		return writeJSON(w, r, filesBase)
	case "sarif":
		return WriteSARIF(w, r)
	case "junit":
		return WriteJUnit(w, r, opts.failOn, opts.allowIncomplete)
	case "gitlab-codequality":
		return WriteGitLabCodeQuality(w, r)
	case "markdown":
		ew := &errWriter{w: w}
		WriteMarkdown(ew, r)
		return ew.err
	default: // "table", already validated
		return WriteTable(w, r)
	}
}

// gate applies --fail-on: ErrGateFailed when findings that are not in the
// baseline reach the threshold, else ErrIncomplete when the assessment is
// incomplete (unless allowIncomplete). A target that is not an upgrade is
// a user error, not a coverage limit, so allowIncomplete does not excuse
// it. "never" never fails. Suppressed findings are no longer in
// r.Findings.
func gate(r engine.Report, failOn string, allowIncomplete bool) error {
	if failOn == "never" {
		return nil
	}
	if findingsReach(r, failOn) {
		return ErrGateFailed
	}
	for _, g := range r.NotAssessed {
		if g.Capability == engine.GapTarget {
			return fmt.Errorf("%w (%s)", ErrTargetNotUpgrade, g.Reason)
		}
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
