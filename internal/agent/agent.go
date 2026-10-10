// Package agent runs the in-cluster continuous loop: collect → evaluate per
// target → ClusterReadiness CRD status (always) → push snapshot to the server
// on content change. The agent's local value never depends on server
// availability (spec §3).
package agent

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/secretfile"
	"github.com/abd-ulbasit/upgradescope/internal/suppress"
)

// AgentVersion is stamped into CRD status and push envelopes. The CLI sets it
// from the build version; "dev" otherwise.
var AgentVersion = "dev"

// ignoreSource labels spec.ignore suppressions and warnings.
const ignoreSource = "spec.ignore"

// maxSkipNotes is how many invalid or duplicate spec targets get a note of
// their own in status.notAssessed; a last line counts the rest.
const maxSkipNotes = 8

type Config struct {
	Interval    time.Duration // default 10m, min 1m
	ServerURL   string        // optional; "" = CRD-only mode
	ServerToken string        // bearer for push
	// ServerTokenFile, when set, is the file the push token is read from,
	// in place of ServerToken: a mounted Secret. It is read at start (a
	// missing, empty or unacceptable file fails it) and re-read when it
	// changes, checked at most every 5 seconds when a push is made, so a
	// rotated token needs no restart. A new file that is empty or unreadable
	// leaves the old token in use and is logged, naming the file and never
	// the token.
	ServerTokenFile string
	ClusterName     string        // human label sent to server; default = ClusterID
	CRName          string        // default crd.DefaultName
	TeamLabel       string        // default "team"
	ForceSyncEvery  time.Duration // default 1h: push even if hash unchanged
	// PodPassEvery and PodPassMaxAge set how often the agent lists every pod
	// outside kube-system: every PodPassEvery ticks (the pass is the first
	// of them), and sooner when the last full pass is PodPassMaxAge old; in
	// between, add-ons are detected from that pass's images and labels, so
	// an add-on installed or upgraded since (other than through a Helm
	// release or GitOps chart reference, whose change forces a full pass) is
	// reported as it was for at most PodPassEvery-1 ticks, and a pass
	// PodPassMaxAge old is not reused (#228).
	// Defaults collect.DefaultPodPassEvery (3) and
	// collect.DefaultPodPassMaxAge (1h); 1 lists every pod every tick.
	PodPassEvery  int
	PodPassMaxAge time.Duration
	// ServerRootCAs verifies the server's certificate on pushes (from
	// LoadServerCAs: the system roots plus a private CA); nil = the system
	// roots.
	ServerRootCAs *x509.CertPool
	// Targets, when non-empty, are the source of truth for spec.targets:
	// every tick reconciles the CR to them, overriding kubectl edits. Empty
	// leaves spec.targets to whoever edits the CR. applyDefaults normalizes
	// each to MAJOR.MINOR ("v1.38" → "1.38", "1.37.2" → "1.37").
	Targets []string
	// HelmNamespaces, when non-empty, are the only namespaces Helm release
	// storage is read in (--helm-namespaces, #344): for a role that grants
	// Secrets and ConfigMaps there alone (the chart's
	// rbac.helmSecretsNamespaces). The helm capability is then partial,
	// saying releases elsewhere were not assessed. Empty reads the whole
	// cluster (collect.Options.HelmNamespaces).
	HelmNamespaces []string
	// SkipCRDManagement leaves the CRD alone entirely (--manage-crd=false):
	// no read, no schema upgrade. The default keeps it in step with the
	// embedded manifest at startup.
	SkipCRDManagement bool
	// HealthAddr is where /healthz, /readyz and /metrics listen, e.g.
	// ":8081"; "" serves none of them.
	HealthAddr string
	// PprofAddr, when set, serves the Go profiler (/debug/pprof/) on its
	// own listener, apart from the health one. Loopback only
	// (ValidatePprofAddr): a profile exposes the process's memory and
	// goroutines, and only a pod's own network namespace (kubectl
	// port-forward) should reach it. "" serves none (#247).
	PprofAddr string
	// Logger receives the startup line and one line per tick; nil uses
	// slog.Default().
	Logger *slog.Logger

	// serverTokenFn gives the push token at each push: the file's current
	// value (ServerTokenFile), set by applyDefaults; nil = ServerToken.
	serverTokenFn func() string
	// secretOpts configures the secretfile.File behind ServerTokenFile
	// (tests: check interval, log).
	secretOpts []secretfile.Option
}

// ValidateInterval rejects an evaluation interval below the 1m minimum.
// Zero is below it: Config treats a zero Interval as unset, but a caller
// that was given an interval explicitly (the --interval flag) must not
// have 0 silently mean the default.
func ValidateInterval(d time.Duration) error {
	if d < time.Minute {
		return fmt.Errorf("interval %s below minimum 1m", d)
	}
	return nil
}

// ValidateServerURL rejects a --server-url no push could reach: anything
// but an http or https URL with a host. Without a scheme ("fleet.example.com")
// or with another one, every push would fail with "unsupported protocol
// scheme", retried as if the server were down (#238).
func ValidateServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("server URL %q: %w", raw, err)
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return fmt.Errorf("server URL %q: want an http:// or https:// URL, e.g. https://upgradescope.example.com", raw)
	}
	if u.Hostname() == "" { // "https://:8080" has a Host (":8080") but no host: Go would dial localhost
		return fmt.Errorf("server URL %q: no host", raw)
	}
	return nil
}

