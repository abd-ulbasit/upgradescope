package server

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// Ingest benchmark (#71, hack/bench/serve.sh): a fleet of 200 clusters
// pushing snapshots to a server evaluating 3 targets each, on SQLite or
// Postgres. Per round it records throughput, p50/p99 latency of a push
// (as an agent sees it, retries included), the database's growth per
// snapshot, the process's CPU time and peak live heap. Rounds, in order:
//
//	new        every cluster's first snapshot
//	changed    every cluster pushes a changed inventory (a new snapshot, evaluated for every target)
//	duplicate  every cluster pushes the same inventory again (the hourly force-sync)
//
// Off by default; knobs, all env:
//
//	UPGRADESCOPE_BENCH_INGEST=1         enable
//	UPGRADESCOPE_BENCH_BACKEND          sqlite (default) or postgres
//	UPGRADESCOPE_BENCH_PG_DSN           a postgres:// URL of a throwaway server (the test creates its own databases on it)
//	UPGRADESCOPE_BENCH_CONCURRENCY      comma list of concurrent pushers, default 1,16,200 (200 is every cluster at once)
//	UPGRADESCOPE_BENCH_CLUSTERS         default 200
//	UPGRADESCOPE_BENCH_OUT              a file that gets one JSON line per round
//
// The load generator runs in the server's process and talks to it over
// loopback; with Postgres the database is wherever the DSN points, so its
// round trips are in every number.
const ingestTargets = 3 // the default target (next minor) plus two --targets

// ingestTier is one size of cluster.
type ingestTier struct {
	name                                          string
	nodes, namespaces, releases, addOns, apiKinds int
	objectsPerKind                                int
}

// A fleet of mostly mid-sized clusters with a few big ones: the shares are
// 50% small, 40% medium, 10% large.
var ingestTiers = []ingestTier{
	{"small", 10, 30, 5, 6, 2, 5},
	{"medium", 40, 150, 25, 12, 4, 30},
	{"large", 400, 600, 100, 24, 8, 100},
}

func tierOf(cluster int) ingestTier {
	switch p := cluster % 10; {
	case p < 5:
		return ingestTiers[0]
	case p < 9:
		return ingestTiers[1]
	default:
		return ingestTiers[2]
	}
}

// fleetPlan picks, from the shipped knowledge base, what an inventory names:
// APIs removed by the targets (1.35 to 1.37) and add-ons at their oldest
// release line, so the evaluations carry findings like a real fleet's.
type fleetPlan struct {
	apis   []kb.APILifecycleEntry
	addOns []addOnPick
}

type addOnPick struct{ id, version string }

func newFleetPlan(k kb.KB) fleetPlan {
	var p fleetPlan
	for _, e := range k.APILifecycle {
		if e.Removed != nil && e.Removed.Major == 1 && e.Removed.Minor >= 35 && e.Removed.Minor <= 37 {
			p.apis = append(p.apis, e)
		}
	}
	for _, a := range k.AddOns {
		if len(a.Cycles) == 0 {
			continue
		}
		p.addOns = append(p.addOns, addOnPick{a.ID, a.Cycles[len(a.Cycles)-1].Cycle + ".0"})
	}
	return p
}

