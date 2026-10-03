package server

import (
	"bytes"
	"cmp"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// pushHead opens a schemaVersion 1 push whose inventory a shape fills in:
// a current agent's, stamped with the collector schema and reporting
// every capability (pushHeadCapabilities of them), so the server judges
// what the shape carries as a current collector's and decides a verdict
// on it. An unmarked inventory, or one with a required capability not
// reported, is judged otherwise (#194: legacyView, assessmentGaps), and a
// heap proof built on one would not measure the reports, notifications
// and re-evaluations it claims to; unmarkedPush rewrites a push into one
// on purpose.
const pushHead = `{"schemaVersion":1,"clusterName":"prod-eu-1","agentVersion":"` + pushHeadAgent + `","kbVersion":"k","inventory":{` +
	`"schemaVersion":1,"collectorSchema":1,"clusterId":"uid-123","serverVersion":"v1.34.2","capabilities":{` +
	`"api-usage":{"available":true},"deprecated-calls":{"available":true},"helm":{"available":true},` +
	`"addons":{"available":true},"versions":{"available":true},"crds":{"available":true}},`

// pushHeadAgent is pushHead's agentVersion, and pushHeadCapabilities how
// many capabilities it reports.
const (
	pushHeadAgent        = "0.2.0"
	pushHeadCapabilities = 6
)

// unmarkedPush is push, a pushHead push, as an agent reporting agent
// that does not stamp the collector schema sends it: a v0.1.x one
// (legacyView judges neither its API usage nor its deprecated calls) or
// a v0.2.0 release candidate (unattributedUsageView sets aside the usage
// counts that name no object).
func unmarkedPush(push, agent string) string {
	if !strings.HasPrefix(push, pushHead) {
		panic("unmarkedPush: not a pushHead push")
	}
	head := strings.Replace(pushHead, `"collectorSchema":1,`, "", 1)
	head = strings.Replace(head, `"agentVersion":"`+pushHeadAgent+`"`, `"agentVersion":"`+agent+`"`, 1)
	return head + push[len(pushHead):]
}

// ingestHeapShapes builds snapshot pushes of about size bytes in the
// shapes that cost the most to decode per byte (#121): every element of a
// known list a struct (`{}` ObjectRefs, APIUsages), a map of unique keys,
// a list of strings, and, for scale, a realistic inventory and one big
// string.
func ingestHeapShapes() map[string]func(size int) string {
	repeat := func(start, unit, end string) func(int) string {
		return func(size int) string {
			var b strings.Builder
			b.WriteString(pushHead + start + unit)
			for b.Len() < size-len(unit)-len(end)-1 {
				b.WriteString("," + unit)
			}
			b.WriteString(end)
			return b.String()
		}
	}
	return map[string]func(int) string{
		"one string": func(size int) string {
			return pushHead + `"x":"` + strings.Repeat("y", max(size-len(pushHead)-9, 0)) + `"}}`
		},
		"ObjectRefs {}":                ingestObjectRefs,
		"ObjectRefs {}, 100 per usage": objectRefUsages,
		"APIUsages {}":                 repeat(`"apiUsage":[`, `{}`, `]}}`),
		"strings":                      repeat(`"unrecognizedImages":[`, `""`, `]}}`),
		"skipped strings":              repeat(`"capabilities":{"api-usage":{"available":true,"partial":true,"skipped":[`, `""`, `]}}}}`),
		"unique map keys":              uniqueNamespaces,
		"realistic":                    realisticInventory,
		// The reviewer's push (#121): namespace keys of 190 apostrophes,
		// 1,000 per entry, refused once decoded; valid names, capped at
		// 100 listed per finding (stored); and 100 per entry, which the
		// engine lists twice in each finding, past the report limit.
		"namespace map of apostrophes":  namespaceApostrophes,
		"namespace map, 1000 per usage": namespaceUsages(1000),
		"namespace map, 100 per usage":  namespaceUsages(100),
		"Helm releases of escapes":      helmReleases,
		// Every object a finding, named with characters encoding/json
		// escapes as six bytes: its three stored reports were each six
		// times the push (+323 MiB at the node budget).
		"PSP usages, long names of <": func(size int) string {
			return pspUsagesNamed(size, strings.Repeat("<", 190))
		},
	}
}

// ingestObjectRefs is one API usage entry of `{}` object refs to about
// size bytes: the dearest push to decode per byte, refused once decoded
// (a collector records at most 100 per entry).
func ingestObjectRefs(size int) string {
	const head, unit, end = `"apiUsage":[{"version":"v1","kind":"Pod","count":1,"objects":[`, `{}`, `]}]}}`
	var b strings.Builder
	b.WriteString(pushHead + head + unit)
	for b.Len() < size-len(unit)-len(end)-1 {
		b.WriteString("," + unit)
	}
	b.WriteString(end)
	return b.String()
}

// objectRefUsages is ingestObjectRefs as a collector writes it: entries
// of 100 `{}` object refs each, one per kind.
func objectRefUsages(size int) string {
	return fill(size, `"apiUsage":[`, func(i int) string {
		return fmt.Sprintf(`{"version":"v1","kind":"Kind%d","count":100,"objects":[{}%s]}`, i, strings.Repeat(",{}", 99))
	}, "]}}")
}

// afterDecode is the status of a push of an ingestHeapShapes shape
// within the snapshot budget, when it is not 202: 422 for one no
// collector writes, 413 for one whose report would be over its limit.
var afterDecode = map[string]int{
	"ObjectRefs {}":                http.StatusUnprocessableEntity,
	"APIUsages {}":                 http.StatusUnprocessableEntity,
	"strings":                      http.StatusUnprocessableEntity,
	"namespace map of apostrophes": http.StatusUnprocessableEntity,
	"namespace map, 100 per usage": http.StatusRequestEntityTooLarge,
	"Helm releases of escapes":     http.StatusRequestEntityTooLarge,
	"PSP usages, long names of <":  http.StatusRequestEntityTooLarge,
}

func uniqueNamespaces(size int) string {
	var b strings.Builder
	b.WriteString(pushHead + `"apiUsage":[{"version":"v1","kind":"Pod","count":1,"namespaces":{"0":1`)
	for i := 1; b.Len() < size-20; i++ {
		fmt.Fprintf(&b, `,"%x":1`, i)
	}
	b.WriteString("}}]}}")
	return b.String()
}

// realisticInventory is GVKs of 100 named objects each, across namespaces.
func realisticInventory(size int) string {
	var b strings.Builder
	b.WriteString(pushHead + `"apiUsage":[`)
	for i := 0; b.Len() < size-12000; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"group":"apps","version":"v1","kind":"Kind%d","count":100,"namespaces":{"team-a":50,"team-b":50},"objects":[`, i)
		for j := range 100 {
			if j > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"namespace":"team-%d","name":"object-%d-%d","manager":"kube-controller-manager"}`, j%2, i, j)
		}
		b.WriteString("]}")
	}
	b.WriteString("]}}")
	return b.String()
}

