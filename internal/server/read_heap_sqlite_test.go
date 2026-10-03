package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// heapTargets are MaxExtraTargets extra targets above the 1.34 the heap
// shapes run, so that, with the default 1.35, every push and every
// re-evaluation of one builds and stores the most reports a server does:
// each target adds one of up to --max-snapshot-bytes. The heap tests that
// store snapshots run at them (atTargetCap), to guard the worst case.
var heapTargets = []string{"1.36", "1.37", "1.38", "1.39"}

// atTargetCap configures a test server with heapTargets.
func atTargetCap(c *Config) { c.ExtraTargets = heapTargets }

// newSQLiteTestServer is newTestServer on the SQLite store the server
// ships with: memory figures that matter are the real driver's, which
// copies what it reads, not the fake's, which hands out what it holds.
func newSQLiteTestServer(t *testing.T, opts ...func(*Config)) *Server {
	t.Helper()
	return newSQLiteTestServerAt(t, filepath.Join(t.TempDir(), "upgradescope.db"), opts...)
}

// newSQLiteTestServerAt is newSQLiteTestServer on the database at path.
func newSQLiteTestServerAt(t *testing.T, path string, opts ...func(*Config)) *Server {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := Config{Store: st, KB: testKB(), IngestToken: "ingest-tok"}
	for _, o := range opts {
		o(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.now = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }
	return s
}

// withNotifier configures a test server with a notifier, as
// --slack-webhook or --webhook do: an evaluation of a target that was
// decided before then loads that evaluation's report, the baseline of
// what changed (deltaFor).
func withNotifier(c *Config) { c.Notifier = &recordingNotifier{} }

// earlierPush is body judged at the patch release before: a snapshot of
// the same cluster with another hash, the same targets and reports as
// large, which a later push of body is evaluated against.
func earlierPush(t *testing.T, body string) string {
	t.Helper()
	const now, before = `"serverVersion":"v1.34.2"`, `"serverVersion":"v1.34.1"`
	if !strings.Contains(body, now) {
		t.Fatalf("push has no %s", now)
	}
	return strings.Replace(body, now, before, 1)
}

// rewordStoredFindings rewrites every finding key and title in the
// reports stored in the database at path, as a knowledge base that
// reworded every finding would: the next evaluation of each target then
// differs from the stored one in every finding, so it is inserted, not
// refreshed, and its notification has every blocker as news. It returns
// how many evaluations it rewrote: those with a finding.
func rewordStoredFindings(t *testing.T, path string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res, err := db.Exec(`UPDATE evaluations SET report = CAST(replace(replace(CAST(report AS TEXT),
		'"key":"', '"key":"was-'), '"title":"', '"title":"was-') AS BLOB)
		WHERE instr(CAST(report AS TEXT), '"findings":[{') > 0`)
	if err != nil {
		t.Fatalf("rewording stored findings: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// outboxRows is how many notifications are queued in the database at path.
func outboxRows(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&n); err != nil {
		t.Fatalf("counting queued notifications: %v", err)
	}
	return n
}

// concurrentGets sends n GETs of path at once and returns how much the heap
// grew, failing the test unless every one answered 200, or, for an
// export, 413 for one over the report limit (maxReportBytes).
func concurrentGets(t *testing.T, s *Server, path string, n int) (grew uint64, size int) {
	t.Helper()
	codes, msgs, sizes := make([]int, n), make([]string, n), make([]int, n)
	grew = heapPeak(func() {
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				codes[i], sizes[i] = rec.Code, rec.Body.Len()
				msgs[i] = string(rec.Body.Bytes()[:min(rec.Body.Len(), 300)]) // not the whole body, which the heap would count
			})
		}
		wg.Wait()
	})
	for i, code := range codes {
		overLimit := code == http.StatusRequestEntityTooLarge && strings.Contains(path, "/export") && strings.Contains(msgs[i], "the export would be over")
		if code != http.StatusOK && !overLimit {
			t.Fatalf("%s x%d: status = %d (%.300s), want 200", path, n, code, msgs[i])
		}
	}
	return grew, sizes[0]
}

// maxReevaluationHeap is what the background re-evaluation pass may add to
// the heap: it takes clusters one at a time, and for each target loads the
// current evaluation's report, decodes its findings' heads, evaluates,
// and holds the new report until the cluster's commit. Measured at the
// most targets a server takes (atTargetCap), with a notifier, for a
// snapshot whose five reports are each about the report limit and whose
// every finding changed: up to ~198 MiB in six runs on a loaded 8-core
// machine (runs of one shape differ by up to ~40 MiB), ~105 MiB with no
// extra target, so each adds 18-33 MiB. The bound leaves ~13% over the
// worst run.
const maxReevaluationHeap = 224 << 20