// fleetInventory is cluster i's inventory at round (0 first, then each
// change). A round changes a kubelet version and a Helm revision, so the
// hash differs and the push is a new snapshot.
func fleetInventory(p fleetPlan, i, round int) inventory.Inventory {
	tier := tierOf(i)
	inv := inventory.Inventory{
		SchemaVersion:   1,
		ClusterID:       fmt.Sprintf("uid-%04d", i),
		CollectorSchema: inventory.CurrentCollectorSchema,
		CollectedAt:     time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(round) * time.Hour),
		ServerVersion:   "v1.34.2",
		Source:          inventory.SourceCluster,
		Capabilities:    collectedCaps(),
		ControlPlane: []inventory.ComponentVersion{
			{Component: "kube-apiserver", Version: "v1.34.2"}, {Component: "kube-controller-manager", Version: "v1.34.2"},
			{Component: "kube-scheduler", Version: "v1.34.2"}, {Component: "kube-proxy", Version: "v1.34.2"},
		},
	}
	for n := range tier.nodes {
		v := "v1.34.2"
		if n == 0 && round > 0 {
			v = fmt.Sprintf("v1.34.%d", 2+round)
		}
		inv.Nodes = append(inv.Nodes, inventory.NodeInfo{Name: fmt.Sprintf("node-%04d-%03d", i, n), KubeletVersion: v, ContainerRuntime: "containerd://2.1.3"})
	}
	for n := range tier.namespaces {
		inv.Namespaces = append(inv.Namespaces, inventory.NamespaceInfo{Name: fmt.Sprintf("team-%02d-ns-%03d", n%12, n), Team: fmt.Sprintf("team-%02d", n%12)})
	}
	for n := range tier.releases {
		rev := 3
		if n == 0 {
			rev += round
		}
		inv.HelmReleases = append(inv.HelmReleases, inventory.HelmRelease{
			Name: fmt.Sprintf("release-%03d", n), Namespace: fmt.Sprintf("team-%02d-ns-%03d", n%12, n%tier.namespaces),
			ChartName: fmt.Sprintf("chart-%03d", n), ChartVersion: "1.2.3", AppVersion: "4.5.6", KubeVersion: ">=1.21.0-0",
			Status: "deployed", Revision: rev,
		})
	}
	for n := range min(tier.addOns, len(p.addOns)) {
		a := p.addOns[n]
		inv.AddOns = append(inv.AddOns, inventory.AddOnInstance{ID: a.id, Version: a.version, Namespaces: []string{"kube-system"}, Source: "image"})
	}
	for k := range min(tier.apiKinds, len(p.apis)) {
		e := p.apis[k]
		u := inventory.APIUsage{Group: e.Group, Version: e.Version, Kind: e.Kind, Count: tier.objectsPerKind, Namespaces: map[string]int{}}
		for o := range tier.objectsPerKind {
			ns := fmt.Sprintf("team-%02d-ns-%03d", o%12, o%tier.namespaces)
			u.Namespaces[ns]++
			u.Objects = append(u.Objects, inventory.ObjectRef{Namespace: ns, Name: fmt.Sprintf("object-%03d", o), Manager: "helm"})
		}
		inv.APIUsage = append(inv.APIUsage, u)
	}
	return inv
}

// ingestRound is one round's measurements, one JSON line each.
type ingestRound struct {
	Backend         string  `json:"backend"`
	Concurrency     int     `json:"concurrency"`
	Round           string  `json:"round"`
	Clusters        int     `json:"clusters"`
	Targets         int     `json:"targets"`
	Pushes          int     `json:"pushes"`
	Accepted        int     `json:"accepted"`   // 202, a new snapshot
	Duplicates      int     `json:"duplicates"` // 200, the same inventory as the last
	Failed          int     `json:"failed"`     // gave up after the agent's retries
	Retried         int     `json:"retried"`    // pushes that needed a retry (a 503 busy or a 429)
	Attempts        int     `json:"attempts"`
	Busy503         int     `json:"busy503"`         // attempts the server answered 503 "too many concurrent pushes"
	TransportErrors int     `json:"transportErrors"` // attempts that never got an answer (the load generator's connection, not the server's logic)
	WallSeconds     float64 `json:"wallSeconds"`
	PushesPerSec    float64 `json:"pushesPerSecond"`
	P50MS           float64 `json:"p50Ms"`
	P99MS           float64 `json:"p99Ms"`
	MaxMS           float64 `json:"maxMs"`
	CPUMSPerPush    float64 `json:"cpuMsPerPush"`
	PeakHeapMiB     float64 `json:"peakHeapMiB"`
	BodyBytesAvg    int     `json:"bodyBytesAvg"` // the JSON request body
	GzipBytesAvg    int     `json:"gzipBytesAvg"` // as the agent sends it
	DBBytesBefore   int64   `json:"dbBytesBefore"`
	DBBytesAfter    int64   `json:"dbBytesAfter"`
	DBBytesPerSnap  float64 `json:"dbBytesPerSnapshot"` // growth over the new snapshots (0 for the duplicate round)
	RTTMS           float64 `json:"postgresRttMs,omitempty"`
}

