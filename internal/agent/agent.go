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
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
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
	Interval       time.Duration // default 10m, min 1m
	ServerURL      string        // optional; "" = CRD-only mode
	ServerToken    string        // bearer for push
	ClusterName    string        // human label sent to server; default = ClusterID
	CRName         string        // default crd.DefaultName
	TeamLabel      string        // default "team"
	ForceSyncEvery time.Duration // default 1h: push even if hash unchanged
	// ServerRootCAs verifies the server's certificate on pushes (from
	// LoadServerCAs: the system roots plus a private CA); nil = the system
	// roots.
	ServerRootCAs *x509.CertPool
	// Targets, when non-empty, are the source of truth for spec.targets:
	// every tick reconciles the CR to them, overriding kubectl edits. Empty
	// leaves spec.targets to whoever edits the CR. applyDefaults normalizes
	// each to MAJOR.MINOR ("v1.38" → "1.38", "1.37.2" → "1.37").
	Targets []string
	// SkipCRDManagement leaves the CRD alone entirely (--manage-crd=false):
	// no read, no schema upgrade. The default keeps it in step with the
	// embedded manifest at startup.
	SkipCRDManagement bool
	// HealthAddr is where /healthz, /readyz and /metrics listen, e.g.
	// ":8081"; "" serves none of them.
	HealthAddr string
	// Logger receives the startup line and one line per tick; nil uses
	// slog.Default().
	Logger *slog.Logger
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
	if c.ServerURL != "" && c.ServerToken == "" {
		return fmt.Errorf("server-url set but server-token empty (the ingest endpoint requires a bearer token)")
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
			skip(fmt.Sprintf("targets: skipped invalid spec target %q", raw))
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
	// again until its storage object changes (#71), and API discovery is
	// asked again only under collect.DiscoveryCache's staleness rules (#228).
	helmCache := collect.NewHelmCache()
	discoveryCache := collect.NewDiscoveryCache()
	r.collectFn = func(ctx context.Context) inventory.Inventory {
		return collect.Collect(ctx, clients, k, collect.Options{TeamLabel: cfg.TeamLabel, HelmCache: helmCache, DiscoveryCache: discoveryCache})
	}
	if cfg.ServerURL != "" {
		r.pusher = newPusher(cfg.ServerURL, cfg.ServerToken, cfg.ServerRootCAs)
		r.pusher.log = cfg.Logger
	}
	return r
}

// tick is one loop iteration: collect → resolve targets → evaluate each →
// WriteStatus (always, even when the server is unreachable; only a spec
// that could not be read or reconciled stops it, the CR then marked as not
// current) → push on hash change or force interval, each phase on its own
// part of the tick deadline (maxTickReserve). Partial failures are joined
// and returned; the
// caller never stops the loop on a tick error. The outcome, with the push
// result kept apart from the tick's own errors, is left in r.last.
func (r *runner) tick(ctx context.Context) error {
	var errs []error
	r.last = tickReport{push: pushOff}
	ph := newTickPhases(ctx)
	cctx, cancel := ph.collect()
	inv := r.collectFn(cctx)
	cancel()
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
		if err := r.ensureCRD(ctx); err != nil {
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
				return r.statusNotWritten(ph, fmt.Errorf("status not written: %w", serr))
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
		// warnings (expired or invalid rules, reason-less annotations) are
		// the same for every target, so each is noted once.
		reports := make([]engine.Report, 0, len(targets))
		for _, target := range targets {
			report, warnings := suppress.Apply(engine.Evaluate(inv, r.kb, target, r.now()), spec.Ignore,
				suppress.Options{Now: r.now(), Source: ignoreSource})
			for _, w := range warnings {
				if !slices.Contains(notes, w) {
					notes = append(notes, w)
				}
			}
			reports = append(reports, report)
		}
		st = crd.StatusFromReports(reports, inv.ServerVersion, AgentVersion, r.now())
		// Target-selection notes lead: WriteStatus keeps only the first
		// maxNotAssessed entries, and "N targets not assessed" must not be
		// the one folded into "… and N more".
		st.NotAssessed = append(notes, st.NotAssessed...)
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
//     write) end by the tick deadline - reserve/2;
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
	name := r.cfg.ClusterName
	if name == "" {
		name = inv.ClusterID
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
// moves. The server must use the same canonicalization for its duplicate
// detection.
func snapshotHash(inv inventory.Inventory) (hash string, raw []byte, err error) {
	raw, err = json.Marshal(inv)
	if err != nil {
		return "", nil, fmt.Errorf("marshal inventory: %w", err)
	}
	stable := inv
	stable.CollectedAt, stable.APIServerStartTime = time.Time{}, time.Time{}
	canon, err := json.Marshal(stable)
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
	server := cfg.ServerURL
	if server == "" {
		server = "off (CRD-only)"
	}
	log.Info(msgStarting, "version", AgentVersion, "kbVersion", k.Version, "maxKnownK8s", k.MaxKnownK8s.String(),
		"interval", cfg.Interval.String(), "tickTimeout", tickTimeout(cfg.Interval).String(),
		"tickReserve", tickReserve(tickTimeout(cfg.Interval)).String(),
		"crName", cfg.CRName, "server", server, "healthAddr", healthAddr)

	r := newRunner(clients, dyn, k, cfg)
	if !cfg.SkipCRDManagement {
		ensure := func(ctx context.Context) error { return crd.EnsureCRD(ctx, apiext) }
		if err := ensure(ctx); err != nil {
			if errors.Is(err, crd.ErrCRDNotInstalled) {
				return err // every tick would 404; say why once, clearly
			}
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

// jitter returns d ±10%, so the second and later ticks of agents started at
// the same moment drift apart instead of reaching the upgradescope server
// together. The first tick is not jittered: it runs at once so the pod is
// Ready, and `helm install --wait` gets an answer, as soon as possible.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
}
