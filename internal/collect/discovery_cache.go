package collect

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	openapi_v2 "github.com/google/gnostic-models/openapiv2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/openapi"
	restclient "k8s.io/client-go/rest"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// DiscoveryMaxAge is the oldest API discovery a DiscoveryCache serves.
const DiscoveryMaxAge = time.Hour

// DiscoveryCache keeps what API discovery returned between the agent's
// ticks (#228), so a steady tick does not ask the apiserver again which
// groups, versions and resources it serves: 4 requests of every tick,
// asked by the GitOps detection and by api-usage, whose answer changes only
// when the cluster's APIs do.
//
// What it keeps is one complete answer of ServerGroupsAndResources, the
// call api-usage makes; GitOps detection's ServerGroups and
// ServerResourcesForGroupVersion are answered from it once it holds one. An
// answer with an error, even a partial one (a group whose discovery
// failed), is never kept, so a failure is asked again on the next call. The
// server version is never cached: the versions step asks for it on every
// tick, and an answer from another version empties the cache before any
// other step reads it, so an upgrade of the apiserver is seen on the tick
// it happens. Otherwise the answer is dropped, and asked again on the next
// tick, when
//
//   - it is DiscoveryMaxAge (an hour) old;
//   - the tick's CustomResourceDefinitions (their groups, kinds and served
//     versions, read by the crds step) differ from those of the tick it
//     was filled in, or the crds step could not list them: a CRD added,
//     removed or changed is seen one tick later. Custom resources that
//     could not be listed (the crds capability partial) are not a reason:
//     the CRDs were still read;
//
// so discovery is at most one tick behind a CRD change, and an hour behind
// any other change of the APIs served at the same version (an APIService
// registered, an API enabled by a restart with other flags).
//
// The zero value is not usable; a nil *DiscoveryCache caches nothing, which
// is what a one-shot scan has. One collection at a time.
type DiscoveryCache struct {
	mu  sync.Mutex
	now func() time.Time

	valid   bool
	filled  time.Time
	groups  []*metav1.APIGroup
	lists   []*metav1.APIResourceList
	version string // the server version seen in the tick it was filled in
	crds    string // crdFingerprint of the tick it was filled in; "" until that tick's crds step ran
	stale   bool   // drop it before the next tick
}

// NewDiscoveryCache returns an empty cache.
func NewDiscoveryCache() *DiscoveryCache { return &DiscoveryCache{now: time.Now} }

// begin starts a collection: an answer too old, or marked stale by the
// tick before, is dropped.
func (c *DiscoveryCache) begin() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && (c.stale || c.now().Sub(c.filled) >= DiscoveryMaxAge) {
		c.dropLocked()
	}
	c.stale = false
}

// observeVersion is the server version this tick's versions step read
// ("" when it could not): an answer filled at another version is dropped.
func (c *DiscoveryCache) observeVersion(v string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && (v == "" || v != c.version) {
		c.dropLocked()
	}
	c.version = v
}

// end ends a collection: CRDs that differ from those of the tick the answer
// was filled in, or that could not be read, make it stale for the next.
// The CRDs were read when the crds capability is available, partial or
// not: it is partial when a CRD's custom resources could not be listed
// (an agent is granted none) or a CRD serves no version to list them at,
// and either leaves every CRD recorded (collectCRDs). Only a CRD list that
// failed makes the capability unavailable.
func (c *DiscoveryCache) end(inv *inventory.Inventory) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fp := ""
	if st, ok := inv.Capabilities[inventory.CapCRDs]; ok && st.Available {
		fp = crdFingerprint(inv.CRDs)
	}
	switch {
	case !c.valid:
	case fp == "":
		c.stale = true
	case c.crds == "":
		c.crds = fp // the tick it was filled in
	case fp != c.crds:
		c.stale = true
	}
}

func (c *DiscoveryCache) dropLocked() {
	c.valid, c.groups, c.lists, c.crds = false, nil, nil, ""
}