func snapshotUnits(body string) int {
	var m jsonMeter
	m.feed([]byte(body))
	return m.cost.units()
}

// atSnapshotBudget returns the largest push shape builds (up to the
// default size cap) that is still within the snapshot node budget.
func atSnapshotBudget(shape func(size int) string) string {
	lo, hi := 0, DefaultMaxSnapshotBytes-64
	if body := shape(hi); snapshotUnits(body) <= maxSnapshotUnits {
		return body
	}
	for hi-lo > 4096 {
		if mid := (lo + hi) / 2; snapshotUnits(shape(mid)) <= maxSnapshotUnits {
			lo = mid
		} else {
			hi = mid
		}
	}
	return shape(lo)
}

// maxIngestDecodeHeap is what decoding, evaluating and storing one
// snapshot push may add to the heap (#121), on SQLite at the most targets
// a server takes (atTargetCap): measured up to ~119 MiB for a cluster's
// first push (1,000 namespace keys per API usage entry decode to many
// maps), and up to ~216 MiB in six runs on a loaded 8-core machine for a
// cluster's later push with a notifier, whose five reports are each about
// the report limit and each previous one its baseline
// (TestStoredSnapshotHeapIsBounded; runs of one shape differ by up to
// ~35 MiB). With no extra target that push takes up to ~119 MiB, so each
// adds 18-28 MiB. The bound leaves ~11% over the worst run.
const maxIngestDecodeHeap = 240 << 20