// ValidateServerToken rejects a push token no request could carry: one
// with whitespace or control characters in it. A trailing newline (a
// Secret made from a file with one) made every push fail with an invalid
// Authorization header, retried as if the server were down (#238).
// Callers reading a token from a file or the environment trim its
// surrounding whitespace first; what is left must have none.
func ValidateServerToken(tok string) error {
	if i := strings.IndexFunc(tok, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }); i >= 0 {
		return fmt.Errorf("server token has whitespace or a control character at byte %d: a bearer token has none", i)
	}
	return nil
}

// ValidateForceSyncEvery rejects a force-sync period that is not positive.
// Config treats a zero ForceSyncEvery as unset (1h), but a caller that was
// given one explicitly (--force-sync-every) must not have 0 silently mean
// the default, nor a negative period mean every tick (#238). A period at
// or below the interval is not refused: it means every tick
// (forceSyncInEffect), and Run says so.
func ValidateForceSyncEvery(d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("force-sync-every %s: must be positive (how long an unchanged inventory waits before it is pushed again)", d)
	}
	return nil
}

// ValidatePodPassEvery rejects a --pod-pass-every below 1. Config treats a
// zero PodPassEvery as unset (3), but a caller that was given one explicitly
// must not have 0 silently mean the default; 1 lists every pod every tick.
func ValidatePodPassEvery(n int) error {
	if n < 1 {
		return fmt.Errorf("pod-pass-every %d: must be at least 1 (the number of ticks per full read of the pods outside kube-system; 1 reads them every tick)", n)
	}
	return nil
}

// ValidatePodPassMaxAge rejects a --pod-pass-max-age that is not positive:
// a full read of the pods could never be put off for a duration of zero or
// less, and 0 must not silently mean the default (#228).
func ValidatePodPassMaxAge(d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("pod-pass-max-age %s: must be positive (how old the last full read of the pods may be when a tick reuses it)", d)
	}
	return nil
}

// ValidatePprofAddr rejects a --pprof-addr that is not a loopback host and
// port ("127.0.0.1:6060", "localhost:6060", "[::1]:6060"; "" is off). A
// profile lets whoever can fetch it read the process's heap and goroutine
// stacks, so the profiler never listens on a port without a host, nor on a
// wildcard or a routable address (#247).
func ValidatePprofAddr(addr string) error {
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("pprof address %q: want host:port on loopback, e.g. 127.0.0.1:6060 (%v)", addr, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("pprof address %q: port %q is not a number from 0 to 65535", addr, port)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("pprof address %q: the host must be loopback (127.0.0.1, [::1] or localhost): a profile exposes the process's memory and goroutines, so reach it with kubectl port-forward", addr)
	}
	return nil
}

// pprofHandler serves the profiler's endpoints on a mux of their own:
// importing net/http/pprof also registers them on http.DefaultServeMux,
// which nothing in this program serves.
func pprofHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// startupCRDTimeout bounds the CRD check at startup: a get, a patch or a
// create and its wait for Established (crd.establishTimeout, 10s). One
// that runs out is retried on every tick (runner.ensureCRD).
var startupCRDTimeout = 30 * time.Second

// minTickSpacing is the shortest time between two ticks' pushes: Run
// sleeps jitter(interval), at least this long, after a tick ends (its push
// done) and before the next begins.
func minTickSpacing(interval time.Duration) time.Duration {
	return interval - interval/10
}

// forceSyncInEffect is the force-sync period the agent applies. One at or
// below the interval asks for a push every tick (or more than one), so it
// means every tick, as it did before #238: it is lowered to
// minTickSpacing, which no tick spacing can undercut. Keeping it at the
// interval instead would skip the force-sync on every tick the jitter
// brings early, about half of them, since maybePush compares it to the
// time since the last push.
func forceSyncInEffect(forceSync, interval time.Duration) time.Duration {
	if forceSync <= interval {
		return minTickSpacing(interval)
	}
	return forceSync
}

// podPassMaxAgeFloor is the age a --pod-pass-max-age must exceed for
// --pod-pass-every to be able to reuse a pass on every one of the
// every-1 ticks after it: every-1 of the longest spacing the jitter draws
// between two ticks, 11/10 of the interval. It is a floor and not enough:
// the sleep starts after a tick ends, so each spacing also holds that
// tick's run time, which the floor leaves out because the agent does not
// know it ahead. 0 when every is 1 or less, which reuses nothing. A
// product beyond the largest Duration (a --pod-pass-every of billions,
// which nothing bounds above) saturates there instead of wrapping to a
// small or negative age: no max age exceeds it, so the warning is raised.
func podPassMaxAgeFloor(interval time.Duration, every int) time.Duration {
	if every <= 1 {
		return 0
	}
	spacing := jitterBy(interval, int64(interval/5))
	n := time.Duration(every - 1)
	if spacing > 0 && n > math.MaxInt64/spacing {
		return math.MaxInt64
	}
	return n * spacing
}