// percentile of sorted xs, nearest rank.
func percentile(xs []time.Duration, p float64) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	rank := int(float64(len(xs))*p/100 + 0.999999)
	return xs[min(max(rank, 1), len(xs))-1]
}

func TestBenchServeIngest(t *testing.T) {
	if os.Getenv("UPGRADESCOPE_BENCH_INGEST") != "1" {
		t.Skip("set UPGRADESCOPE_BENCH_INGEST=1 (hack/bench/serve.sh does) to run the ingest benchmark")
	}
	backend := cmpOr(os.Getenv("UPGRADESCOPE_BENCH_BACKEND"), "sqlite")
	clusters := benchEnvInt(t, "UPGRADESCOPE_BENCH_CLUSTERS", 200)
	var levels []int
	for _, s := range strings.Split(cmpOr(os.Getenv("UPGRADESCOPE_BENCH_CONCURRENCY"), "1,16,200"), ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 {
			t.Fatalf("UPGRADESCOPE_BENCH_CONCURRENCY entry %q is not a positive integer", s)
		}
		levels = append(levels, n)
	}
	var out *os.File
	if p := os.Getenv("UPGRADESCOPE_BENCH_OUT"); p != "" {
		var err error
		if out, err = os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
			t.Fatal(err)
		}
		defer out.Close()
	}
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	plan := newFleetPlan(k)
	t.Logf("fleet: %d clusters, %d removed-API kinds and %d add-ons to draw from, %s", clusters, len(plan.apis), len(plan.addOns), backend)

	for _, c := range levels {
		db := newBenchDB(t, backend)
		srv, err := New(Config{Store: db.store, KB: k, ExtraTargets: []string{"1.36", "1.37"}, IngestToken: "ingest-tok"})
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(srv.Handler())
		for _, round := range []struct {
			name  string
			round int // inventory generation; the duplicate round repeats the last
		}{{"new", 0}, {"changed", 1}, {"duplicate", 1}} {
			res := runIngestRound(t, ts, db, plan, backend, round.name, round.round, clusters, c)
			res.RTTMS = db.rttMS
			t.Logf("%s c=%-3d %-9s %4d pushes (%d new, %d dup, %d failed; %d retried: %d busy, %d conn errors) in %6.2fs = %6.1f/s; p50 %7.1fms p99 %8.1fms max %8.1fms; %.1f cpu-ms/push; heap %.0f MiB; body %d KiB (gzip %d KiB); db %d KiB -> %d KiB (%.1f KiB/snapshot)",
				backend, c, round.name, res.Pushes, res.Accepted, res.Duplicates, res.Failed, res.Retried, res.Busy503, res.TransportErrors, res.WallSeconds, res.PushesPerSec,
				res.P50MS, res.P99MS, res.MaxMS, res.CPUMSPerPush, res.PeakHeapMiB, res.BodyBytesAvg>>10, res.GzipBytesAvg>>10,
				res.DBBytesBefore>>10, res.DBBytesAfter>>10, res.DBBytesPerSnap/1024)
			if out != nil {
				line, _ := json.Marshal(res)
				fmt.Fprintf(out, "%s\n", line)
			}
			if res.Failed > 0 {
				t.Errorf("c=%d %s: %d pushes failed after the agent's retries", c, round.name, res.Failed)
			}
		}
		if rows := db.tableSizes(); rows != "" {
			t.Logf("%s c=%d table sizes: %s", backend, c, rows)
		}
		ts.Close()
		db.close()
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func benchEnvInt(t *testing.T, name string, def int) int {
	t.Helper()
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		t.Fatalf("%s=%q is not a positive integer", name, s)
	}
	return n
}