// serveIngest runs one snapshot push straight through the handler.
func serveIngest(s *Server, w http.ResponseWriter, body []byte, gzipped bool) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/snapshots", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer ingest-tok")
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	s.Handler().ServeHTTP(w, req)
}

// ingestVariant is how a push of an ingestHeapShapes shape is sent:
// rewritten by push from pushHead's marked, current inventory, plain or
// gzipped (gzip), and, within the snapshot budget, answered as judged:
// status overrides afterDecode for a shape this variant's view judges
// otherwise.
type ingestVariant struct {
	name   string
	push   func(string) string
	gzip   []bool
	status map[string]int
}

// ingestVariants are a push as each kind of agent sends it. The node
// budget is counted on the bytes as they arrive, before anything is
// decoded, so each is refused at the same size; what the server then
// judges differs (#194). A v0.2.0 release candidate's unmarked usage
// counts that name no object are not judged (unattributedUsageView), so
// the namespace map's findings, and its report past the limit, are not
// built; nor are a v0.1.x agent's API usage findings (legacyView), the
// long-named PSP objects' among them. Their Helm releases are judged,
// and still refused. The marker changes nothing gzip does, so only the
// marked push is also sent gzipped.
var ingestVariants = []ingestVariant{
	{name: "marked", push: func(b string) string { return b }, gzip: []bool{false, true}},
	{
		name: "unmarked, v0.2.0-rc.2", push: func(b string) string { return unmarkedPush(b, "0.2.0-rc.2") }, gzip: []bool{false},
		status: map[string]int{"namespace map, 100 per usage": http.StatusAccepted},
	},
	{
		name: "unmarked, v0.1.1", push: func(b string) string { return unmarkedPush(b, "0.1.1") }, gzip: []bool{false},
		status: map[string]int{"namespace map, 100 per usage": http.StatusAccepted, "PSP usages, long names of <": http.StatusAccepted},
	},
}

// The node budget is counted on the pushed bytes as they arrive, before
// the inventory is decoded and long before the server decides whose
// collector wrote it (legacyView), so the same bytes are refused with 413
// whether the inventory is stamped with the collector schema or not, and
// whichever agent sent it; just within the budget, each is decoded and
// checked alike (an object list no collector writes: 422). This is the
// cheap half of TestIngestDecodeHeapIsBounded, run by every go test.
func TestSnapshotNodeBudgetPrecedesTheCollectorView(t *testing.T) {
	over, within := ingestObjectRefs(400<<10), ingestObjectRefs(300<<10)
	if snapshotUnits(over) <= maxSnapshotUnits || snapshotUnits(within) > maxSnapshotUnits {
		t.Fatalf("fixture: %d and %d units, want over and within the %d budget", snapshotUnits(over), snapshotUnits(within), maxSnapshotUnits)
	}
	for _, v := range ingestVariants {
		for body, want := range map[string]int{over: http.StatusRequestEntityTooLarge, within: http.StatusUnprocessableEntity} {
			s := newTestServer(t, newFakeStore())
			rec := httptest.NewRecorder()
			serveIngest(s, rec, []byte(v.push(body)), false)
			if rec.Code != want {
				t.Fatalf("%s, %d bytes: status = %d (%.300s), want %d", v.name, len(body), rec.Code, rec.Body, want)
			}
			if want == http.StatusRequestEntityTooLarge && !strings.Contains(rec.Body.String(), "too large to evaluate") {
				t.Fatalf("%s: %s; want the node budget's refusal", v.name, rec.Body)
			}
		}
	}
}