// podPassEveryThatFits is the largest --pod-pass-every whose
// podPassMaxAgeFloor is below maxAge, and at least 1 (which lists every
// pod on every tick, the only setting left when maxAge is not above one
// longest spacing). The warning offers it as the way to keep the max age
// and still reuse the pass; the ticks' run time comes on top of the floor,
// so a slow tick may call for one less.
func podPassEveryThatFits(interval, maxAge time.Duration) int {
	spacing := jitterBy(interval, int64(interval/5))
	if spacing <= 0 || maxAge <= spacing {
		return 1
	}
	// Every k satisfies (k-1) x spacing < maxAge, that is
	// k-1 <= (maxAge-1)/spacing.
	n := int64((maxAge - 1) / spacing)
	if n >= math.MaxInt-1 {
		return math.MaxInt
	}
	return 1 + int(n)
}

// podPassMaxAgeTooShort reports a --pod-pass-max-age that is not above
// podPassMaxAgeFloor while --pod-pass-every would reuse a pass. At or below
// the interval the last full pass is as old as the max age by the next
// tick, so only a tick the jitter brings early reuses it; between the
// interval and the floor some reuses work and the last ones depend on the
// jitter draws (and on the ticks' own run time, on top of the floor).
// Defaults must be applied (#228).
func podPassMaxAgeTooShort(cfg Config) bool {
	return cfg.PodPassEvery > 1 && cfg.PodPassMaxAge <= podPassMaxAgeFloor(cfg.Interval, cfg.PodPassEvery)
}

// applyDefaults fills zero values and rejects invalid combinations.
func (c *Config) applyDefaults() error {
	if c.Interval == 0 {
		c.Interval = 10 * time.Minute
	}
	if err := ValidateInterval(c.Interval); err != nil {
		return err
	}
	if c.CRName == "" {
		c.CRName = crd.DefaultName
	}
	if c.TeamLabel == "" {
		c.TeamLabel = "team"
	}
	if c.ForceSyncEvery == 0 {
		c.ForceSyncEvery = time.Hour
	}
	if err := ValidateForceSyncEvery(c.ForceSyncEvery); err != nil {
		return err
	}
	if c.ServerURL != "" && c.ServerTokenFile != "" {
		logger := c.Logger
		if logger == nil {
			logger = slog.Default()
		}
		f, err := secretfile.Open(c.ServerTokenFile, append([]secretfile.Option{
			secretfile.WithValidate(ValidateServerToken),
			secretfile.WithLogf(func(isError bool, format string, args ...any) {
				if isError {
					logger.Error(fmt.Sprintf(format, args...))
				} else {
					logger.Info(fmt.Sprintf(format, args...))
				}
			})}, c.secretOpts...)...)
		if err != nil {
			return fmt.Errorf("server token file: %w", err)
		}
		c.ServerToken, c.serverTokenFn = f.Value(), f.Value
	}
	if c.PodPassEvery == 0 {
		c.PodPassEvery = collect.DefaultPodPassEvery
	}
	if err := ValidatePodPassEvery(c.PodPassEvery); err != nil {
		return err
	}
	if c.PodPassMaxAge == 0 {
		c.PodPassMaxAge = collect.DefaultPodPassMaxAge
	}
	if err := ValidatePodPassMaxAge(c.PodPassMaxAge); err != nil {
		return err
	}
	if c.ServerURL != "" {
		if c.ServerToken == "" {
			return fmt.Errorf("server-url set but server-token empty (the ingest endpoint requires a bearer token)")
		}
		if err := ValidateServerURL(c.ServerURL); err != nil {
			return err
		}
		if err := ValidateServerToken(c.ServerToken); err != nil {
			return err
		}
	}
	// Normalize to MAJOR.MINOR: the CRD pins spec.targets items to that
	// form, so writing "v1.38" or "1.37.2" verbatim would be rejected with
	// 422 on every create and patch. It also keeps the per-tick comparison
	// with the stored spec stable.
	var targets []string
	for _, raw := range c.Targets {
		v, err := inventory.ParseTarget(raw)
		if err != nil {
			return fmt.Errorf("targets: %w", err)
		}
		if minor := fmt.Sprintf("%d.%d", v.Major, v.Minor); !slices.Contains(targets, minor) {
			targets = append(targets, minor)
		}
	}
	// spec.targets takes at most crd.MaxTargets: more would be rejected by
	// the apiserver on every tick.
	if len(targets) > crd.MaxTargets {
		return fmt.Errorf("targets: %d given, at most %d are evaluated (the ClusterReadiness schema allows no more)", len(targets), crd.MaxTargets)
	}
	c.Targets = targets
	return nil
}