// crdFingerprint identifies the CRDs' groups, kinds and served versions.
func crdFingerprint(crds []inventory.CRD) string {
	h := sha256.New()
	for _, c := range crds {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%v\n", c.Group, c.Kind, c.Plural, c.Versions)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// client is d answering discovery from the cache where it can.
func (c *DiscoveryCache) client(d discovery.DiscoveryInterface) discovery.DiscoveryInterface {
	if c == nil || d == nil {
		return d
	}
	return &cachedDiscovery{DiscoveryInterface: d, ctx: discovery.ToDiscoveryInterfaceWithContext(d), cache: c}
}

// cachedDiscovery is a discovery client whose ServerGroupsAndResources,
// ServerGroups and ServerResourcesForGroupVersion read the cache, filling
// it from a complete answer of ServerGroupsAndResources; every other call
// goes to the client it wraps.
type cachedDiscovery struct {
	discovery.DiscoveryInterface
	ctx   discovery.DiscoveryInterfaceWithContext
	cache *DiscoveryCache
}

var (
	_ discovery.DiscoveryInterface            = &cachedDiscovery{}
	_ discovery.DiscoveryInterfaceWithContext = &cachedDiscovery{}
)

// held returns the cached answer, if there is one.
func (d *cachedDiscovery) held() ([]*metav1.APIGroup, []*metav1.APIResourceList, bool) {
	d.cache.mu.Lock()
	defer d.cache.mu.Unlock()
	return d.cache.groups, d.cache.lists, d.cache.valid
}

func (d *cachedDiscovery) ServerGroupsAndResourcesWithContext(ctx context.Context) ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	if groups, lists, ok := d.held(); ok {
		return groups, lists, nil
	}
	groups, lists, err := d.ctx.ServerGroupsAndResourcesWithContext(ctx)
	if err == nil {
		c := d.cache
		c.mu.Lock()
		c.valid, c.filled, c.groups, c.lists, c.crds = true, c.now(), groups, lists, ""
		c.mu.Unlock()
	}
	return groups, lists, err
}

func (d *cachedDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return d.ServerGroupsAndResourcesWithContext(context.Background())
}

func (d *cachedDiscovery) ServerGroupsWithContext(ctx context.Context) (*metav1.APIGroupList, error) {
	if groups, _, ok := d.held(); ok {
		l := &metav1.APIGroupList{TypeMeta: metav1.TypeMeta{Kind: "APIGroupList", APIVersion: "v1"}}
		for _, g := range groups {
			if g != nil {
				l.Groups = append(l.Groups, *g)
			}
		}
		return l, nil
	}
	return d.ctx.ServerGroupsWithContext(ctx)
}

func (d *cachedDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	return d.ServerGroupsWithContext(context.Background())
}

func (d *cachedDiscovery) ServerResourcesForGroupVersionWithContext(ctx context.Context, groupVersion string) (*metav1.APIResourceList, error) {
	if _, lists, ok := d.held(); ok {
		for _, l := range lists {
			if l != nil && l.GroupVersion == groupVersion {
				return l, nil
			}
		}
	}
	return d.ctx.ServerResourcesForGroupVersionWithContext(ctx, groupVersion)
}

func (d *cachedDiscovery) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	return d.ServerResourcesForGroupVersionWithContext(context.Background(), groupVersion)
}

// The rest goes to the wrapped client. RESTClient is declared here because
// both views of it have one.

func (d *cachedDiscovery) RESTClient() restclient.Interface { return d.DiscoveryInterface.RESTClient() }

func (d *cachedDiscovery) ServerPreferredResourcesWithContext(ctx context.Context) ([]*metav1.APIResourceList, error) {
	return d.ctx.ServerPreferredResourcesWithContext(ctx)
}

func (d *cachedDiscovery) ServerPreferredNamespacedResourcesWithContext(ctx context.Context) ([]*metav1.APIResourceList, error) {
	return d.ctx.ServerPreferredNamespacedResourcesWithContext(ctx)
}

func (d *cachedDiscovery) ServerVersionWithContext(ctx context.Context) (*version.Info, error) {
	return d.ctx.ServerVersionWithContext(ctx)
}

func (d *cachedDiscovery) OpenAPISchemaWithContext(ctx context.Context) (*openapi_v2.Document, error) {
	return d.ctx.OpenAPISchemaWithContext(ctx)
}

func (d *cachedDiscovery) OpenAPIV3WithContext(ctx context.Context) openapi.ClientWithContext {
	return d.ctx.OpenAPIV3WithContext(ctx)
}

func (d *cachedDiscovery) WithLegacyWithContext(ctx context.Context) discovery.DiscoveryInterfaceWithContext {
	return d.ctx.WithLegacyWithContext(ctx)
}