// pushJob is one cluster's body for a round.
type pushJob struct {
	cluster string
	gz      []byte
	raw     int
}

// runIngestRound pushes every cluster's inventory at generation gen through
// c concurrent pushers, each behaving as the agent does: gzip body, bearer
// token, up to three retries after a 429 or 5xx, waiting the longer of its
// backoff (1s, 2s, 4s) and the server's Retry-After.
func runIngestRound(t *testing.T, ts *httptest.Server, db *benchDB, plan fleetPlan, backend, name string, gen, clusters, c int) ingestRound {
	t.Helper()
	jobs := make([]pushJob, clusters)
	var rawTotal, gzTotal int
	for i := range jobs {
		inv := fleetInventory(plan, i, gen)
		invJSON, err := json.Marshal(inv)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(map[string]any{
			"schemaVersion": 1, "clusterName": fmt.Sprintf("cluster-%04d", i), "agentVersion": "v0.2.0-bench", "kbVersion": "agent-kb",
			"inventory": json.RawMessage(invJSON),
		})
		if err != nil {
			t.Fatal(err)
		}
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write(body)
		_ = zw.Close()
		jobs[i] = pushJob{cluster: fmt.Sprintf("cluster-%04d", i), gz: gz.Bytes(), raw: len(body)}
		rawTotal += len(body)
		gzTotal += gz.Len()
	}
	runtime.GC()
	before := db.size()
	sampler := startHeapSampler()
	cpu0 := cpuTime()
	start := time.Now()

	var (
		next                            atomic.Int64
		mu                              sync.Mutex
		lat                             []time.Duration
		accepted, dups, failed, retried int
		attempts, busyTotal, errTotal   int
		wg                              sync.WaitGroup
	)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: c + 4}, Timeout: 60 * time.Second}
	defer client.CloseIdleConnections()
	for range c {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(jobs) {
					return
				}
				began := time.Now()
				status, tries, busy, errs := pushWithRetries(client, ts.URL, jobs[i])
				d := time.Since(began)
				mu.Lock()
				attempts += tries
				busyTotal += busy
				errTotal += errs
				if tries > 1 {
					retried++
				}
				switch status {
				case http.StatusAccepted:
					accepted++
					lat = append(lat, d)
				case http.StatusOK:
					dups++
					lat = append(lat, d)
				default:
					failed++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	wall := time.Since(start)
	cpu := cpuTime() - cpu0
	peak := sampler.finish()
	after := db.size()

	slices.Sort(lat)
	res := ingestRound{
		Backend: backend, Concurrency: c, Round: name, Clusters: clusters, Targets: ingestTargets, Pushes: len(jobs),
		Accepted: accepted, Duplicates: dups, Failed: failed, Retried: retried, Attempts: attempts, Busy503: busyTotal, TransportErrors: errTotal,
		WallSeconds: wall.Seconds(), PushesPerSec: float64(len(jobs)-failed) / wall.Seconds(),
		P50MS: ms(percentile(lat, 50)), P99MS: ms(percentile(lat, 99)),
		CPUMSPerPush: ms(cpu) / float64(len(jobs)), PeakHeapMiB: float64(peak) / (1 << 20),
		BodyBytesAvg: rawTotal / len(jobs), GzipBytesAvg: gzTotal / len(jobs),
		DBBytesBefore: before, DBBytesAfter: after,
	}
	if len(lat) > 0 {
		res.MaxMS = ms(lat[len(lat)-1])
	}
	if accepted > 0 {
		res.DBBytesPerSnap = float64(after-before) / float64(accepted)
	}
	return res
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// pushWithRetries posts one body as the agent's pusher does and returns the
// final status and the attempts made.
func pushWithRetries(client *http.Client, base string, j pushJob) (status, tries, busy, transportErrs int) {
	for attempt := 0; ; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/snapshots", bytes.NewReader(j.gz))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "gzip")
		req.Header.Set("Authorization", "Bearer ingest-tok")
		resp, err := client.Do(req)
		tries++
		retryAfter := time.Duration(0)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			status = resp.StatusCode
			if s, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil {
				retryAfter = time.Duration(s) * time.Second
			}
			if status < 500 && status != http.StatusTooManyRequests && status != http.StatusRequestTimeout {
				return status, tries, busy, transportErrs
			}
			if status == http.StatusServiceUnavailable {
				busy++
			}
		} else {
			status = 0
			transportErrs++
		}
		if attempt == 3 {
			return status, tries, busy, transportErrs
		}
		time.Sleep(max(time.Second<<attempt, retryAfter))
	}
}