// resolveTargets picks evaluation targets per tick: spec.Targets if any parse
// as targets (invalid entries are skipped with a note for
// status.notAssessed), else the next minor above the observed server version
// (vendor-suffixed GitVersions such as "v1.33.5-gke.1080000" included).
// Resolved per-tick because the spec can change at any time.
//
// At most crd.MaxTargets are returned, the first in spec order: every target
// is a row in status, and the schema's maxItems keeps new CRs within that.
// A CR written under an older schema may list more; the rest are counted
// in one note, not assessed. Notes for skipped entries are capped too
// (maxSkipNotes), so the notes do not grow with the spec either.
func resolveTargets(spec crd.Spec, inv inventory.Inventory) (targets []inventory.Version, notes []string, err error) {
	seen := map[inventory.Version]bool{}
	skipped, over := 0, 0
	firstOver := ""
	skip := func(note string) {
		if skipped < maxSkipNotes {
			notes = append(notes, note)
		}
		skipped++
	}
	for _, raw := range spec.Targets {
		v, perr := inventory.ParseTarget(raw)
		if perr != nil {
			skip(fmt.Sprintf("targets: skipped invalid spec target %q: %v", raw, perr))
			continue
		}
		// The CRD schema allows repeats; a second evaluation of the same
		// target would duplicate its status row and its metric series.
		if seen[v] {
			skip(fmt.Sprintf("targets: ignored duplicate spec target %q", raw))
			continue
		}
		seen[v] = true
		if len(targets) == crd.MaxTargets {
			if over == 0 {
				firstOver = raw
			}
			over++
			continue
		}
		targets = append(targets, v)
	}
	if skipped > maxSkipNotes {
		notes = append(notes, fmt.Sprintf("targets: %d more invalid or duplicate spec targets skipped", skipped-maxSkipNotes))
	}
	if over > 0 {
		notes = append(notes, fmt.Sprintf("targets: %d spec targets not assessed: at most %d are evaluated, the first in spec order (first left out: %q)",
			over, crd.MaxTargets, firstOver))
	}
	if len(targets) > 0 {
		return targets, notes, nil
	}
	if inv.ServerVersion == "" {
		return nil, notes, fmt.Errorf("targets: no spec targets and server version unknown")
	}
	observed, perr := inventory.ParseVersion(inv.ServerVersion)
	if perr != nil {
		return nil, notes, fmt.Errorf("targets: no spec targets and server version %q unparseable: set spec.targets explicitly", inv.ServerVersion)
	}
	return []inventory.Version{observed.Next()}, notes, nil
}

// runner holds per-process loop state. All clock and I/O seams are fields so
// tick is directly testable without timing dependence.
type runner struct {
	dyn    dynamic.Interface
	kb     kb.KB
	cfg    Config
	pusher *pusher // nil in CRD-only mode

	now       func() time.Time
	collectFn func(ctx context.Context) inventory.Inventory
	// tickBudget is the tick deadline runTick sets: tickTimeout(Interval).
	tickBudget time.Duration
	// ensureCRD, while not nil, brings the CRD up to date with this binary:
	// set when the check at startup failed, it runs every tick until it
	// succeeds.
	ensureCRD func(ctx context.Context) error

	lastHash string    // hash of the last successfully pushed inventory
	lastPush time.Time // when it was pushed

	last tickReport // the latest tick's outcome, for the observer

	// conformed is what the collector left out of the latest collection so
	// that the server would accept it (collect.Options.OnConform); tick logs
	// it with what its own Conform left out.
	conformed []string
	// collectorConformed is set by the latest collection when it was
	// collect.Collect, which conforms what it returns: the tick then does
	// not walk the whole inventory a second time (the push is checked once
	// more, by Admit). An injected collectFn leaves it false.
	collectorConformed bool
}

func newRunner(clients collect.Clients, dyn dynamic.Interface, k kb.KB, cfg Config) *runner {
	r := &runner{
		dyn: dyn,
		kb:  k,
		cfg: cfg,
		now: time.Now,

		tickBudget: tickTimeout(cfg.Interval),
	}
	// The caches outlive the ticks: a release already decoded is not fetched
	// again until its storage object changes (#71), API discovery is asked
	// again only under collect.DiscoveryCache's staleness rules (#228), and a
	// refused OCIRepository list only once an hour (#248).
	helmCache := collect.NewHelmCache()
	discoveryCache := collect.NewDiscoveryCache()
	gitopsCache := collect.NewGitOpsCache()
	// The pods outside kube-system are listed every PodPassEvery ticks, and
	// their add-on evidence reused in between (#228).
	podPass := collect.NewPodPassCache(cfg.PodPassEvery, cfg.PodPassMaxAge)
	r.collectFn = func(ctx context.Context) inventory.Inventory {
		inv := collect.Collect(ctx, clients, k, collect.Options{TeamLabel: cfg.TeamLabel, HelmCache: helmCache, HelmNamespaces: cfg.HelmNamespaces, DiscoveryCache: discoveryCache, GitOpsCache: gitopsCache, PodPass: podPass,
			OnConform: func(notes []string) { r.conformed = notes }})
		r.collectorConformed = true
		return inv
	}
	if cfg.ServerURL != "" {
		r.pusher = newPusher(cfg.ServerURL, cfg.ServerToken, cfg.ServerRootCAs)
		r.pusher.tokenFn = cfg.serverTokenFn
		r.pusher.log = cfg.Logger
	}
	return r
}

