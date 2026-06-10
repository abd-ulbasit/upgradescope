// Package agent runs the in-cluster continuous loop: collect → evaluate per
// target → ClusterReadiness CRD status (always) → push snapshot to the server
// on content change. The agent's local value never depends on server
// availability (spec §3).
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// AgentVersion is stamped into CRD status and push envelopes. The CLI sets it
// from the build version; "dev" otherwise.
var AgentVersion = "dev"

type Config struct {
	Interval       time.Duration // default 10m, min 1m
	ServerURL      string        // optional; "" = CRD-only mode
	ServerToken    string        // bearer for push
	ClusterName    string        // human label sent to server; default = ClusterID
	CRName         string        // default crd.DefaultName
	TeamLabel      string        // default "team"
	ForceSyncEvery time.Duration // default 1h: push even if hash unchanged
}

// applyDefaults fills zero values and rejects invalid combinations.
func (c *Config) applyDefaults() error {
	if c.Interval == 0 {
		c.Interval = 10 * time.Minute
	}
	if c.Interval < time.Minute {
		return fmt.Errorf("interval %s below minimum 1m", c.Interval)
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
	return nil
}

// resolveTargets picks evaluation targets per tick: spec.Targets if any parse
// (invalid entries are skipped with a note for status.notAssessed), else the
// next minor above the observed server version. Resolved per-tick because the
// spec can change at any time.
func resolveTargets(spec crd.Spec, inv inventory.Inventory) (targets []inventory.Version, notes []string, err error) {
	for _, raw := range spec.Targets {
		v, perr := inventory.ParseVersion(raw)
		if perr != nil {
			notes = append(notes, fmt.Sprintf("targets: skipped invalid spec target %q", raw))
			continue
		}
		targets = append(targets, v)
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

	lastHash string    // hash of the last successfully pushed inventory
	lastPush time.Time // when it was pushed
	tickErrs int       // failed ticks, for log context only
}

func newRunner(clients collect.Clients, dyn dynamic.Interface, k kb.KB, cfg Config) *runner {
	r := &runner{
		dyn: dyn,
		kb:  k,
		cfg: cfg,
		now: time.Now,
	}
	r.collectFn = func(ctx context.Context) inventory.Inventory {
		return collect.Collect(ctx, clients, k, collect.Options{TeamLabel: cfg.TeamLabel})
	}
	if cfg.ServerURL != "" {
		r.pusher = newPusher(cfg.ServerURL, cfg.ServerToken)
	}
	return r
}

// tick is one loop iteration: collect → resolve targets → evaluate each →
// WriteStatus (always, even when the server is unreachable) → push on hash
// change or force interval. Partial failures are joined and returned for
// logging; the caller never stops the loop on a tick error.
func (r *runner) tick(ctx context.Context) error {
	var errs []error
	inv := r.collectFn(ctx)

	// The CR may have been deleted between ticks; recreate, then read spec.
	if err := crd.EnsureObject(ctx, r.dyn, r.cfg.CRName); err != nil {
		errs = append(errs, err)
	}
	spec, _, err := crd.ReadSpec(ctx, r.dyn, r.cfg.CRName)
	if err != nil {
		errs = append(errs, err)
	}

	targets, notes, terr := resolveTargets(spec, inv)
	var st crd.Status
	if terr != nil {
		st = crd.Status{
			ObservedServerVersion: inv.ServerVersion,
			KBVersion:             r.kb.Version,
			LastEvaluated:         metav1.NewTime(r.now().UTC()),
			AgentVersion:          AgentVersion,
			NotAssessed:           append(notes, terr.Error()),
		}
	} else {
		reports := make([]engine.Report, 0, len(targets))
		for _, target := range targets {
			reports = append(reports, engine.Evaluate(inv, r.kb, target, r.now()))
		}
		st = crd.StatusFromReports(reports, inv.ServerVersion, AgentVersion, r.now())
		st.NotAssessed = append(st.NotAssessed, notes...)
	}

	if err := crd.WriteStatus(ctx, r.dyn, r.cfg.CRName, st); err != nil {
		errs = append(errs, err)
	}

	if r.pusher != nil {
		if err := r.maybePush(ctx, inv); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// maybePush sends the snapshot iff its content hash changed since the last
// successful push, or ForceSyncEvery elapsed. lastHash/lastPush only advance
// on success, so a failed push is naturally re-offered next tick.
func (r *runner) maybePush(ctx context.Context, inv inventory.Inventory) error {
	hash, raw, err := snapshotHash(inv)
	if err != nil {
		return err
	}
	if hash == r.lastHash && r.now().Sub(r.lastPush) < r.cfg.ForceSyncEvery {
		return nil
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
		return err
	}
	r.lastHash, r.lastPush = hash, r.now()
	return nil
}

// snapshotHash returns (sha256 hex of canonical inventory JSON, wire JSON).
// Canonical form zeroes CollectedAt: the timestamp changes every tick and
// hashing it would defeat content dedup entirely. The server must use the
// same canonicalization for its duplicate detection.
func snapshotHash(inv inventory.Inventory) (hash string, raw []byte, err error) {
	raw, err = json.Marshal(inv)
	if err != nil {
		return "", nil, fmt.Errorf("marshal inventory: %w", err)
	}
	stable := inv
	stable.CollectedAt = time.Time{}
	canon, err := json.Marshal(stable)
	if err != nil {
		return "", nil, fmt.Errorf("marshal canonical inventory: %w", err)
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), raw, nil
}

// Run executes the continuous loop until ctx is canceled (returns nil — a
// cancel is a graceful stop, not an error). The first tick runs immediately;
// later ticks fire every Interval ±10% jitter. Tick errors are logged and
// counted, never fatal: the loop never dies on a tick error.
func Run(ctx context.Context, clients collect.Clients, dyn dynamic.Interface, apiext apiextensionsclient.Interface, k kb.KB, cfg Config) error {
	if err := cfg.applyDefaults(); err != nil {
		return err
	}
	if err := crd.EnsureCRD(ctx, apiext); err != nil {
		// Non-fatal: the Helm chart installs the CRD via crds/, and RBAC may
		// deny apiextensions writes. WriteStatus failures will surface loudly
		// per tick if the CRD is truly absent.
		slog.Warn("ensure ClusterReadiness CRD failed; assuming it is pre-installed", "err", err)
	}
	r := newRunner(clients, dyn, k, cfg)
	for {
		if err := r.tick(ctx); err != nil {
			r.tickErrs++
			slog.Error("tick failed", "err", err, "consecutiveInfo", r.tickErrs)
		} else {
			r.tickErrs = 0
		}
		timer := time.NewTimer(jitter(cfg.Interval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// jitter returns d ±10%, so a fleet of agents installed at the same moment
// does not thundering-herd the apiserver and the upgradescope server.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
}
