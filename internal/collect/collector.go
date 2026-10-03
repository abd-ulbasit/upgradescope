// Package collect builds an inventory.Inventory from a live cluster (or,
// in files mode, from rendered manifests). Sub-collectors degrade
// independently: an error marks the capability unavailable with a reason
// instead of failing the whole collection (spec §9).
package collect

import (
	"context"
	"errors"
	"fmt"
	"time"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// Clients bundles the API clients the live-cluster sub-collectors need.
// Any nil client degrades the capabilities that depend on it.
type Clients struct {
	Kube       kubernetes.Interface
	Metadata   metadata.Interface
	Discovery  discovery.DiscoveryInterface
	RESTClient rest.Interface
	// APIExtensions reads CustomResourceDefinitions (the crds capability).
	APIExtensions apiextensionsclient.Interface
	// Dynamic reads the Argo CD and Flux custom resources that name the
	// charts they deploy (the helm capability). Nil: they are not read.
	Dynamic dynamic.Interface
}

// Options tunes collection behavior.
type Options struct {
	TeamLabel string // namespace label used for team attribution; default "team"
}

// listPageSize bounds every cluster-wide list call: large clusters must
// never be read in one unbounded request.
const listPageSize = 500

// clientQPS and clientBurst replace client-go's client-side rate limit
// (5 QPS, burst 10) when the caller sets none: the Helm collector makes
// one GET per release, which at 5 QPS adds a minute for 300 releases.
// These are kubectl's defaults; apiserver priority and fairness still
// protects the server.
const (
	clientQPS   = 50
	clientBurst = 300
)

// step is one independently-degradable sub-collector bound to a capability.
type step struct {
	cap inventory.Capability
	run func(ctx context.Context, inv *inventory.Inventory) error
}

// Collect builds an Inventory from a live cluster. It never returns an
// error: each sub-collector failure becomes Capabilities[cap] =
// {Available: false, Reason: err.Error()} and collection continues. Each
// sub-collector runs under its own share of ctx's deadline (see runSteps),
// so one stalled step cannot starve the others.
func Collect(ctx context.Context, c Clients, k kb.KB, opts Options) inventory.Inventory {
	if opts.TeamLabel == "" {
		opts.TeamLabel = "team"
	}
	inv := inventory.Inventory{
		SchemaVersion:   1,
		CollectorSchema: inventory.CurrentCollectorSchema,
		Source:          inventory.SourceCluster,
		CollectedAt:     time.Now().UTC(),
		Capabilities:    map[inventory.Capability]inventory.CapabilityStatus{},
	}
	runSteps(ctx, &inv, steps(c, k, opts))
	return inv
}

// partialError is the outcome of a step that produced usable data and has
// something to say about it. runSteps keeps the capability available and
// surfaces msg as the Reason. With incomplete set, some of what the step
// covers was not read (one forbidden resource among many): the capability
// is marked Partial, and skipped names what went unread (see
// inventory.CapabilityStatus.Skipped). Without it the reason is
// informational and the data complete (helm's per-driver release counts).
type partialError struct {
	msg        string
	incomplete bool
	skipped    []string
}

func (e partialError) Error() string { return e.msg }

// runSteps runs ss in order, each under its own deadline: an equal share
// of the time left before ctx's deadline (none when ctx has none), the
// last step getting all that remains. A stalled step then leaves every
// later step at least the scan budget divided by the number of steps, and
// degrades only its own capability, its reason naming the step deadline.
func runSteps(ctx context.Context, inv *inventory.Inventory, ss []step) {
	for i, s := range ss {
		sctx, cancel, share := stepContext(ctx, len(ss)-i)
		err := s.run(sctx, inv)
		var note string
		if ctx.Err() == nil && errors.Is(sctx.Err(), context.DeadlineExceeded) {
			note = fmt.Sprintf(" (step deadline: gave up after %s, this step's share of the scan's time)", roundShare(share))
		}
		cancel()
		var pe partialError
		switch {
		case err == nil:
			inv.Capabilities[s.cap] = inventory.CapabilityStatus{Available: true}
		case errors.As(err, &pe):
			inv.Capabilities[s.cap] = inventory.CapabilityStatus{Available: true, Reason: pe.Error() + note,
				Partial: pe.incomplete, Skipped: pe.skipped}
		default:
			inv.Capabilities[s.cap] = inventory.CapabilityStatus{Available: false, Reason: err.Error() + note}
		}
	}
	// A reason joins one failure per resource a step could not read, and
	// object refs carry their ignore annotations whole: cut both to the
	// inventory limits, which the server refuses a push beyond.
	inv.CutFreeText()
}

// stepContext derives a step's context from the scan's: an equal share of
// the time left before ctx's deadline among the left steps still to run,
// this one included; no limit of its own when ctx has no deadline.
func stepContext(ctx context.Context, left int) (context.Context, context.CancelFunc, time.Duration) {
	deadline, ok := ctx.Deadline()
	if !ok {
		sctx, cancel := context.WithCancel(ctx)
		return sctx, cancel, 0
	}
	share := time.Until(deadline) / time.Duration(left)
	sctx, cancel := context.WithTimeout(ctx, share)
	return sctx, cancel, share
}

// roundShare rounds a step's share of the scan budget for its reason.
func roundShare(d time.Duration) time.Duration {
	if d >= time.Second {
		return d.Round(time.Second)
	}
	return d.Round(time.Millisecond)
}

// steps lists the live sub-collectors in execution order: helm before
// addons (the add-on matcher consumes inv.HelmReleases), api-usage before
// deprecated-calls (which needs the deprecated endpoints api-usage listed
// itself, and must see their metric rows on every scan alike). crds lists
// custom resources only at versions that are not deprecated, so it adds
// no metric rows; the /metrics scrape stays last.
func steps(c Clients, k kb.KB, opts Options) []step {
	var selfListed []string // api-usage's own deprecated LISTs
	return []step{
		{cap: inventory.CapVersions, run: func(ctx context.Context, inv *inventory.Inventory) error {
			if c.Kube == nil || c.Discovery == nil {
				return errors.New("kubernetes client not configured")
			}
			return collectVersions(ctx, c.Discovery, c.Kube, opts.TeamLabel, inv)
		}},
		{cap: inventory.CapHelm, run: func(ctx context.Context, inv *inventory.Inventory) error {
			if c.Kube == nil || c.Metadata == nil {
				return errors.New("kubernetes/metadata client not configured")
			}
			return collectHelmStep(ctx, c, k.APILifecycle, inv)
		}},
		{cap: inventory.CapAddOns, run: func(ctx context.Context, inv *inventory.Inventory) error { // after helm: consumes inv.HelmReleases
			if c.Kube == nil {
				return errors.New("kubernetes client not configured")
			}
			return collectAddOns(ctx, c.Kube, k.AddOns, inv)
		}},
		{cap: inventory.CapAPIUsage, run: func(ctx context.Context, inv *inventory.Inventory) error {
			if c.Discovery == nil || c.Metadata == nil {
				return errors.New("discovery/metadata client not configured")
			}
			var err error
			selfListed, err = collectAPIUsage(ctx, c.Discovery, c.Metadata, k.APILifecycle, inv)
			return err
		}},
		{cap: inventory.CapCRDs, run: func(ctx context.Context, inv *inventory.Inventory) error {
			if c.APIExtensions == nil || c.Metadata == nil {
				return errors.New("apiextensions/metadata client not configured")
			}
			return collectCRDs(ctx, c.APIExtensions, c.Metadata, inv)
		}},
		{cap: inventory.CapDeprecatedCalls, run: func(ctx context.Context, inv *inventory.Inventory) error { // after api-usage: consumes selfListed
			if c.RESTClient == nil {
				return errors.New("rest client not configured")
			}
			return collectDeprecatedCalls(ctx, c.RESTClient, selfListed, inv)
		}},
	}
}

// NewClients builds the concrete client set from a rest.Config.
// The sole construction point — everything else consumes the interfaces.
//
// API warning headers are discarded: client-go's default handler prints
// each one to stderr as a klog line, above the report and in agent logs,
// and the deprecations they announce are already findings. Without a
// caller-set rate limit, clientQPS/clientBurst apply. The caller's cfg is
// not modified.
func NewClients(cfg *rest.Config) (Clients, error) {
	cfg = rest.CopyConfig(cfg)
	cfg.WarningHandlerWithContext = rest.NoWarnings{}
	if cfg.QPS == 0 && cfg.RateLimiter == nil {
		cfg.QPS, cfg.Burst = clientQPS, clientBurst
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return Clients{}, fmt.Errorf("build kubernetes client: %w", err)
	}
	md, err := metadata.NewForConfig(cfg)
	if err != nil {
		return Clients{}, fmt.Errorf("build metadata client: %w", err)
	}
	ext, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		return Clients{}, fmt.Errorf("build apiextensions client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return Clients{}, fmt.Errorf("build dynamic client: %w", err)
	}
	return Clients{
		Kube:          kube,
		Metadata:      md,
		Discovery:     kube.Discovery(),
		RESTClient:    kube.CoreV1().RESTClient(),
		APIExtensions: ext,
		Dynamic:       dyn,
	}, nil
}