// storedHeapShapes are the snapshots that cost the most once stored, each
// pushed as storedBody stores it: the dearest to decode at the node
// budget; those whose every API-usage entry is a finding, so each stored
// report is as large as the snapshot (~4 MB, and 17 MB with long object
// names), the long names also of characters encoding/json escapes as six
// bytes (<, and U+2028, of three); and those whose reports the engine
// builds larger than their push, which reach the report limit
// (maxReportBytes) first: namespaces listed twice in each finding, teams
// repeated in each, and Helm releases, field managers, deprecated-API
// callers and capability gaps of characters the exports lengthen
// (escapes), in every string an export renders that a push can carry.
// Namespaces and team labels cannot carry them (ingest refuses those).
func storedHeapShapes() map[string]func(int) string {
	return map[string]func(int) string{
		"ObjectRefs {}, 100 per usage": objectRefUsages,
		"PSP usages":                   pspUsages,
		"PSP usages, long names":       longPSPUsages,
		"PSP usages, long names of <": func(size int) string {
			return pspUsagesNamed(size, strings.Repeat("<", 190))
		},
		"PSP usages, long names of U+2028": func(size int) string {
			return pspUsagesNamed(size, strings.Repeat("\u2028", 63))
		},
		"namespace map, 100 per usage": namespaceUsages(100),
		"teams":                        teamUsages,
		"Helm releases of escapes":     helmReleases,
		"managers of escapes":          managerUsages,
		"deprecated calls of escapes":  deprecatedCalls,
		"capability gaps of escapes":   capabilityGaps,
	}
}

// The steps that load what was stored, measured on the SQLite store, which
// copies every snapshot and report it reads, on a server at the most
// targets it takes (atTargetCap) with a notifier, so every evaluation
// with a decided one before it loads that one as its baseline: a
// cluster's later push and its five evaluations, against an earlier
// snapshot as large whose every finding differs, within
// maxIngestDecodeHeap; the re-evaluation pass the next day, every
// evaluation outdated and every finding changed again (as a knowledge
// base update can make them), within maxReevaluationHeap; and the
// dearest /gate stream with ?cluster= against the snapshot within
// maxGateDecodeHeap (answered, or 413 when the answer would be over the
// answer limit). docs/operations.md adds these up for the chart's memory
// limit.
func TestStoredSnapshotHeapIsBounded(t *testing.T) {
	if testing.Short() || raceEnabled || !heapRun {
		t.Skip("stores snapshots at the node budget; make test-heap runs it, without the race detector")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // as in TestGateDecodeHeapIsBounded
	gate := atNodeBudget(gateHeapShapes()["many keys and an alias"])
	for name, shape := range storedHeapShapes() {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "upgradescope.db")
			s := newSQLiteTestServerAt(t, path, atTargetCap, withNotifier)
			body := []byte(storedBody(name, shape))
			targets := append([]string{"1.35"}, heapTargets...)

			// The cluster's earlier snapshot, its five reports as large and
			// every finding in them unlike this push's: the push loads each
			// as its target's baseline and finds every blocker news.
			rec := httptest.NewRecorder()
			serveIngest(s, rec, []byte(earlierPush(t, string(body))), false)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("earlier push: status %d (%.300s), want 202", rec.Code, rec.Body)
			}
			// A report with no finding (none of its shape's kinds flagged at
			// that target) has nothing to reword, and its evaluation is
			// refreshed, not inserted.
			reworded := rewordStoredFindings(t, path)
			queued := outboxRows(t, path)
			rec = httptest.NewRecorder()
			grew := heapPeak(func() { serveIngest(s, rec, body, false) })
			if rec.Code != http.StatusAccepted || grew > maxIngestDecodeHeap {
				t.Errorf("push: status %d, the heap grew %d MiB; want 202 within %d MiB", rec.Code, grew>>20, maxIngestDecodeHeap>>20)
			}
			pushNotified := outboxRows(t, path) > queued
			t.Logf("%d bytes: push grew the heap %d MiB (%d of %d reports reworded, notification queued: %v)", len(body), grew>>20, reworded, len(targets), pushNotified)

			// A knowledge-base update the next day: every evaluation is
			// outdated and every finding of each differs from the stored
			// one, so the pass decodes each stored report, loads it again
			// as the baseline, and inserts the new one.
			next := s.now().Add(24 * time.Hour)
			s.now = func() time.Time { return next }
			if n := rewordStoredFindings(t, path); n != 2*reworded {
				t.Fatalf("reworded %d evaluations, want %d", n, 2*reworded)
			}
			queued = outboxRows(t, path)
			grew = heapPeak(func() { s.reevaluateAll(context.Background()) })
			if grew > maxReevaluationHeap {
				t.Errorf("re-evaluation pass: the heap grew %d MiB, want at most %d MiB", grew>>20, maxReevaluationHeap>>20)
			}
			var inserted int64
			for _, target := range targets {
				e, err := s.cfg.Store.CurrentEvaluationSummary(context.Background(), 1, target)
				if err != nil || !e.EvaluatedAt.Equal(next) {
					t.Fatalf("after the pass, %s = (evaluated %v, %v), want re-evaluated at %v", target, e.EvaluatedAt, err, next)
				}
				if e.CreatedAt.Equal(next) {
					inserted++
				}
			}
			if inserted != reworded {
				t.Fatalf("the pass inserted %d evaluations, want the %d whose findings changed", inserted, reworded)
			}
			passNotified := outboxRows(t, path) > queued
			if pushNotified != passNotified {
				t.Errorf("the push queued a notification: %v, the pass: %v; want the same, from the same findings", pushNotified, passNotified)
			}
			t.Logf("re-evaluation pass grew the heap %d MiB (notification queued: %v)", grew>>20, passNotified)

			rec = httptest.NewRecorder()
			grew = heapPeak(func() {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/gate?target=1.35&fail-on=never&cluster=prod-eu-1", strings.NewReader(gate))
				req.Header.Set("Content-Type", "application/x-yaml")
				s.Handler().ServeHTTP(rec, req)
			})
			// Against a cluster whose report is this large, the answer
			// would be over the answer limit: 413, after the evaluation
			// that costs the heap measured here.
			if answered := rec.Code == http.StatusOK || rec.Code == http.StatusRequestEntityTooLarge; !answered || grew > maxGateDecodeHeap {
				t.Errorf("gate ?cluster=: status %d (%.300s), the heap grew %d MiB; want 200 or 413 within %d MiB", rec.Code, rec.Body, grew>>20, maxGateDecodeHeap>>20)
			}
			t.Logf("gate ?cluster=: status %d, grew the heap %d MiB", rec.Code, grew>>20)
		})
	}
}

