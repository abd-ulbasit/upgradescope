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
	// HelmCache, when set, remembers the Helm releases an earlier Collect
	// decoded, so this one fetches only the storage objects that are new or
	// changed instead of one per release. A long-running caller (the agent)
	// keeps one across calls; a one-shot scan leaves it nil.
	HelmCache *HelmCache
	// DiscoveryCache, when set, keeps API discovery across calls under the
	// staleness rules DiscoveryCache states, so a steady tick does not ask
	// for it again. The agent keeps one; a one-shot scan leaves it nil.
	DiscoveryCache *DiscoveryCache
	// GitOpsCache, when set, remembers the OCIRepository lists the
	// apiserver refused, so a role without list on them is not asked again
	// on every call, only once ForbiddenListRecheck has passed. The agent
	// keeps one; a one-shot scan leaves it nil.
	GitOpsCache *GitOpsCache
}

// listPageSize bounds every cluster-wide list call: large clusters must
// never be read in one unbounded request.
const listPageSize = 500

// The pod and node lists, which a large cluster's tick spends most of its
// requests on (#228: at 2,001 nodes and 14,000 pods, 500-object pages were
// 35 of a steady tick's 53 requests), are paged by size rather than by
// count alone: the first page is listPageSize objects, and each later one
// as many as fit wholePageBytes at the size of the LARGEST object of the
// page before (pageLimit), between listPageSize and podPageSize or
// nodePageSize. A page is what one request holds: client-go reads its
// response whole and decodes it whole, about three times its encoded size
// in live heap.
//
// No limit set before a page is read can bound its bytes: the objects it
// will hold are unseen. So the worst case of a page is its limit times the
// largest object, and the most a page may hold is what bounds it: 1,000,
// twice listPageSize, so no page holds more than twice the objects a page
// held before #228. It happens when small objects are followed by large
// ones (pods are listed by namespace, so a namespace of small pods before
// one of large pods): 500 pods of 137 bytes, then 1,000 of up to 41,685
// bytes (39.4 MiB encoded), measured 122.3 to 125.3 MiB of live heap on
// GitHub's linux/amd64 runner (TestPodPagePeakHeapIsBounded, which allows
// 144 MiB), under half the chart's 256Mi; at the 2,000 a page of an
// earlier draft, 249.4 to 250.0 MiB (Apple M1 Pro), past the agent's
// GOMEMLIMIT. Objects as large as those of the page before (a run of such
// pods) are read 500 a page, 61.7 to 68.0 MiB, as before. Small objects
// (the scale lab's KWOK pods and nodes, about 3 KiB each) are read 1,000 a
// page, a production cluster's pods of about 8 KiB some 990, and anything
// of 16,744 bytes or more (a Node listing many images) 500, as before;
// between 16 KiB and that, 501 to 511. One page past the agent's
// GOMEMLIMIT (90% of 256Mi) beside the rest of the agent takes 1,000 pods
// of about 60 KiB after a page of small ones, at the 3.2 bytes of live
// heap per encoded byte measured above, where pages of 500 took about 120
// KiB (computed; docs/claims.md PF-02).
const (
	wholePageBytes = 8 << 20
	podPageSize    = 1000
	nodePageSize   = 1000
)

// pageLimit is the limit of the page after one whose largest object
// encodes to largest bytes (a Pod's or Node's Size, its protobuf
// encoding): as many as fit wholePageBytes at that size, at least
// listPageSize and at most most. An empty page (largest 0) says nothing.
func pageLimit(largest int, most int64) int64 {
	if largest <= 0 {
		return listPageSize
	}
	return max(listPageSize, min(most, wholePageBytes/int64(largest)))
}

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
	opts.DiscoveryCache.begin()
	c.Discovery = opts.DiscoveryCache.client(c.Discovery)
	runSteps(ctx, &inv, steps(c, k, opts))
	opts.DiscoveryCache.end(&inv)
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
// The last step's deadline is ctx's own, so ctx has expired with it: the
// note is left out only when ctx was cancelled (a stop), not when its
// deadline passed (#238).
func runSteps(ctx context.Context, inv *inventory.Inventory, ss []step) {
	for i, s := range ss {
		sctx, cancel, share := stepContext(ctx, len(ss)-i)
		err := s.run(sctx, inv)
		var note string
		if !errors.Is(ctx.Err(), context.Canceled) && errors.Is(sctx.Err(), context.DeadlineExceeded) {
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
	// inventory limits, which the server refuses a push beyond. What is
	// beyond them and cannot be cut (a Helm release or a manifest object
	// whose name is no identifier, an image repository of 17 KiB) is left
	// out and named in its capability (#268): the server refuses a whole
	// inventory for one such value, and the agent would offer the same one
	// every tick. What no repair can mend is left, and the agent, which
	// checks again before it pushes, does not send it.
	_, _ = inv.Conform()
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
	// api-usage's own deprecated LISTs; until it has run, unknown wherever
	// the scanner could list one (#239).
	self := unknownSelfCalls(k.APILifecycle, nil)
	// versions' kube-system pods, which addons does not list again (#227).
	// Not one shared all-namespaces list: versions would then fail with it
	// under the narrow kube-system-only role (#122) or when a cluster-wide
	// list stalls, where today it still succeeds.
	var kubeSystem kubeSystemPods
	return []step{
		{cap: inventory.CapVersions, run: func(ctx context.Context, inv *inventory.Inventory) error {
			if c.Kube == nil || c.Discovery == nil {
				return errors.New("kubernetes client not configured")
			}
			err := collectVersionsFrom(ctx, c.Discovery, c.Kube, opts.TeamLabel, inv, &kubeSystem)
			opts.DiscoveryCache.observeVersion(inv.ServerVersion) // before any step reads discovery
			return err
		}},
		{cap: inventory.CapHelm, run: func(ctx context.Context, inv *inventory.Inventory) error {
			if c.Kube == nil || c.Metadata == nil {
				return errors.New("kubernetes/metadata client not configured")
			}
			return collectHelmStep(ctx, c, k.APILifecycle, opts.HelmCache, opts.GitOpsCache, inv)
		}},
		{cap: inventory.CapAddOns, run: func(ctx context.Context, inv *inventory.Inventory) error { // after helm: consumes inv.HelmReleases
			if c.Kube == nil {
				return errors.New("kubernetes client not configured")
			}
			return collectAddOnsFrom(ctx, c.Kube, k.AddOns, inv, &kubeSystem)
		}},
		{cap: inventory.CapAPIUsage, run: func(ctx context.Context, inv *inventory.Inventory) error {
			if c.Discovery == nil || c.Metadata == nil {
				return errors.New("discovery/metadata client not configured")
			}
			var err error
			self, err = collectAPIUsage(ctx, c.Discovery, c.Metadata, k.APILifecycle, inv)
			return err
		}},
		{cap: inventory.CapCRDs, run: func(ctx context.Context, inv *inventory.Inventory) error {
			if c.APIExtensions == nil || c.Metadata == nil {
				return errors.New("apiextensions/metadata client not configured")
			}
			return collectCRDs(ctx, c.APIExtensions, c.Metadata, inv)
		}},
		{cap: inventory.CapDeprecatedCalls, run: func(ctx context.Context, inv *inventory.Inventory) error { // after api-usage: consumes self
			if c.RESTClient == nil {
				return errors.New("rest client not configured")
			}
			return collectDeprecatedCalls(ctx, c.RESTClient, self, inv)
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