// benchDB is the store under test, with a way to read its size.
type benchDB struct {
	store   store.Store
	size    func() int64
	tables  func() string
	cleanup func()
	rttMS   float64
}

func (d *benchDB) tableSizes() string {
	if d.tables == nil {
		return ""
	}
	return d.tables()
}

func (d *benchDB) close() {
	_ = d.store.Close()
	if d.cleanup != nil {
		d.cleanup()
	}
}

// newBenchDB opens an empty database: a new SQLite file, or a new database
// on the Postgres at UPGRADESCOPE_BENCH_PG_DSN (dropped by close).
func newBenchDB(t *testing.T, backend string) *benchDB {
	t.Helper()
	switch backend {
	case "sqlite":
		path := filepath.Join(t.TempDir(), "bench.db")
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		return &benchDB{store: st, size: func() int64 { return fileSize(path) + fileSize(path+"-wal") }}
	case "postgres":
		return newBenchPostgres(t)
	}
	t.Fatalf("UPGRADESCOPE_BENCH_BACKEND=%q, want sqlite or postgres", backend)
	return nil
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func newBenchPostgres(t *testing.T) *benchDB {
	t.Helper()
	base := os.Getenv("UPGRADESCOPE_BENCH_PG_DSN")
	if base == "" {
		t.Fatal("UPGRADESCOPE_BENCH_PG_DSN is required for the postgres backend")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("UPGRADESCOPE_BENCH_PG_DSN must be a postgres:// URL")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("bench_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		admin.Close()
		t.Fatalf("create the benchmark's own database: %v", err)
	}
	u2 := *u
	u2.Path = "/" + name
	st, err := store.OpenPostgres(u2.String())
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", u2.String())
	if err != nil {
		t.Fatal(err)
	}
	// Round trip time to the database, which every statement pays.
	var rtts []time.Duration
	for range 30 {
		s := time.Now()
		var one int
		if err := db.QueryRow("SELECT 1").Scan(&one); err != nil {
			t.Fatal(err)
		}
		rtts = append(rtts, time.Since(s))
	}
	slices.Sort(rtts)
	return &benchDB{
		store: st,
		rttMS: ms(rtts[len(rtts)/2]),
		size: func() int64 {
			var n int64
			if err := db.QueryRow("SELECT pg_database_size(current_database())").Scan(&n); err != nil {
				t.Errorf("pg_database_size: %v", err)
			}
			return n
		},
		tables: func() string {
			rows, err := db.Query(`SELECT c.relname, pg_total_relation_size(c.oid) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = 'public' AND c.relkind = 'r' ORDER BY 2 DESC LIMIT 6`)
			if err != nil {
				return ""
			}
			defer rows.Close()
			var parts []string
			for rows.Next() {
				var name string
				var sz int64
				if rows.Scan(&name, &sz) == nil {
					parts = append(parts, fmt.Sprintf("%s %d KiB", name, sz>>10))
				}
			}
			return strings.Join(parts, ", ")
		},
		cleanup: func() {
			db.Close()
			if _, err := admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); err != nil {
				t.Logf("drop %s: %v", name, err)
			}
			admin.Close()
		},
	}
}
