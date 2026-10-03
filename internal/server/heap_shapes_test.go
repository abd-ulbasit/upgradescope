package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// usageKind is the kind of a shape's i-th API usage entry. A push names
// each group/version/kind once (inventory.ValidateLimits), so the first is
// PodSecurityPolicy, which testKB removes in 1.35, and the rest are kinds
// of the same group that testKB does not know: an unknown-api finding
// each, as large.
func usageKind(i int) string {
	if i == 0 {
		return "PodSecurityPolicy"
	}
	return fmt.Sprint("PodSecurityPolicy", i)
}

// escapes are the characters an encoder lengthens most: HTML writes
// ' " & as five bytes and < > as four, %q and JSON write " as two, and
// csvSafe puts ' before an = after a ;.
const escapes = `'"&<>;=`

// escaped is n bytes rotating through escapes, starting at the i-th.
func escaped(n, i int) string {
	var b strings.Builder
	for j := range n {
		b.WriteByte(escapes[(i+j)%len(escapes)])
	}
	return b.String()
}

// fill writes pushHead, open, then units from unit(i) joined by commas
// while they fit in size bytes with close, then close.
func fill(size int, open string, unit func(i int) string, close string) string {
	var b strings.Builder
	b.WriteString(pushHead + open)
	for i := 0; ; i++ {
		u := unit(i)
		if b.Len()+len(u)+1+len(close) > size {
			break
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(u)
	}
	b.WriteString(close)
	return b.String()
}

// nsKey is the i-th of the 63-byte namespace names a shape uses: the
// longest an RFC 1123 label may be.
func nsKey(i int) string {
	k := fmt.Sprintf("n%x-", i)
	return k + strings.Repeat("x", 63-len(k))
}

// namespaceUsages are API usage entries with per namespaces each and no
// objects (#121): every namespace in a finding's namespaces and its
// evidence sentence, each once per push. With 1,000 namespace keys of
// 190 apostrophes per entry, before identifiers were checked, a 21 MB
// push stored three 42 MB reports; with per at most
// engine.MaxFindingNamespaces, valid names and no cap on the report, it
// still stored about twice the push.
func namespaceUsages(per int) func(int) string {
	return func(size int) string {
		k := 0
		return fill(size, `"apiUsage":[`, func(i int) string {
			var b strings.Builder
			fmt.Fprintf(&b, `{"group":"policy","version":"v1beta1","kind":%q,"count":%d,"namespaces":{`, usageKind(i), per)
			for j := range per {
				if j > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, `%q:1`, nsKey(k))
				k++
			}
			b.WriteString("}}")
			return b.String()
		}, "]}}")
	}
}

