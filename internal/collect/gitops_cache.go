package collect

import (
	"maps"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ForbiddenListRecheck is how long a GitOpsCache remembers a refused list:
// once that much time has passed it is asked for again, so a list granted
// later is used within the hour.
const ForbiddenListRecheck = time.Hour

// GitOpsCache remembers, between a long-running caller's (the agent's)
// collections, which OCIRepository lists the apiserver refused (#248). A
// role granted get but not list on them (a namespaced Role, or one written
// for the agent before #248) refuses the cluster-wide list and then each
// namespace's before readOCIRepositories falls back to a GET by name: one
// refused request per namespace plus one, each an entry in the audit log,
// on every tick. With a cache, a refused list is not asked for again until
// ForbiddenListRecheck has passed since it was refused, so a steady tick
// goes straight to what worked; then it is asked again, and forgotten once
// it succeeds.
//
// Only a refusal (403) is remembered: a list that failed otherwise is asked
// for again on the next call. A list is keyed by the API version it was
// asked at and its namespace, so a Flux upgrade that serves another version
// starts afresh, and a refusal is dropped once it is ForbiddenListRecheck
// old, so the cache holds at most one entry per namespace refused within
// that time, plus the cluster-wide list.
//
// The zero value is not usable; a nil *GitOpsCache remembers nothing, which
// is what a one-shot scan has: it asks for each list once, in its one pass.
type GitOpsCache struct {
	mu      sync.Mutex
	now     func() time.Time
	refused map[gitopsList]time.Time // when each list was last refused
}

// gitopsList names one list readOCIRepositories asks for.
type gitopsList struct {
	gvr       schema.GroupVersionResource
	namespace string // metav1.NamespaceAll: the cluster-wide list
}

// NewGitOpsCache returns an empty cache.
func NewGitOpsCache() *GitOpsCache {
	return &GitOpsCache{now: time.Now, refused: map[gitopsList]time.Time{}}
}

// expire drops the refusals that are ForbiddenListRecheck old: those lists
// are asked for again.
func (c *GitOpsCache) expire() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	maps.DeleteFunc(c.refused, func(_ gitopsList, at time.Time) bool { return now.Sub(at) >= ForbiddenListRecheck })
}

// wasRefused reports whether l was refused less than ForbiddenListRecheck
// ago: it is then not asked for.
func (c *GitOpsCache) wasRefused(l gitopsList) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.refused[l]
	return ok && c.now().Sub(at) < ForbiddenListRecheck
}

// record records how asking for l went: a refusal is remembered from now,
// a success forgets an earlier one, and any other error changes nothing.
func (c *GitOpsCache) record(l gitopsList, err error) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case apierrors.IsForbidden(err):
		c.refused[l] = c.now()
	case err == nil:
		delete(c.refused, l)
	}
}