// Snapshot decode cost follows the JSON's structure, not its size: 20 MiB
// of `{}` ObjectRefs decoded to ~2.6 GB of heap, from any per-cluster
// agent token (#121). Each shape at the size cap and at the largest size
// within the node budget, plain and gzipped, is refused with 413 before
// it is decoded, or decoded, checked, evaluated at the most targets a
// server takes (atTargetCap) and stored on SQLite within
// maxIngestDecodeHeap; within the budget it is accepted unless no
// collector writes it (422) or its report would be over the report
// limit (413, afterDecode), and the body budget is given back either way.
// So it is for the same bytes from an agent that does not stamp the
// collector schema (ingestVariants): the budget refuses them alike, and
// what is decoded is judged as that agent's within the same bound.
func TestIngestDecodeHeapIsBounded(t *testing.T) {
	if testing.Short() || raceEnabled || !heapRun {
		t.Skip("decodes several 20 MiB snapshots; make test-heap runs it, without the race detector")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // live heap, as under GOMEMLIMIT (see TestGateDecodeHeapIsBounded)
	for name, shape := range ingestHeapShapes() {
		t.Run(name, func(t *testing.T) {
			full := shape(DefaultMaxSnapshotBytes - 64)
			bodies := []string{full}
			if fits := atSnapshotBudget(shape); fits != full {
				bodies = append(bodies, fits)
			}
			for _, v := range ingestVariants {
				t.Run(v.name, func(t *testing.T) {
					for _, marked := range bodies {
						decode := snapshotUnits(marked) <= maxSnapshotUnits
						body := v.push(marked)
						if (snapshotUnits(body) <= maxSnapshotUnits) != decode {
							t.Fatalf("fixture: %d bytes are within the node budget marked (%v) but not as %s; want the same bytes either way",
								len(marked), decode, v.name)
						}
						for _, gz := range v.gzip {
							payload := []byte(body)
							if gz {
								payload = gzipBytes(t, payload)
							}
							s := newSQLiteTestServer(t, atTargetCap)
							rec := httptest.NewRecorder()
							grew := heapPeak(func() { serveIngest(s, rec, payload, gz) })
							want := http.StatusRequestEntityTooLarge
							if decode {
								want = cmp.Or(v.status[name], afterDecode[name], http.StatusAccepted)
							}
							if rec.Code != want {
								t.Fatalf("%d bytes, gzip %v: status = %d (%.300s), want %d", len(body), gz, rec.Code, rec.Body, want)
							}
							if grew > maxIngestDecodeHeap {
								t.Fatalf("%d bytes, gzip %v: status %d; the heap grew %d MiB, want at most %d MiB",
									len(body), gz, rec.Code, grew>>20, maxIngestDecodeHeap>>20)
							}
							if used := s.ingestBuffered.inUse(); used != 0 {
								t.Fatalf("%d bytes, gzip %v: %d body bytes still charged", len(body), gz, used)
							}
							t.Logf("%d bytes (%d units), gzip %v: status %d, heap grew %d MiB", len(body), snapshotUnits(body), gz, rec.Code, grew>>20)
						}
					}
				})
			}
		})
	}
}

// Snapshot bodies wait for the ingest slot in memory, so the bytes held
// across every push are bounded, like /gate's: with the slot busy, 30
// concurrent maximum-size pushes buffer at most maxBufferedSnapshotBodies
// bodies; the rest are refused with 503 + Retry-After (the agent retries
// it) as soon as their bytes stop fitting. The kept ones are ingested once
// the slot frees, and the budget returns to 0.
func TestIngestBufferedBodiesAreBounded(t *testing.T) {
	const limit = 1 << 20
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.MaxSnapshotBytes = limit })
	s.ingestQueueTimeout = time.Minute
	for range cap(s.ingestSlots) {
		s.ingestSlots <- struct{}{} // hold the ingest slot
	}
	body := []byte(realisticInventory(limit - 4<<10))
	budget := int64(maxBufferedSnapshotBodies * len(body))
	if budget+int64(len(body)) <= s.ingestBuffered.max {
		t.Fatalf("fixture is %d bytes: want %d bodies to fit the %d-byte budget and one more not to",
			len(body), maxBufferedSnapshotBodies, s.ingestBuffered.max)
	}
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	const n = 30
	type result struct {
		code       int
		retryAfter string
		body       string
	}
	results := make(chan result, n)
	for range n {
		go func() {
			rec := httptest.NewRecorder()
			serveIngest(s, rec, body, false)
			results <- result{rec.Code, rec.Header().Get("Retry-After"), rec.Body.String()}
		}()
	}
	for range n - maxBufferedSnapshotBodies {
		select {
		case r := <-results:
			if r.code != http.StatusServiceUnavailable || r.retryAfter == "" {
				t.Fatalf("status = %d (%s), Retry-After %q; want 503 with Retry-After while the budget is full", r.code, r.body, r.retryAfter)
			}
		case <-time.After(time.Minute):
			t.Fatalf("fewer than %d pushes were refused while the slot was busy", n-maxBufferedSnapshotBodies)
		}
	}
	for deadline := time.Now().Add(10 * time.Second); s.ingestBuffered.inUse() != budget; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d body bytes charged, want the %d bodies waiting (%d bytes)", s.ingestBuffered.inUse(), maxBufferedSnapshotBodies, budget)
		}
	}
	// 30 bodies unbounded are 30 MiB; bounded, 2. 3x the cap leaves room
	// for the test's own garbage.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var during runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&during)
		grew := int64(during.HeapAlloc) - int64(before.HeapAlloc)
		if grew <= 3*limit {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("heap grew %d MiB with %d pushes waiting; the cap is %d MiB", grew>>20, maxBufferedSnapshotBodies, limit>>20)
		}
	}
	for range cap(s.ingestSlots) {
		<-s.ingestSlots
	}
	for range maxBufferedSnapshotBodies {
		select {
		case r := <-results:
			if r.code != http.StatusAccepted && r.code != http.StatusOK {
				t.Fatalf("status = %d (%s), want 202 (or 200 duplicate) once the slot frees", r.code, r.body)
			}
		case <-time.After(time.Minute):
			t.Fatal("waiting pushes did not finish after the slot was freed")
		}
	}
	if used := s.ingestBuffered.inUse(); used != 0 {
		t.Fatalf("%d body bytes still charged after every push finished", used)
	}
}