// namespaceApostrophes is namespaceUsages(1000) with the reviewer's keys:
// 190 apostrophes after a number, which no namespace can be (422).
func namespaceApostrophes(size int) string {
	k := 0
	return fill(size, `"apiUsage":[`, func(i int) string {
		var b strings.Builder
		fmt.Fprintf(&b, `{"group":"policy","version":"v1beta1","kind":%q,"count":1000,"namespaces":{`, usageKind(i))
		for j := range 1000 {
			if j > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"n%d%s":1`, k, strings.Repeat("'", 190))
			k++
		}
		b.WriteString("}}")
		return b.String()
	}, "]}}")
}

// teamUsages are 1,000 namespaces of short names, each with its own
// 63-byte team, and API usage entries in 100 of them each: every finding
// lists 100 teams of 66 bytes from 100 keys of 8.
func teamUsages(size int) string {
	var head strings.Builder
	head.WriteString(`"namespaces":[`)
	for i := range 1000 {
		if i > 0 {
			head.WriteByte(',')
		}
		team := fmt.Sprintf("t%d-", i)
		fmt.Fprintf(&head, `{"name":"n%x","team":"%s%s"}`, i, team, strings.Repeat("a", 63-len(team)))
	}
	head.WriteString(`],"apiUsage":[`)
	k := 0
	return fill(size, head.String(), func(i int) string {
		var b strings.Builder
		fmt.Fprintf(&b, `{"group":"policy","version":"v1beta1","kind":%q,"count":100,"namespaces":{`, usageKind(i))
		for j := range 100 {
			if j > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"n%x":1`, k%1000)
			k++
		}
		b.WriteString("}}")
		return b.String()
	}, "]}}")
}

// helmReleases are installed releases whose chart kubeVersion does not
// parse, each an info finding that quotes it in its title and its
// detail, with the chart name and version: all of them escapes. A 5 MB
// push of releases with a short kubeVersion that excludes the target
// stored three 41 MB reports.
func helmReleases(size int) string {
	return fill(size, `"helmReleases":[`, func(i int) string {
		return fmt.Sprintf(`{"name":"r%x","namespace":"web","chartName":%q,"chartVersion":%q,"kubeVersion":%q,"status":"deployed"}`,
			i, escaped(16, i), escaped(8, i+1), escaped(32, i+2))
	}, "]}}")
}

// managerUsages are API usage entries of 100 objects each, every object
// written by its own manager of 120 escapes: the finding's detail names
// each manager, and HTML writes most of their bytes as five.
func managerUsages(size int) string {
	return fill(size, `"apiUsage":[`, func(i int) string {
		var b strings.Builder
		fmt.Fprintf(&b, `{"group":"policy","version":"v1beta1","kind":%q,"count":100,"objects":[`, usageKind(i))
		for j := range 100 {
			if j > 0 {
				b.WriteByte(',')
			}
			m, err := jsonString(fmt.Sprintf("%03d", j) + escaped(120, j))
			if err != nil {
				panic(err)
			}
			fmt.Fprintf(&b, `{"name":"o-%d-%d","manager":%s}`, i, j, m)
		}
		b.WriteString("]}")
		return b.String()
	}, "]}}")
}

// deprecatedCalls are apiserver_requested_deprecated_apis rows, each a
// finding of its own, for resources named with escapes.
func deprecatedCalls(size int) string {
	return fill(size, `"deprecatedCalls":[`, func(i int) string {
		r, err := jsonString(fmt.Sprintf("r%x", i) + escaped(24, i))
		if err != nil {
			panic(err)
		}
		return fmt.Sprintf(`{"group":"g","version":"v1","resource":%s,"removedRelease":"1.2"}`, r)
	}, "]}}")
}

// capabilityGaps are the most capabilities a push may report
// (inventory.MaxCapabilities, versions in pushHead among them), the rest
// partial, each a gap the report repeats for every target: a reason of
// the longest a push may send and a list of what it skipped to about size
// bytes in all, all escapes, which the HTML export lists. The evaluation
// summaries keep a bounded part of each.
func capabilityGaps(size int) string {
	const n = inventory.MaxCapabilities - 1
	per := max(size/n-inventory.MaxReasonBytes, 0)
	var b strings.Builder
	b.WriteString(pushHead + `"capabilities":{`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		r, err := jsonString(escaped(inventory.MaxReasonBytes/2, i)) // " is two bytes in JSON
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(&b, `"c%d":{"available":true,"partial":true,"reason":%s,"skipped":[`, i, r)
		for j, start := 0, b.Len(); b.Len()-start < per; j++ {
			if j > 0 {
				b.WriteByte(',')
			}
			e, err := jsonString(escaped(100, j))
			if err != nil {
				panic(err)
			}
			b.WriteString(e)
		}
		b.WriteString("]}")
	}
	b.WriteString("}}}")
	return b.String()
}

// jsonString is s as a JSON string.
func jsonString(s string) (string, error) {
	b, err := marshalJSON(s)
	return strings.TrimSpace(string(b)), err
}

// storedBodies caches storedBody's pushes, which take a search to find.
var storedBodies sync.Map

// storedBody returns the largest push of shape the server stores: within
// the snapshot budget (atSnapshotBudget) and with every target's report
// within its limit, which a push of some shapes reaches first. Every
// target is the default and heapTargets (storedPush), the most a server
// evaluates.
//
// The bodies are sized for a server at that cap, and the read proofs
// (TestReadHeapIsBounded, TestFleetReadsLoadNoReport) store them on
// servers with no extra target. That still measures the dearest reads
// because what makes these reports large (a finding per usage, release,
// call or gap) is the same at every target testKB knows from 1.35 up,
// and the farther targets add only a version-skew finding (~0.4 KB): the
// default target's report is about as large as any of the five. A testKB
// change that made a higher target's reports much larger would shrink
// these bodies, and the read proofs with them.
func storedBody(name string, shape func(int) string) string {
	if b, ok := storedBodies.Load(name); ok {
		return b.(string)
	}
	body := atSnapshotBudget(shape)
	if !storedPush(body) {
		lo, hi := 0, len(body)
		for hi-lo > 32<<10 {
			if mid := (lo + hi) / 2; storedPush(shape(mid)) {
				lo = mid
			} else {
				hi = mid
			}
		}
		body = shape(lo)
	}
	storedBodies.Store(name, body)
	return body
}

// storedPush reports whether a server at the extra-target cap
// (atTargetCap) stores body: every one of its reports, at the default
// target and at heapTargets, is within the report limit.
func storedPush(body string) bool {
	s, err := New(Config{Store: newFakeStore(), KB: testKB(), IngestToken: "ingest-tok", ExtraTargets: heapTargets})
	if err != nil {
		panic(err)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }
	rec := httptest.NewRecorder()
	serveIngest(s, rec, []byte(body), false)
	return rec.Code == http.StatusAccepted
}