// tick is one loop iteration: collect → resolve targets → evaluate each →
// WriteStatus (always, even when the server is unreachable; only a spec
// that could not be read or reconciled stops it, the CR then marked as not
// current) → push on hash change or force interval, each phase on its own
// part of the tick deadline (maxTickReserve). Partial failures are joined
// and returned; the caller never stops the loop on a tick error. The
// outcome, with the push result kept apart from the tick's own errors, is
// left in r.last.
func (r *runner) tick(ctx context.Context) error {
	var errs []error
	r.last = tickReport{push: pushOff}
	ph := newTickPhases(ctx)
	cctx, cancel := ph.collect()
	r.conformed, r.collectorConformed = nil, false
	inv := r.collectFn(cctx)
	cancel()
	// collect.Collect leaves nothing the server would refuse; this holds
	// whatever the collector (collectFn is injectable), and makes the status
	// written and the snapshot pushed one inventory (#268). An injected
	// collector is conformed here; collect.Collect already did, so its
	// inventory is not walked a second time.
	// What was left out, by the collector (collect.Options.OnConform) or by
	// this, is said once per tick: the notes name no identifier (counts and
	// kinds only), so they are safe to log, and the capability reasons carry
	// the same for the report.
	notes := r.conformed
	if !r.collectorConformed {
		more, _ := inv.Conform()
		notes = append(notes, more...)
	}
	if len(notes) > 0 {
		log := r.cfg.Logger
		if log == nil {
			log = slog.Default()
		}
		log.Warn("inventory conformed: some data was left out so the server would accept the push", "notes", notes)
	}
	r.last.caps = inv.Capabilities

	// The ClusterReadiness calls share the status slice of the reserve;
	// the push, last, keeps the tick's own context.
	sctx, cancel := ph.status()
	errs = append(errs, r.writeStatus(sctx, ph, inv)...)
	cancel()
	r.last.err = errors.Join(errs...)

	if r.pusher != nil {
		pushed, err := r.maybePush(ctx, inv)
		switch {
		case err != nil:
			r.last.push, r.last.pushErr = pushFailed, err
			errs = append(errs, err)
		case pushed:
			r.last.push = pushOK
		default:
			r.last.push = pushUnchanged
		}
	}
	return errors.Join(errs...)
}

// writeStatus reads the ClusterReadiness spec, evaluates inv for its
// targets and writes the status, under ctx (the status slice of the tick
// reserve). It returns the tick's errors; a status write that succeeded
// but left an old marker in place is left in r.last.markerErr instead.
func (r *runner) writeStatus(ctx context.Context, ph tickPhases, inv inventory.Inventory) []error {
	var errs []error
	// An older schema the startup check could not upgrade prunes the
	// status fields it lacks from every write: try again (one GET when in
	// step), and say so in the status meanwhile.
	var crdNote string
	if r.ensureCRD != nil {
		// On its own part of the status slice: a check that hangs, or a
		// re-created CRD's wait for Established (up to 10s), must leave the
		// spec read and the status write after it their time.
		cctx, cancel := ph.crdCheck()
		err := r.ensureCRD(cctx)
		cancel()
		if err != nil {
			r.last.crdErr = err
			crdNote = "crd: the ClusterReadiness CRD could not be brought up to date with this agent, retried every tick " +
				"(status fields the installed schema lacks are dropped by the apiserver): " + oneLine(err, maxCRDNoteReason)
		} else {
			r.ensureCRD = nil
		}
	}
	// Read the spec, and the object the status is written over (one GET,
	// #228). The CR may have been deleted between ticks: recreate it, then
	// read it again. gen is the generation whose spec this tick evaluates; 0
	// (unknown) lets WriteStatus stamp the current one.
	spec, gen, obj, err := crd.ReadSpecObject(ctx, r.dyn, r.cfg.CRName)
	if err == nil && obj == nil {
		if cerr := crd.EnsureObject(ctx, r.dyn, r.cfg.CRName, r.cfg.Targets); cerr != nil {
			if errors.Is(cerr, crd.ErrCRDNotInstalled) {
				// --manage-crd=false, or the CRD vanished: say the fix where
				// /readyz and the tick log will carry it.
				cerr = fmt.Errorf("%w; %s", cerr, crd.InstallHint(AgentVersion))
			}
			errs = append(errs, cerr)
		}
		spec, gen, obj, err = crd.ReadSpecObject(ctx, r.dyn, r.cfg.CRName)
	}
	if err != nil {
		// No spec, no status: evaluating the default target instead, under
		// a generation the tick never read, would claim a spec it did not
		// evaluate (#238). The CR keeps its last status, marked as not
		// current; the next tick reads the spec again.
		return append(errs, r.statusNotWritten(ph, fmt.Errorf("status not written: %w", err))...)
	}
	if len(r.cfg.Targets) > 0 {
		if !slices.Equal(spec.Targets, r.cfg.Targets) {
			g, serr := crd.SetTargets(ctx, r.dyn, r.cfg.CRName, r.cfg.Targets)
			if serr != nil {
				// The stored spec still lists other targets, at the
				// generation the status would be stamped with.
				return append(errs, r.statusNotWritten(ph, fmt.Errorf("status not written: %w", serr))...)
			}
			gen, obj = g, nil // patched: the status write reads it again
		}
		spec.Targets = r.cfg.Targets
	}

	targets, notes, terr := resolveTargets(spec, inv)
	if crdNote != "" {
		notes = append([]string{crdNote}, notes...)
	}
	var st crd.Status
	if terr != nil {
		st = crd.Status{
			ObservedServerVersion: inv.ServerVersion,
			KBVersion:             r.kb.Version,
			LastEvaluated:         metav1.NewTime(r.now().UTC()),
			AgentVersion:          AgentVersion,
			NotAssessed:           append(notes, terr.Error()),
		}
		// Nothing was evaluated, so no verdict series exists for the
		// unknown-verdict alert; failing the tick is what surfaces it.
		errs = append(errs, terr)
	} else {
		// spec.ignore and object annotations apply per report; their
		// warnings (expired or invalid rules, deprecated annotation keys,
		// reason-less annotations) are mostly the same for every target,
		// so each is noted once.
		reports := make([]engine.Report, 0, len(targets))
		var ignoreNotes []string
		for _, target := range targets {
			report, warnings := suppress.Apply(engine.Evaluate(inv, r.kb, target, r.now()), spec.Ignore,
				suppress.Options{Now: r.now(), Source: ignoreSource})
			for _, w := range warnings {
				if !slices.Contains(ignoreNotes, w) {
					ignoreNotes = append(ignoreNotes, w)
				}
			}
			reports = append(reports, report)
		}
		st = crd.StatusFromReports(reports, inv.ServerVersion, AgentVersion, r.now())
		// WriteStatus keeps only the first maxNotAssessed entries, so
		// order is priority. Target-selection notes lead: "N targets not
		// assessed" must not be the one folded into "… and N more". The
		// report's capability gaps follow, which the Ready condition
		// sends readers here for. The ignore warnings come last: there
		// can be one per annotated object (a cluster moving from v0.1.x
		// has many), and they must not push a gap out of the list (#68).
		st.NotAssessed = slices.Concat(notes, st.NotAssessed, ignoreNotes)
		r.last.reports = reports
	}

	st.ObservedGeneration = gen
	if err := crd.WriteStatusOver(ctx, r.dyn, r.cfg.CRName, st, obj); errors.Is(err, crd.ErrStatusErrorNotCleared) {
		// The status is current; only the marker of an earlier failure
		// stayed. Not a failed tick, and not a reason to mark the CR again:
		// the next write retries the clearing.
		r.last.markerErr = err
	} else if err != nil {
		errs = append(errs, err)
		// The CR keeps the last verdict it was given: say on the object
		// itself that it is not current (#199). The next write clears it.
		if merr := r.markStatusError(ph, err); merr != nil {
			errs = append(errs, merr)
		}
	}
	return errs
}