// maxFleetReadHeap is what any number of concurrent fleet-wide reads may
// add to the heap: they take no slot, so each must cost what its response
// costs, never what a stored report costs.
const maxFleetReadHeap = 16 << 20

// The cluster list, the fleet matrix and /metrics take no read slot, and
// each loaded every current evaluation's whole report for its notAssessed:
// after one 17 MB push of PodSecurityPolicy usages (accepted from any
// ingest token; its reports are as large), 30 of any of them at once grew
// the heap by 285-584 MiB on SQLite (10 at once, 175-227 MiB), so ~20
// dashboard polls exceeded the chart's memory limit. They now read the
// summary columns only: 0-2 MiB for 30 at once.
func TestFleetReadsLoadNoReport(t *testing.T) {
	if testing.Short() || raceEnabled || !heapRun {
		t.Skip("stores a snapshot at the node budget; make test-heap runs it, without the race detector")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(10)) // as in TestGateDecodeHeapIsBounded
	for name, shape := range storedHeapShapes() {
		t.Run(name, func(t *testing.T) {
			s := newSQLiteTestServer(t)
			body := storedBody(name, shape)
			rec := httptest.NewRecorder()
			serveIngest(s, rec, []byte(body), false)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("push: status = %d (%.300s)", rec.Code, rec.Body)
			}
			for _, path := range []string{"/api/v1/clusters", "/api/v1/fleet", "/api/v1/fleet?targets=1.35,1.36,1.40", "/metrics"} {
				const n = 30
				grew, size := concurrentGets(t, s, path, n)
				if grew > maxFleetReadHeap {
					t.Errorf("%s x%d: the heap grew %d MiB, want at most %d MiB", path, n, grew>>20, maxFleetReadHeap>>20)
				}
				t.Logf("%d-byte push, %s x%d: heap grew %d MiB, response %d bytes", len(body), path, n, grew>>20, size)
			}
		})
	}
}