// stallPush opens a raw connection, sends snapshot push headers declaring
// declared body bytes, sends the first sent bytes and stalls.
func stallPush(t *testing.T, addr string, declared int, sent string, gzipped bool) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	enc := ""
	if gzipped {
		enc = "Content-Encoding: gzip\r\n"
	}
	fmt.Fprintf(conn, "POST /api/v1/snapshots HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer ingest-tok\r\n%s"+
		"Content-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", enc, declared, sent)
	return conn
}

// A push whose body does not arrive within ReadTimeout gets 408, which the
// agent retries, not a 422 it drops the snapshot on; and the error names
// no socket addresses (it used to echo "read tcp 127.0.0.1:…->…: i/o
// timeout"). The same holds when the stall is before the gzip header.
func TestIngestBodyReadTimeoutIs408(t *testing.T) {
	for _, tc := range []struct {
		name string
		sent string
		gzip bool
	}{
		{"identity", `{"schemaVersion":1,`, false},
		{"before the gzip header", "", true},
		{"inside a gzip stream", "\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\xff", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, newFakeStore())
			ts := httptest.NewUnstartedServer(s.Handler())
			ts.Config.ReadTimeout = 300 * time.Millisecond
			ts.Start()
			t.Cleanup(ts.Close)
			conn := stallPush(t, ts.Listener.Addr().String(), 100000, tc.sent, tc.gzip)
			code, body := readStatus(t, conn, 5*time.Second)
			if code != http.StatusRequestTimeout || strings.Contains(body, "127.0.0.1") || strings.Contains(body, "tcp") {
				t.Fatalf("status = %d (%s), want 408 naming no address", code, body)
			}
			if used := s.ingestBuffered.inUse(); used != 0 {
				t.Fatalf("%d body bytes still charged", used)
			}
		})
	}
}