// maxCRDNoteReason bounds the error quoted in the CRD note.
const maxCRDNoteReason = 240

// oneLine is err on one line, cut to at most n runes.
func oneLine(err error, n int) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}

// statusNotWritten marks the CR's status as not current, for a tick that
// could not write it because of cause.
func (r *runner) statusNotWritten(ph tickPhases, cause error) []error {
	errs := []error{cause}
	if merr := r.markStatusError(ph, cause); merr != nil {
		errs = append(errs, merr)
	}
	return errs
}

// markStatusError marks the CR's status as not current (#199) under the
// marker's own slice of the tick reserve, so a status write or spec read
// that ran out the status slice still leaves the marker time to land.
func (r *runner) markStatusError(ph tickPhases, cause error) error {
	ctx, cancel := ph.marker()
	defer cancel()
	return crd.MarkStatusError(ctx, r.dyn, r.cfg.CRName, cause, r.now())
}

// The tick reserve is the part of the tick deadline held back from
// collection for the work that follows it: 30s, or half the tick deadline
// when that is under a minute (an --interval under 2m: at the 1m minimum
// the deadline is 30s, so collection gets 15s and the reserve 15s). The
// reserve is carved, in order, into
//   - the status slice: the ClusterReadiness calls (the CRD check, the
//     spec read, the object's create and spec.targets patch, the status
//     write) end by the tick deadline - reserve/2. The CRD check, run when
//     the one at startup failed (runner.ensureCRD), ends 3*reserve/4
//     before the tick deadline, so neither a check that hangs nor a
//     re-created CRD's wait for Established leaves the calls after it less
//     than reserve/4. A CRD Established only later is found in step by
//     the next tick, which writes the status;
//   - the marker slice: the status-error marker gets reserve/4 of its own
//     from when it starts, so it ends by the tick deadline - reserve/4;
//   - the push, which runs until the tick deadline: at least reserve/4.
//
// Each slice is cut from the tick's context, never from the collection's,
// so a collection that runs out its time leaves each of them its own time
// (#238), while a stop (SIGTERM) still cancels them all.
const maxTickReserve = 30 * time.Second

// tickReserve is the reserve of a tick whose deadline is budget away.
func tickReserve(budget time.Duration) time.Duration {
	return min(maxTickReserve, budget/2)
}

// tickPhases carves one tick's deadline into the phase contexts (see
// maxTickReserve). A context without a deadline (a test calling tick
// directly) gives no phase a deadline of its own.
type tickPhases struct {
	ctx      context.Context
	deadline time.Time
	reserve  time.Duration
	bounded  bool
}

func newTickPhases(ctx context.Context) tickPhases {
	d, ok := ctx.Deadline()
	p := tickPhases{ctx: ctx, deadline: d, bounded: ok}
	if ok {
		p.reserve = tickReserve(time.Until(d))
	}
	return p
}

// endingAhead is the tick's context, ending ahead of the tick deadline.
func (p tickPhases) endingAhead(ahead time.Duration) (context.Context, context.CancelFunc) {
	if !p.bounded {
		return context.WithCancel(p.ctx)
	}
	return context.WithDeadline(p.ctx, p.deadline.Add(-ahead))
}

func (p tickPhases) collect() (context.Context, context.CancelFunc) {
	return p.endingAhead(p.reserve)
}

func (p tickPhases) status() (context.Context, context.CancelFunc) {
	return p.endingAhead(p.reserve / 2)
}

func (p tickPhases) crdCheck() (context.Context, context.CancelFunc) {
	return p.endingAhead(3 * p.reserve / 4)
}

func (p tickPhases) marker() (context.Context, context.CancelFunc) {
	if !p.bounded {
		return context.WithCancel(p.ctx)
	}
	return context.WithTimeout(p.ctx, p.reserve/4)
}

