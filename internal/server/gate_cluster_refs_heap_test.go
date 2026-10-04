package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// deprecatedRefsCluster is a cluster listing inventory.MaxObjectRefs
// objects of every deprecated or removed GVK in k, each named name plus
// its index (so none is a name manyRefsStream gives), as a collector
// lists at most that many per API.
func deprecatedRefsCluster(k kb.KB, name string) inventory.Inventory {
	inv := testInventory()
	for _, e := range k.APILifecycle {
		if e.Deprecated == nil && e.Removed == nil {
			continue
		}
		u := inventory.APIUsage{Group: e.Group, Version: e.Version, Kind: e.Kind,
			Count: inventory.MaxObjectRefs, Namespaces: map[string]int{}}
		for i := range inventory.MaxObjectRefs {
			ns := fmt.Sprintf("n%d", i%10)
			u.Namespaces[ns]++
			u.Objects = append(u.Objects, inventory.ObjectRef{Namespace: ns, Name: name + fmt.Sprint(i)})
		}
		inv.APIUsage = append(inv.APIUsage, u)
	}
	return inv
}

// The dearest ?cluster= answer to evaluate (#72): the cluster lists a
// hundred refs at every deprecated GVK, and the PR adds a hundred more at
// each, so upsertUsage hands the engine 200 refs per finding, with names
// of up to 200 bytes and a ?path=, before capObjects cuts each listing
// back to a hundred (the PR's first). The engine evaluates, the gate
// suppresses and encodes from those lists, and the heap stays within
// maxGateDecodeHeap, the bound SE-05b states (measured ~21-30 MiB). The
// answer lists a hundred objects and counts the other hundred as omitted.
// The cluster's names are always 199 bytes; the PR's are the longest
// whose answer is within the answer limit (all 199 of "x", fewer of `"`,
// which JSON writes as two bytes: the answer lists only the PR's).
func TestGateClusterRefsAtEveryGVKHeapIsBounded(t *testing.T) {
	if testing.Short() || raceEnabled || !heapRun {
		t.Skip("pushes a 5 MiB snapshot and evaluates 200 refs at each of 136 GVKs; make test-heap runs it, without the race detector")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // as in TestGateDecodeHeapIsBounded
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	const path = "deploy/overlays/production/rendered.yaml"
	for _, unit := range []string{"x", `"`} {
		t.Run(unit, func(t *testing.T) {
			s := newTestServer(t, newFakeStore(), func(c *Config) { c.KB = k })
			// 197 units and an index of at most two digits: names of at
			// most 199 bytes. The cluster's start with "c" so that none is
			// a name the PR gives (upsertUsage would replace it).
			rec := httptest.NewRecorder()
			serveIngest(s, rec, pushReqBody(t, deprecatedRefsCluster(k, "c"+strings.Repeat(unit, 196))), false)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("push: status = %d (%.300s)", rec.Code, rec.Body)
			}
			post := func(n int) *httptest.ResponseRecorder {
				q := url.Values{"target": {"1.35"}, "fail-on": {"never"}, "cluster": {"prod-eu-1"}, "path": {path}}
				req := httptest.NewRequest(http.MethodPost, "/api/v1/gate?"+q.Encode(),
					strings.NewReader(manyRefsStream(k, inventory.MaxObjectRefs, strings.Repeat(unit, n))))
				req.Header.Set("Content-Type", "application/x-yaml")
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, req)
				return rec
			}
			lo, hi := 1, 197
			if post(hi).Code == http.StatusOK {
				lo = hi
			} else {
				for hi-lo > 1 {
					if mid := (lo + hi) / 2; post(mid).Code == http.StatusOK {
						lo = mid
					} else {
						hi = mid
					}
				}
			}
			grew := heapPeak(func() { rec = post(lo) })
			if rec.Code != http.StatusOK {
				t.Fatalf("gate with %d-unit names: status = %d (%.300s), want 200", lo, rec.Code, rec.Body)
			}
			var a struct {
				Findings []struct {
					Objects        []inventory.ObjectRef
					ObjectsOmitted int
				} `json:"findings"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
				t.Fatal(err)
			}
			// Every finding had 200 refs before the cut: a hundred listed
			// and a hundred omitted, never more of either.
			full := 0
			for _, f := range a.Findings {
				if len(f.Objects) == inventory.MaxObjectRefs && f.ObjectsOmitted == inventory.MaxObjectRefs {
					full++
				}
				if len(f.Objects) > inventory.MaxObjectRefs {
					t.Fatalf("a finding lists %d objects", len(f.Objects))
				}
			}
			if full < 100 {
				t.Fatalf("%d of %d findings list a hundred objects and omit a hundred; the shape is not the worst case", full, len(a.Findings))
			}
			if grew > maxGateDecodeHeap {
				t.Fatalf("the heap grew %d MiB, want at most %d MiB", grew>>20, maxGateDecodeHeap>>20)
			}
			t.Logf("%d-unit names: %d findings, %d with 200 refs before the cut, answer %d bytes, heap grew %d MiB",
				lo, len(a.Findings), full, rec.Body.Len(), grew>>20)
		})
	}
}