// runTick runs one tick under the tick deadline and records its duration.
func (r *runner) runTick(ctx context.Context) tickReport {
	ctx, cancel := context.WithTimeout(ctx, r.tickBudget)
	defer cancel()
	start := time.Now()
	_ = r.tick(ctx) // the report in r.last carries the errors, split by kind
	r.last.duration = time.Since(start)
	return r.last
}

// maybePush sends the snapshot iff its content hash changed since the last
// successful push, or ForceSyncEvery elapsed. Retry across ticks works via
// the hash gate, not the pusher's buffer: lastHash/lastPush only advance on
// success, so after a failed push the same content still differs from
// lastHash next tick and a fresh payload is offered (replacing any payload
// the pusher kept buffered) and flushed again. pushed reports whether a
// snapshot was sent.
func (r *runner) maybePush(ctx context.Context, inv inventory.Inventory) (pushed bool, err error) {
	hash, raw, err := snapshotHash(inv)
	if err != nil {
		return false, err
	}
	if hash == r.lastHash && r.now().Sub(r.lastPush) < r.cfg.ForceSyncEvery {
		return false, nil
	}
	// The server refuses (422) an inventory that is not admissible, for
	// good: the same content is offered every tick, as lastHash moves only
	// on success. Conform has left out what could be, so an inventory still
	// refused here has a value no repair mends; say so rather than send it.
	// (Admit works on a copy: it cuts free text in place, in maps shared with
	// inv, which is already cut.)
	if aerr := inv.Admit(); aerr != nil {
		return false, fmt.Errorf("push skipped: the server would refuse this inventory (422), so it is not sent: %w", aerr)
	}
	name := r.cfg.ClusterName
	if name == "" {
		name = inv.ClusterID
	}
	if name == "" {
		// The server refuses a snapshot without a name; offered again next
		// tick, as the hash gate has not moved.
		return false, errors.New("push skipped: no cluster name: --cluster-name is unset and the cluster UID " +
			"(the kube-system namespace's) could not be read (see status.notAssessed); set --cluster-name")
	}
	r.pusher.offer(pushPayload{
		SchemaVersion: 1,
		ClusterName:   name,
		AgentVersion:  AgentVersion,
		KBVersion:     r.kb.Version,
		Inventory:     raw,
	})
	if err := r.pusher.flush(ctx); err != nil {
		return false, err
	}
	r.lastHash, r.lastPush = hash, r.now()
	return true, nil
}

// snapshotHash returns (sha256 hex of canonical inventory JSON, wire JSON).
// Canonical form zeroes CollectedAt: the timestamp changes every tick and
// hashing it would defeat content dedup entirely. It zeroes
// APIServerStartTime too: it says which apiserver answered the /metrics
// scrape, and with HA apiservers that changes whenever the connection
// moves; and AddOnEvidenceAgeSeconds, which grows with every tick that
// reuses a pod pass (#228). That is inventory.Canonical, which the server
// uses for its duplicate detection too.
func snapshotHash(inv inventory.Inventory) (hash string, raw []byte, err error) {
	raw, err = json.Marshal(inv)
	if err != nil {
		return "", nil, fmt.Errorf("marshal inventory: %w", err)
	}
	canon, err := json.Marshal(inv.Canonical())
	if err != nil {
		return "", nil, fmt.Errorf("marshal canonical inventory: %w", err)
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), raw, nil
}

// Run executes the continuous loop until ctx is canceled (returns nil — a
// cancel is a graceful stop, not an error). The first tick runs immediately;
// later ticks fire every Interval ±10% jitter, each under tickTimeout. Every
// tick logs one line and updates the metrics; tick errors are never fatal:
// the loop never dies on a tick error. With HealthAddr set, /healthz,
// /readyz and /metrics are served until Run returns.
func Run(ctx context.Context, clients collect.Clients, dyn dynamic.Interface, apiext apiextensionsclient.Interface, k kb.KB, cfg Config) error {
	if err := cfg.applyDefaults(); err != nil {
		return err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.ServerURL != "" && cfg.ForceSyncEvery <= cfg.Interval {
		asked := cfg.ForceSyncEvery
		cfg.ForceSyncEvery = forceSyncInEffect(asked, cfg.Interval)
		log.Warn("force-sync-every is at or below the interval: an unchanged inventory is pushed every tick, as the agent pushes at most once a tick",
			"forceSyncEvery", asked.String(), "interval", cfg.Interval.String(), "inEffect", cfg.ForceSyncEvery.String())
	}
	if podPassMaxAgeTooShort(cfg) {
		floor := podPassMaxAgeFloor(cfg.Interval, cfg.PodPassEvery)
		log.Warn("pod-pass-max-age is too short for pod-pass-every: the ticks after a full pass can be as far as 1.1 times the interval apart, plus the time a tick takes, so a pass is not reliably reused for pod-pass-every minus one of them (not at all at or below the interval, but by a tick the jitter brings early) and the pods outside kube-system are listed on ticks that pod-pass-every would reuse; raise pod-pass-max-age above mustExceed plus the run time of the ticks in between (a few seconds each at 2,001 nodes), or lower pod-pass-every to podPassEveryThatFits (less if ticks are slow; 1 lists every pod on every tick, which also ends this warning)",
			"podPassMaxAge", cfg.PodPassMaxAge.String(), "interval", cfg.Interval.String(), "podPassEvery", cfg.PodPassEvery,
			"mustExceed", floor.String(), "podPassEveryThatFits", podPassEveryThatFits(cfg.Interval, cfg.PodPassMaxAge))
	}
	obs := newObserver(log, k, cfg.Interval)
	healthAddr := ""
	if cfg.HealthAddr != "" {
		// Bound before anything else so the probes answer during startup;
		// a taken port fails the start instead of leaving probes dark.
		ln, err := net.Listen("tcp", cfg.HealthAddr)
		if err != nil {
			return fmt.Errorf("health listener: %w", err)
		}
		healthAddr = ln.Addr().String()
		srv := &http.Server{Handler: obs.handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				log.Error("health listener stopped", "err", err)
			}
		}()
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()
	}
	pprofAddr := ""
	if cfg.PprofAddr != "" {
		if err := ValidatePprofAddr(cfg.PprofAddr); err != nil {
			return err
		}
		ln, err := net.Listen("tcp", cfg.PprofAddr)
		if err != nil {
			return fmt.Errorf("pprof listener: %w", err)
		}
		pprofAddr = ln.Addr().String()
		// No WriteTimeout: a CPU profile is written after its seconds.
		srv := &http.Server{Handler: pprofHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				log.Error("pprof listener stopped", "err", err)
			}
		}()
		defer func() { _ = srv.Close() }()
	}
	server := cfg.ServerURL
	if server == "" {
		server = "off (CRD-only)"
	}
	log.Info(msgStarting, "version", AgentVersion, "kbVersion", k.Version, "maxKnownK8s", k.MaxKnownK8s.String(),
		"interval", cfg.Interval.String(), "tickTimeout", tickTimeout(cfg.Interval).String(),
		"tickReserve", tickReserve(tickTimeout(cfg.Interval)).String(),
		"podPassEvery", cfg.PodPassEvery, "podPassMaxAge", cfg.PodPassMaxAge.String(),
		"crName", cfg.CRName, "server", server, "healthAddr", healthAddr, "pprofAddr", pprofAddr)

	r := newRunner(clients, dyn, k, cfg)
	if !cfg.SkipCRDManagement {
		ensure := func(ctx context.Context) error { return crd.EnsureCRD(ctx, apiext) }
		// Bounded: an apiserver that hangs at pod start must not hold the
		// first tick back. A check that times out is retried every tick.
		sctx, scancel := context.WithTimeout(ctx, startupCRDTimeout)
		err := ensure(sctx)
		// The group moved (#68): look for the old CRD, which only reads.
		// The check shares the startup bound.
		legacy, lerr := crd.LegacyCRDInstalled(sctx, apiext)
		scancel()
		if lerr != nil {
			log.Info("could not check for the pre-v0.2.0 ClusterReadiness CRD", "crd", crd.LegacyCRDName, "err", lerr)
		}
		if errors.Is(err, crd.ErrCRDNotInstalled) {
			// Every tick would 404; say why once, clearly. With only the
			// old CRD installed, the cause is a chart upgraded across the
			// group move: Helm does not install crds/ on upgrade.
			if legacy {
				return fmt.Errorf("%w; only %s, on the old group, is installed: the API group moved to %s and helm upgrade does not install the new CRD, so %s (the other steps: %s)",
					err, crd.LegacyCRDName, crd.Group, crd.InstallHint(AgentVersion), crd.UpgradeGuideURL)
			}
			return fmt.Errorf("%w; Helm installs crds/ on first install only, so %s", err, crd.InstallHint(AgentVersion))
		}
		if legacy {
			// Say once that the old CRD can go, now that the new one is
			// in place. Its objects are the owner's to delete, so the
			// agent never does.
			log.Warn(msgLegacyCRD, "crd", crd.LegacyCRDName, "group", crd.Group, "cleanup", crd.LegacyCRDCleanup)
		}
		if err != nil {
			// Non-fatal otherwise: the CRD exists, the schema upgrade did
			// not land (a transient fault, or a narrower custom role that
			// denies patch). Every tick tries again until it succeeds.
			log.Warn("could not bring the ClusterReadiness CRD up to date; continuing with the installed schema, retried every tick", "err", err)
			r.ensureCRD = ensure
		}
	}
	for {
		rep := r.runTick(ctx)
		if ctx.Err() != nil && (rep.err != nil || rep.pushErr != nil) {
			// A stop cancelled this tick's calls; that is no tick failure.
			log.Info(msgStopping, "interruptedTick", true)
			return nil
		}
		obs.record(rep)
		timer := time.NewTimer(jitter(cfg.Interval))
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Info(msgStopping)
			return nil
		case <-timer.C:
		}
	}
}

// jitter returns d ±10% (from minTickSpacing(d) to 11/10 of d), so the
// second and later ticks of agents started at the same moment drift apart
// instead of reaching the upgradescope server together. The first tick is
// not jittered: it runs at once so the pod is Ready, and
// `helm install --wait` gets an answer, as soon as possible. Agents
// started together therefore push together; their push retries are
// jittered (retryDelay), so a busy server's 503s do not keep them aligned.
func jitter(d time.Duration) time.Duration {
	return jitterBy(d, rand.Int64N(int64(d/5)+1))
}

// jitterBy is jitter for a draw n in [0, d/5]. Integer arithmetic keeps
// the floor exact: float64(d)*0.9 can land a nanosecond under it.
func jitterBy(d time.Duration, n int64) time.Duration {
	return minTickSpacing(d) + time.Duration(n)
}
