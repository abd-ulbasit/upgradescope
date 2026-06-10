# upgradescope P2 — Continuous Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship continuous mode: `upgradescope agent` (in-cluster loop → evaluate → `ClusterReadiness` CRD status + push snapshots) and `upgradescope serve` (SQLite-backed ingest, REST API, history, what-if, Slack notifier), plus a Helm chart and gated e2e.

**Architecture:** Per spec §3/§7/§8 (docs/superpowers/specs/2026-06-10-upgradescope-design.md). Smart edge: the agent evaluates locally with its embedded engine/KB and never depends on server availability for the CRD surface. Server re-evaluates stored inventories for what-if/history. P1 packages (`collect`, `kb`, `engine`, `registry`, `inventory`) are complete and untouchable except where stated.

**Tech Stack:** Go 1.26, client-go dynamic client (CRD status, no kubebuilder), net/http + stdlib mux (server), modernc.org/sqlite (CGO-free) via database/sql, embedded SQL migrations, Helm chart in deploy/chart.

---

## File structure

```
internal/crd/manifest.yaml          # ClusterReadiness CRD (embedded; also installable)
internal/crd/types.go               # Go types for spec/status (JSON-tagged; no codegen)
internal/crd/apply.go               # EnsureCRD (apply manifest), ReadSpec, WriteStatus via dynamic client
internal/agent/agent.go             # Run(ctx, cfg): loop = collect→evaluate→CRD status→push
internal/agent/push.go              # snapshot push client: hash-dedup, gzip JSON, bearer, backoff
internal/cli/agent.go               # `upgradescope agent` cobra command (flags→agent.Run)
internal/server/store/store.go      # Store interface (the seam P3's Postgres implements)
internal/server/store/sqlite.go     # SQLite impl (modernc.org/sqlite)
internal/server/store/migrate.go    # embedded migrations runner
internal/server/store/migrations/0001_init.sql
internal/server/api.go              # HTTP handlers: ingest + read API + healthz
internal/server/server.go           # Server wiring: store, kb, notifier, http.Server, graceful stop
internal/server/whatif.go           # re-evaluate stored inventory for arbitrary target
internal/server/notify/notify.go    # Notifier interface + delta computation
internal/server/notify/slack.go     # Slack incoming-webhook notifier (+ generic webhook)
internal/cli/serve.go               # `upgradescope serve` cobra command
deploy/chart/                       # Helm chart: agent Deployment, RBAC, CRD, optional server
hack/demo/agent-e2e.sh              # extends kind demo: install chart, verify CRD status
```

## Shared contracts (normative — all sections code against these)

### ClusterReadiness CRD (`upgradescope.dev/v1alpha1`)

Cluster-scoped, singleton by convention (name `cluster`). No codegen; plain JSON-tagged Go
types + dynamic client with unstructured conversion (`runtime.DefaultUnstructuredConverter`).

```go
// internal/crd/types.go
package crd

const (
	Group    = "upgradescope.dev"
	Version  = "v1alpha1"
	Kind     = "ClusterReadiness"
	Plural   = "clusterreadinesses"
	Singular = "clusterreadiness"
	// DefaultName is the conventional singleton object name.
	DefaultName = "cluster"
)

type Spec struct {
	// Targets are Kubernetes minor versions to evaluate against, e.g. ["1.36","1.37"].
	// Empty → agent defaults to next minor above the observed server version.
	Targets []string `json:"targets,omitempty"`
}

type TargetStatus struct {
	Target        string         `json:"target"`            // "1.36"
	Score         int            `json:"score"`
	Ready         bool           `json:"ready"`
	Blockers      int            `json:"blockers"`
	Warnings      int            `json:"warnings"`
	Infos         int            `json:"infos"`
	ByCategory    map[string]int `json:"byCategory,omitempty"` // category → count
	TopFindings   []TopFinding   `json:"topFindings,omitempty"` // ≤20, severity-sorted
}

type TopFinding struct {
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Title       string `json:"title"`
	Remediation string `json:"remediation,omitempty"`
}

type Status struct {
	ObservedServerVersion string         `json:"observedServerVersion,omitempty"`
	KBVersion             string         `json:"kbVersion,omitempty"`
	LastEvaluated         metav1.Time    `json:"lastEvaluated,omitempty"`
	Targets               []TargetStatus `json:"targets,omitempty"`
	NotAssessed           []string       `json:"notAssessed,omitempty"` // "helm: secrets list forbidden"
	AgentVersion          string         `json:"agentVersion,omitempty"`
}
```

CRD manifest: status subresource enabled; printer columns: TARGET(s) summarized SCORE, READY,
AGE. `internal/crd/apply.go` exports:

```go
func EnsureCRD(ctx context.Context, apiext apiextensionsclient.Interface) error // server-side apply of embedded manifest
func ReadSpec(ctx context.Context, dyn dynamic.Interface, name string) (Spec, bool, error)
func EnsureObject(ctx context.Context, dyn dynamic.Interface, name string) error // create CR with empty spec if absent
func WriteStatus(ctx context.Context, dyn dynamic.Interface, name string, st Status) error // status subresource update w/ conflict retry
```

### Agent loop semantics (`internal/agent`)

```go
type Config struct {
	Interval      time.Duration // default 10m, min 1m
	ServerURL     string        // optional; "" = CRD-only mode
	ServerToken   string        // bearer for push
	ClusterName   string        // human label sent to server; default = ClusterID
	CRName        string        // default crd.DefaultName
	TeamLabel     string        // default "team"
	ForceSyncEvery time.Duration // default 1h: push even if hash unchanged
}
func Run(ctx context.Context, clients collect.Clients, dyn dynamic.Interface, apiext apiextensionsclient.Interface, k kb.KB, cfg Config) error
```

Each tick: collect → for each target (spec.Targets or default next-minor) evaluate →
WriteStatus (always, even if server unreachable) → push snapshot iff content hash changed or
ForceSyncEvery elapsed. Errors: log, count, continue (the loop never dies on a tick error).
Jitter ±10%. Graceful stop on ctx cancel. Targets resolved per-tick (spec can change).

### Snapshot push protocol (agent → server)

`POST {ServerURL}/api/v1/snapshots`, headers: `Authorization: Bearer <token>`,
`Content-Type: application/json`, `Content-Encoding: gzip`. Body (gzipped):

```json
{
  "schemaVersion": 1,
  "clusterName": "prod-eu-1",
  "agentVersion": "v0.2.0",
  "kbVersion": "k8s-1.36+registry-2026-06-10",
  "inventory": { ...inventory.Inventory wire format... }
}
```

Responses: 202 accepted `{"snapshotId": 123}`; 200 duplicate (hash match)
`{"snapshotId": <existing>, "duplicate": true}`; 401 bad token; 422 invalid body
(structured `{"error": "..."}`). Push client: 3 retries, exponential backoff capped 1m,
buffer latest-only (a newer snapshot replaces the pending one).

### Store interface (`internal/server/store`)

```go
type Store interface {
	UpsertCluster(ctx context.Context, c Cluster) (int64, error)        // by name; returns id
	InsertSnapshot(ctx context.Context, s Snapshot) (int64, bool, error) // (id, duplicate, err) — duplicate iff same cluster+hash as latest
	LatestSnapshot(ctx context.Context, clusterID int64) (Snapshot, error)
	ListClusters(ctx context.Context) ([]Cluster, error)
	GetCluster(ctx context.Context, id int64) (Cluster, error)
	InsertEvaluation(ctx context.Context, e Evaluation) (int64, error)
	LatestEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error)
	ScoreHistory(ctx context.Context, clusterID int64, target string, limit int) ([]ScorePoint, error)
	Close() error
}

type Cluster struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`      // unique
	ClusterUID string   `json:"clusterUid"` // inventory.ClusterID
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

type Snapshot struct {
	ID          int64     `json:"id"`
	ClusterID   int64     `json:"clusterId"`
	Hash        string    `json:"hash"` // sha256 of canonical inventory JSON
	KBVersion   string    `json:"kbVersion"`
	AgentVersion string   `json:"agentVersion"`
	ReceivedAt  time.Time `json:"receivedAt"`
	Inventory   []byte    `json:"-"` // raw canonical JSON
}

type Evaluation struct {
	ID         int64     `json:"id"`
	ClusterID  int64     `json:"clusterId"`
	SnapshotID int64     `json:"snapshotId"`
	Target     string    `json:"target"`
	KBVersion  string    `json:"kbVersion"`
	Score      int       `json:"score"`
	Ready      bool      `json:"ready"`
	Blockers   int       `json:"blockers"`
	Warnings   int       `json:"warnings"`
	Report     []byte    `json:"-"` // full engine.Report JSON
	CreatedAt  time.Time `json:"createdAt"`
}

type ScorePoint struct {
	At    time.Time `json:"at"`
	Score int       `json:"score"`
	Ready bool      `json:"ready"`
}
```

SQLite schema (0001_init.sql): tables clusters(id, name UNIQUE, cluster_uid, first_seen,
last_seen), snapshots(id, cluster_id FK, hash, kb_version, agent_version, received_at,
inventory BLOB; INDEX cluster_id, received_at), evaluations(id, cluster_id FK, snapshot_id FK,
target, kb_version, score, ready, blockers, warnings, report BLOB, created_at; INDEX
cluster_id+target+created_at). WAL mode. Driver: modernc.org/sqlite (CGO-free).

### Server behavior

On accepted (non-duplicate) snapshot: evaluate against the snapshot's default target (next
minor above inventory server version) AND any targets in the `targets` query/config, store
Evaluations, run notifier delta vs previous evaluation of same cluster+target. What-if
(`?target=`) evaluates on demand from the latest snapshot (server's own KB), not stored.

### REST API (read side)

```
GET /healthz                                     → 200 {"status":"ok"}
GET /api/v1/clusters                             → [Cluster + latest score summary]
GET /api/v1/clusters/{id}                        → Cluster detail + capabilities + latest eval summaries
GET /api/v1/clusters/{id}/report?target=1.37     → full engine.Report (stored if exists, else what-if)
GET /api/v1/clusters/{id}/findings?target=&severity=&category= → filtered findings list
GET /api/v1/clusters/{id}/history?target=&limit=  → []ScorePoint (default limit 100)
```

Auth: ingest endpoint requires `--ingest-token` bearer (P2: one shared token; per-cluster
tokens are P3). Read API: `--read-token` optional (empty = open; document loudly).
Errors: JSON `{"error": "..."}` with correct status codes. stdlib `http.ServeMux`
(Go 1.22+ method patterns), no router dependency.

### Notifier (`internal/server/notify`)

```go
type Event struct {
	Cluster  string
	Target   string
	Kind     string // "new-blocker" | "eol-approaching" | "became-ready"
	Title    string // human line, e.g. finding title
	Detail   string
}
type Notifier interface{ Notify(ctx context.Context, ev Event) error }
```

Delta rule (computed in server, not notifier): diff finding titles of severity blocker
between previous and new evaluation of (cluster, target): added → new-blocker event each
(cap 5 per evaluation, then one "and N more"); blockers went >0 → 0 → became-ready;
new eol-approaching warnings → eol-approaching. Slack notifier: incoming webhook URL,
plain-text payload `{"text": "..."}`. Generic webhook: POST Event JSON. Failures logged,
never block ingestion. No notification on a cluster's first-ever evaluation.

### CLI

```
upgradescope agent --interval 10m --server-url URL --server-token T --cluster-name N
                   --cr-name cluster --team-label team [--kubeconfig|in-cluster auto]
upgradescope serve --listen :8080 --db /var/lib/upgradescope/db.sqlite
                   --ingest-token T [--read-token T] [--slack-webhook URL] [--webhook URL]
                   [--targets 1.37,1.38]   # extra targets evaluated on every snapshot
```

Both: graceful shutdown on SIGINT/SIGTERM. Agent auto-detects in-cluster config
(rest.InClusterConfig) and falls back to kubeconfig (same loading as scan).

### Helm chart (deploy/chart)

Chart `upgradescope`: CRD in crds/, agent Deployment (read-only RBAC ClusterRole per spec §7
+ get/list/watch on the CRD + update on its status + create for EnsureObject + nonResourceURL
/metrics get; apiextensions create/get/update for EnsureCRD), ServiceAccount, values:
image, interval, serverUrl, serverToken (existingSecret), targets, teamLabel. Optional
`server.enabled` subchart-style block: server Deployment + PVC + Service. Single-cluster
combined install = both enabled.

---

## Section: internal/crd + internal/agent

Covers `internal/crd/{manifest.yaml,types.go,apply.go}`, `internal/agent/{agent.go,push.go}`, `internal/cli/agent.go` + root wiring. Task prefix **A**.

**New dependency:** `k8s.io/apiextensions-apiserver@v0.36.1` (matches the client-go pin). Added in Task A1 step 1. No kubebuilder, no controller-runtime — dynamic client + apiextensions clientset only, per plan tech stack.

**Ordering:** A1 → A2 → A3 → A4 build `internal/crd` bottom-up. A5 (push client) only needs the module. A6 → A7 → A8 build `internal/agent` on top of crd + push. A9 wires the CLI. Do them in order; each task compiles and passes `go test ./...` at its commit.

**Design notes (decisions this section bakes in):**

- **EnsureCRD = create-or-update, not SSA.** The fake apiextensions clientset has unreliable server-side-apply support; create → on `AlreadyExists` get + carry `resourceVersion` + update (wrapped in `retry.RetryOnConflict`) gives the same converge-to-manifest semantics and is fully testable. The manifest is the single source of truth; drift is overwritten.
- **Status construction lives in `internal/crd/types.go`** (`TargetStatusFromReport`, `StatusFromReports`) — the file-structure law allows only three crd files, and this is type-shaping logic. `crd` therefore imports `engine` (one-way, no cycle: `engine` never imports `crd`).
- **Snapshot hash zeroes `CollectedAt`.** `inventory.CollectedAt` changes every tick; hashing it would defeat dedup entirely. `snapshotHash` marshals a copy with `CollectedAt = time.Time{}` as the canonical form. **Cross-section note for STORE/SERVER-API:** the server must canonicalize identically (zero `collectedAt` before hashing) or force-sync pushes will never dedup to `200 duplicate`.
- **Default-target caveat:** `inventory.ParseVersion` rejects vendor suffixes (`v1.34.2-gke.100`, per P1 tests). On such distros default-target resolution fails and the agent reports it in `status.notAssessed`; users set `spec.targets` explicitly. Acceptable for P2; revisit in P3 if it bites.
- **`agent.AgentVersion`** is a package var (default `"dev"`) because the normative `Config`/`Run` signatures have no version field; `cli/agent.go` stamps the build version into it.

---

### Task A1: apiextensions dependency + embedded CRD manifest + EnsureCRD

**Files:**
- Create: `internal/crd/manifest.yaml`
- Create: `internal/crd/apply.go`
- Test: `internal/crd/apply_test.go`

- [ ] **Step 1: Add the apiextensions-apiserver dependency**

```bash
go get k8s.io/apiextensions-apiserver@v0.36.1
```

Expected output includes: `go: added k8s.io/apiextensions-apiserver v0.36.1`. Do NOT run `go mod tidy` until this task's implementation step imports the package (tidy would prune it).

- [ ] **Step 2: Create the CRD manifest**

Create `internal/crd/manifest.yaml`:

```yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: clusterreadinesses.upgradescope.dev
spec:
  group: upgradescope.dev
  scope: Cluster
  names:
    plural: clusterreadinesses
    singular: clusterreadiness
    kind: ClusterReadiness
    listKind: ClusterReadinessList
  versions:
    - name: v1alpha1
      served: true
      storage: true
      subresources:
        status: {}
      additionalPrinterColumns:
        - name: Target
          type: string
          jsonPath: .status.targets[0].target
        - name: Score
          type: integer
          jsonPath: .status.targets[0].score
        - name: Ready
          type: boolean
          jsonPath: .status.targets[0].ready
        - name: Age
          type: date
          jsonPath: .metadata.creationTimestamp
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                targets:
                  description: Kubernetes minor versions to evaluate against, e.g. "1.36". Empty means the agent targets the next minor above the observed server version.
                  type: array
                  items:
                    type: string
                    pattern: '^[0-9]+\.[0-9]+$'
            status:
              type: object
              properties:
                observedServerVersion:
                  type: string
                kbVersion:
                  type: string
                lastEvaluated:
                  type: string
                  format: date-time
                agentVersion:
                  type: string
                notAssessed:
                  type: array
                  items:
                    type: string
                targets:
                  type: array
                  items:
                    type: object
                    required: [target, score, ready, blockers, warnings, infos]
                    properties:
                      target:
                        type: string
                      score:
                        type: integer
                      ready:
                        type: boolean
                      blockers:
                        type: integer
                      warnings:
                        type: integer
                      infos:
                        type: integer
                      byCategory:
                        type: object
                        additionalProperties:
                          type: integer
                      topFindings:
                        type: array
                        maxItems: 20
                        items:
                          type: object
                          required: [category, severity, title]
                          properties:
                            category:
                              type: string
                            severity:
                              type: string
                            title:
                              type: string
                            remediation:
                              type: string
```

Server-side printing supports indexed JSONPaths (`.status.targets[0].score`), not wildcards — hence "first target summarized" printer columns, which is the singleton common case.

- [ ] **Step 3: Write failing tests for manifest validity and EnsureCRD semantics**

Create `internal/crd/apply_test.go`:

```go
package crd

import (
	"context"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const crdName = "clusterreadinesses.upgradescope.dev"

func parseManifest(t *testing.T) apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	var c apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(Manifest, &c); err != nil {
		t.Fatalf("embedded manifest does not parse as a v1 CRD: %v", err)
	}
	return c
}

func TestManifestShape(t *testing.T) {
	c := parseManifest(t)
	if c.Name != crdName {
		t.Errorf("name = %q, want %q", c.Name, crdName)
	}
	if c.Spec.Group != "upgradescope.dev" {
		t.Errorf("group = %q, want upgradescope.dev", c.Spec.Group)
	}
	if c.Spec.Scope != apiextensionsv1.ClusterScoped {
		t.Errorf("scope = %q, want Cluster", c.Spec.Scope)
	}
	if c.Spec.Names.Plural != "clusterreadinesses" || c.Spec.Names.Kind != "ClusterReadiness" {
		t.Errorf("names = %+v, want plural clusterreadinesses kind ClusterReadiness", c.Spec.Names)
	}
	if len(c.Spec.Versions) != 1 || c.Spec.Versions[0].Name != "v1alpha1" {
		t.Fatalf("versions = %+v, want exactly v1alpha1", c.Spec.Versions)
	}
	v := c.Spec.Versions[0]
	if !v.Served || !v.Storage {
		t.Error("v1alpha1 must be served and storage")
	}
	if v.Subresources == nil || v.Subresources.Status == nil {
		t.Error("status subresource not enabled")
	}
	cols := map[string]bool{}
	for _, pc := range v.AdditionalPrinterColumns {
		cols[pc.Name] = true
	}
	for _, want := range []string{"Target", "Score", "Ready", "Age"} {
		if !cols[want] {
			t.Errorf("printer column %q missing (have %v)", want, cols)
		}
	}
	if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
		t.Fatal("openAPIV3Schema missing")
	}
	if _, ok := v.Schema.OpenAPIV3Schema.Properties["status"]; !ok {
		t.Error("schema has no status property")
	}
}

func TestEnsureCRDCreates(t *testing.T) {
	ctx := context.Background()
	fc := apiextfake.NewSimpleClientset()
	if err := EnsureCRD(ctx, fc); err != nil {
		t.Fatalf("EnsureCRD: %v", err)
	}
	got, err := fc.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("CRD not created: %v", err)
	}
	if got.Spec.Scope != apiextensionsv1.ClusterScoped {
		t.Errorf("created CRD scope = %q, want Cluster", got.Spec.Scope)
	}
}

func TestEnsureCRDUpdatesExisting(t *testing.T) {
	ctx := context.Background()
	fc := apiextfake.NewSimpleClientset()
	if err := EnsureCRD(ctx, fc); err != nil {
		t.Fatalf("first EnsureCRD: %v", err)
	}
	// Simulate drift: wipe printer columns on the stored object.
	got, err := fc.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got.Spec.Versions[0].AdditionalPrinterColumns = nil
	if _, err := fc.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, got, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Second EnsureCRD must take the AlreadyExists → update path and reconcile.
	if err := EnsureCRD(ctx, fc); err != nil {
		t.Fatalf("second EnsureCRD: %v", err)
	}
	got2, err := fc.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got2.Spec.Versions[0].AdditionalPrinterColumns) == 0 {
		t.Error("EnsureCRD did not restore printer columns on the update path")
	}
}
```

- [ ] **Step 4: Run tests, expect compile failure**

```bash
go test ./internal/crd/
```

Expected: `undefined: Manifest` and `undefined: EnsureCRD`.

- [ ] **Step 5: Implement apply.go (embed + EnsureCRD)**

Create `internal/crd/apply.go`:

```go
// Package crd owns the ClusterReadiness custom resource: its embedded CRD
// manifest, plain JSON-tagged Go types (no codegen), and apply/status logic
// via the dynamic client.
package crd

import (
	"context"
	_ "embed"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"
)

// Manifest is the embedded ClusterReadiness CRD manifest. It is the single
// source of truth: EnsureCRD converges the cluster to it, and the Helm chart
// ships a copy in crds/.
//
//go:embed manifest.yaml
var Manifest []byte

// EnsureCRD installs or updates the ClusterReadiness CRD from the embedded
// manifest. Create-or-update semantics: existing CRDs are overwritten with
// the manifest's spec (conflicts retried). Callers may treat failure as
// non-fatal when the CRD is pre-installed out of band (e.g. Helm crds/).
func EnsureCRD(ctx context.Context, apiext apiextensionsclient.Interface) error {
	var want apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(Manifest, &want); err != nil {
		return fmt.Errorf("parse embedded CRD manifest: %w", err)
	}
	crds := apiext.ApiextensionsV1().CustomResourceDefinitions()
	_, err := crds.Create(ctx, &want, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create ClusterReadiness CRD: %w", err)
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, gerr := crds.Get(ctx, want.Name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		updated := want.DeepCopy()
		updated.ResourceVersion = existing.ResourceVersion
		_, uerr := crds.Update(ctx, updated, metav1.UpdateOptions{})
		return uerr
	})
	if err != nil {
		return fmt.Errorf("update ClusterReadiness CRD: %w", err)
	}
	return nil
}
```

- [ ] **Step 6: Run tests, expect pass; tidy now that the import exists**

```bash
go test ./internal/crd/ && go mod tidy && go build ./...
```

Expected: `ok  github.com/abd-ulbasit/upgradescope/internal/crd`. `go.mod` keeps `k8s.io/apiextensions-apiserver v0.36.1` as a direct require.

- [ ] **Step 7: Commit**

```bash
git add internal/crd/ go.mod go.sum && git commit -m "feat(crd): embedded ClusterReadiness CRD manifest and EnsureCRD" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task A2: ClusterReadiness Go types + GVR

**Files:**
- Create: `internal/crd/types.go`
- Test: `internal/crd/types_test.go`

- [ ] **Step 1: Write failing tests for constants, GVR, and JSON round-trip**

Create `internal/crd/types_test.go`:

```go
package crd

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestConstantsMatchManifest(t *testing.T) {
	c := parseManifest(t)
	if c.Spec.Group != Group {
		t.Errorf("Group const %q != manifest group %q", Group, c.Spec.Group)
	}
	if c.Spec.Names.Plural != Plural || c.Spec.Names.Singular != Singular || c.Spec.Names.Kind != Kind {
		t.Errorf("names consts (%s/%s/%s) != manifest names %+v", Plural, Singular, Kind, c.Spec.Names)
	}
	if c.Spec.Versions[0].Name != Version {
		t.Errorf("Version const %q != manifest version %q", Version, c.Spec.Versions[0].Name)
	}
	if DefaultName != "cluster" {
		t.Errorf("DefaultName = %q, want cluster", DefaultName)
	}
}

func TestGVR(t *testing.T) {
	want := schema.GroupVersionResource{Group: "upgradescope.dev", Version: "v1alpha1", Resource: "clusterreadinesses"}
	if got := GVR(); got != want {
		t.Errorf("GVR() = %v, want %v", got, want)
	}
}

func TestStatusJSONRoundTrip(t *testing.T) {
	st := Status{
		ObservedServerVersion: "v1.35.2",
		KBVersion:             "k8s-1.36+registry-2026-06-10",
		LastEvaluated:         metav1.NewTime(time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)),
		Targets: []TargetStatus{{
			Target:     "1.36",
			Score:      87,
			Ready:      false,
			Blockers:   1,
			Warnings:   2,
			Infos:      3,
			ByCategory: map[string]int{"removed-api": 1, "eol-approaching": 2},
			TopFindings: []TopFinding{{
				Category:    "removed-api",
				Severity:    "blocker",
				Title:       "flowcontrol.apiserver.k8s.io/v1beta3 FlowSchema removed in 1.32",
				Remediation: "migrate to flowcontrol.apiserver.k8s.io/v1",
			}},
		}},
		NotAssessed:  []string{"helm: secrets list forbidden"},
		AgentVersion: "v0.2.0",
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"observedServerVersion"`, `"kbVersion"`, `"lastEvaluated"`, `"targets"`,
		`"target"`, `"score"`, `"ready"`, `"blockers"`, `"warnings"`, `"infos"`,
		`"byCategory"`, `"topFindings"`, `"category"`, `"severity"`, `"title"`,
		`"remediation"`, `"notAssessed"`, `"agentVersion"`,
	} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("marshaled status missing key %s in %s", key, raw)
		}
	}
	var back Status
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(st, back) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", back, st)
	}
}

func TestSpecJSONOmitsEmptyTargets(t *testing.T) {
	raw, err := json.Marshal(Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{}" {
		t.Errorf("empty Spec marshals to %s, want {}", raw)
	}
}
```

- [ ] **Step 2: Run tests, expect compile failure**

```bash
go test ./internal/crd/
```

Expected: `undefined: Group`, `undefined: GVR`, `undefined: Status`, etc.

- [ ] **Step 3: Implement types.go (contract types verbatim + GVR)**

Create `internal/crd/types.go`:

```go
package crd

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	Group    = "upgradescope.dev"
	Version  = "v1alpha1"
	Kind     = "ClusterReadiness"
	Plural   = "clusterreadinesses"
	Singular = "clusterreadiness"
	// DefaultName is the conventional singleton object name.
	DefaultName = "cluster"
)

// GVR is the dynamic-client resource identifier for ClusterReadiness.
func GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: Group, Version: Version, Resource: Plural}
}

type Spec struct {
	// Targets are Kubernetes minor versions to evaluate against, e.g. ["1.36","1.37"].
	// Empty → agent defaults to next minor above the observed server version.
	Targets []string `json:"targets,omitempty"`
}

type TargetStatus struct {
	Target      string         `json:"target"` // "1.36"
	Score       int            `json:"score"`
	Ready       bool           `json:"ready"`
	Blockers    int            `json:"blockers"`
	Warnings    int            `json:"warnings"`
	Infos       int            `json:"infos"`
	ByCategory  map[string]int `json:"byCategory,omitempty"`  // category → count
	TopFindings []TopFinding   `json:"topFindings,omitempty"` // ≤20, severity-sorted
}

type TopFinding struct {
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Title       string `json:"title"`
	Remediation string `json:"remediation,omitempty"`
}

type Status struct {
	ObservedServerVersion string         `json:"observedServerVersion,omitempty"`
	KBVersion             string         `json:"kbVersion,omitempty"`
	LastEvaluated         metav1.Time    `json:"lastEvaluated,omitempty"`
	Targets               []TargetStatus `json:"targets,omitempty"`
	NotAssessed           []string       `json:"notAssessed,omitempty"` // "helm: secrets list forbidden"
	AgentVersion          string         `json:"agentVersion,omitempty"`
}
```

- [ ] **Step 4: Run tests, expect pass**

```bash
go test ./internal/crd/
```

- [ ] **Step 5: Commit**

```bash
git add internal/crd/ && git commit -m "feat(crd): ClusterReadiness spec/status types and GVR" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task A3: Status construction from engine.Report

**Files:**
- Modify: `internal/crd/types.go`
- Test: `internal/crd/status_test.go` (new test file, still package crd — file-structure law constrains non-test files only)

- [ ] **Step 1: Write failing tests for counts, top-20 cap, and notAssessed strings**

Create `internal/crd/status_test.go`:

```go
package crd

import (
	"fmt"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestTargetStatusFromReportCounts(t *testing.T) {
	r := engine.Report{
		Target: inventory.Version{Major: 1, Minor: 36},
		Score:  72,
		Ready:  false,
		Findings: []engine.Finding{
			{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Title: "b1", Remediation: "fix b1"},
			{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Title: "b2"},
			{Category: engine.CatEOLApproaching, Severity: engine.SevWarning, Title: "w1"},
			{Category: engine.CatDeprecatedAPI, Severity: engine.SevInfo, Title: "i1"},
		},
	}
	ts := TargetStatusFromReport(r)
	if ts.Target != "1.36" {
		t.Errorf("Target = %q, want 1.36", ts.Target)
	}
	if ts.Score != 72 || ts.Ready {
		t.Errorf("Score/Ready = %d/%v, want 72/false", ts.Score, ts.Ready)
	}
	if ts.Blockers != 2 || ts.Warnings != 1 || ts.Infos != 1 {
		t.Errorf("counts = %d/%d/%d, want 2/1/1", ts.Blockers, ts.Warnings, ts.Infos)
	}
	wantByCat := map[string]int{"removed-api": 2, "eol-approaching": 1, "deprecated-api": 1}
	for k, v := range wantByCat {
		if ts.ByCategory[k] != v {
			t.Errorf("ByCategory[%s] = %d, want %d", k, ts.ByCategory[k], v)
		}
	}
	if len(ts.TopFindings) != 4 {
		t.Fatalf("TopFindings len = %d, want 4", len(ts.TopFindings))
	}
	if ts.TopFindings[0].Title != "b1" || ts.TopFindings[0].Remediation != "fix b1" || ts.TopFindings[0].Severity != "blocker" {
		t.Errorf("TopFindings[0] = %+v, want b1 blocker with remediation", ts.TopFindings[0])
	}
}

func TestTargetStatusFromReportTop20Cap(t *testing.T) {
	r := engine.Report{Target: inventory.Version{Major: 1, Minor: 36}}
	for i := 0; i < 25; i++ {
		r.Findings = append(r.Findings, engine.Finding{
			Category: engine.CatDeprecatedAPI,
			Severity: engine.SevWarning,
			Title:    fmt.Sprintf("finding-%02d", i),
		})
	}
	ts := TargetStatusFromReport(r)
	if len(ts.TopFindings) != 20 {
		t.Fatalf("TopFindings len = %d, want 20 (cap)", len(ts.TopFindings))
	}
	// Report.Findings is severity-sorted per engine contract; the cap keeps the head.
	if ts.TopFindings[0].Title != "finding-00" || ts.TopFindings[19].Title != "finding-19" {
		t.Errorf("cap kept wrong findings: first=%q last=%q", ts.TopFindings[0].Title, ts.TopFindings[19].Title)
	}
	if ts.Warnings != 25 {
		t.Errorf("Warnings = %d, want 25 (counts are not capped)", ts.Warnings)
	}
}

func TestStatusFromReports(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	reports := []engine.Report{
		{
			Target:    inventory.Version{Major: 1, Minor: 36},
			KBVersion: "k8s-1.36+registry-2026-06-10",
			Score:     90,
			Ready:     true,
			NotAssessed: []engine.CapabilityGap{
				{Capability: inventory.CapHelm, Reason: "secrets list forbidden"},
				{Capability: inventory.CapDeprecatedCalls, Reason: "metrics scrape forbidden"},
			},
		},
		{Target: inventory.Version{Major: 1, Minor: 37}, KBVersion: "k8s-1.36+registry-2026-06-10", Score: 70},
	}
	st := StatusFromReports(reports, "v1.35.2", "v0.2.0", now)
	if st.ObservedServerVersion != "v1.35.2" {
		t.Errorf("ObservedServerVersion = %q", st.ObservedServerVersion)
	}
	if st.KBVersion != "k8s-1.36+registry-2026-06-10" {
		t.Errorf("KBVersion = %q", st.KBVersion)
	}
	if st.AgentVersion != "v0.2.0" {
		t.Errorf("AgentVersion = %q", st.AgentVersion)
	}
	if !st.LastEvaluated.Time.Equal(now) {
		t.Errorf("LastEvaluated = %v, want %v", st.LastEvaluated, now)
	}
	if len(st.Targets) != 2 || st.Targets[0].Target != "1.36" || st.Targets[1].Target != "1.37" {
		t.Fatalf("Targets = %+v, want [1.36 1.37]", st.Targets)
	}
	want := []string{"helm: secrets list forbidden", "deprecated-calls: metrics scrape forbidden"}
	if len(st.NotAssessed) != len(want) || st.NotAssessed[0] != want[0] || st.NotAssessed[1] != want[1] {
		t.Errorf("NotAssessed = %v, want %v", st.NotAssessed, want)
	}
}

func TestStatusFromReportsEmpty(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	st := StatusFromReports(nil, "v1.35.2", "v0.2.0", now)
	if len(st.Targets) != 0 || len(st.NotAssessed) != 0 || st.KBVersion != "" {
		t.Errorf("empty reports produced non-empty derived fields: %+v", st)
	}
	if st.ObservedServerVersion != "v1.35.2" || st.AgentVersion != "v0.2.0" {
		t.Errorf("base fields missing: %+v", st)
	}
}
```

(Check the exact `inventory.Capability` constant names against `internal/inventory/types.go` — `CapHelm` is `"helm"`, `CapDeprecatedCalls` is `"deprecated-calls"` per P1.)

- [ ] **Step 2: Run tests, expect compile failure**

```bash
go test ./internal/crd/
```

Expected: `undefined: TargetStatusFromReport`, `undefined: StatusFromReports`.

- [ ] **Step 3: Implement status construction in types.go**

Append to `internal/crd/types.go` (add `fmt`, `time`, and the engine import):

```go
// maxTopFindings bounds CRD status size; the full list lives in server/CLI.
const maxTopFindings = 20

// TargetStatusFromReport summarizes one engine.Report for CRD status:
// severity/category counts over all findings, plus the first maxTopFindings
// findings (Report.Findings is already severity-sorted per engine contract).
func TargetStatusFromReport(r engine.Report) TargetStatus {
	ts := TargetStatus{Target: r.Target.String(), Score: r.Score, Ready: r.Ready}
	for _, f := range r.Findings {
		switch f.Severity {
		case engine.SevBlocker:
			ts.Blockers++
		case engine.SevWarning:
			ts.Warnings++
		case engine.SevInfo:
			ts.Infos++
		}
		if ts.ByCategory == nil {
			ts.ByCategory = make(map[string]int)
		}
		ts.ByCategory[string(f.Category)]++
		if len(ts.TopFindings) < maxTopFindings {
			ts.TopFindings = append(ts.TopFindings, TopFinding{
				Category:    string(f.Category),
				Severity:    string(f.Severity),
				Title:       f.Title,
				Remediation: f.Remediation,
			})
		}
	}
	return ts
}

// StatusFromReports builds the full CRD status from per-target reports.
// All reports come from the same inventory, so KBVersion and NotAssessed are
// taken from the first report. NotAssessed gaps render as "capability: reason".
func StatusFromReports(reports []engine.Report, observedServerVersion, agentVersion string, now time.Time) Status {
	st := Status{
		ObservedServerVersion: observedServerVersion,
		LastEvaluated:         metav1.NewTime(now.UTC()),
		AgentVersion:          agentVersion,
	}
	for _, r := range reports {
		st.Targets = append(st.Targets, TargetStatusFromReport(r))
	}
	if len(reports) > 0 {
		st.KBVersion = reports[0].KBVersion
		for _, g := range reports[0].NotAssessed {
			st.NotAssessed = append(st.NotAssessed, fmt.Sprintf("%s: %s", g.Capability, g.Reason))
		}
	}
	return st
}
```

The import block of `types.go` becomes:

```go
import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
)
```

- [ ] **Step 4: Run tests, expect pass**

```bash
go test ./internal/crd/
```

Note: `TestStatusFromReports` LastEvaluated check uses `.Time.Equal(now)` because `metav1.NewTime` truncates to seconds via RFC3339 on round trips — direct construction keeps nanos, `Equal` on `time.Time` handles it. If it fails on precision, change the fixture `now` to a whole-second time (it already is).

- [ ] **Step 5: Commit**

```bash
git add internal/crd/ && git commit -m "feat(crd): status construction from engine reports with top-20 cap" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task A4: ReadSpec, EnsureObject, WriteStatus via dynamic client

**Files:**
- Modify: `internal/crd/apply.go`
- Test: `internal/crd/object_test.go`

- [ ] **Step 1: Write failing tests using the dynamic fake**

Create `internal/crd/object_test.go`:

```go
package crd

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func newDynFake(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{GVR(): Kind + "List"},
		objects...,
	)
}

func newCRObject(name string, targets ...string) *unstructured.Unstructured {
	spec := map[string]interface{}{}
	if len(targets) > 0 {
		list := make([]interface{}, len(targets))
		for i, s := range targets {
			list[i] = s
		}
		spec["targets"] = list
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": Group + "/" + Version,
		"kind":       Kind,
		"metadata":   map[string]interface{}{"name": name},
		"spec":       spec,
	}}
}

func TestReadSpecNotFound(t *testing.T) {
	dyn := newDynFake()
	spec, found, err := ReadSpec(context.Background(), dyn, DefaultName)
	if err != nil {
		t.Fatalf("ReadSpec on absent object: %v", err)
	}
	if found {
		t.Error("found = true for absent object")
	}
	if len(spec.Targets) != 0 {
		t.Errorf("spec = %+v, want zero value", spec)
	}
}

func TestReadSpecTargets(t *testing.T) {
	dyn := newDynFake(newCRObject(DefaultName, "1.36", "1.37"))
	spec, found, err := ReadSpec(context.Background(), dyn, DefaultName)
	if err != nil || !found {
		t.Fatalf("ReadSpec: found=%v err=%v", found, err)
	}
	if want := []string{"1.36", "1.37"}; !reflect.DeepEqual(spec.Targets, want) {
		t.Errorf("Targets = %v, want %v", spec.Targets, want)
	}
}

func TestEnsureObjectCreatesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dyn := newDynFake()
	if err := EnsureObject(ctx, dyn, DefaultName); err != nil {
		t.Fatalf("EnsureObject: %v", err)
	}
	obj, err := dyn.Resource(GVR()).Get(ctx, DefaultName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("object not created: %v", err)
	}
	if obj.GetKind() != Kind {
		t.Errorf("kind = %q, want %q", obj.GetKind(), Kind)
	}
	// Second call must not error on AlreadyExists and must not clobber spec.
	if _, err := dyn.Resource(GVR()).Update(ctx, newCRObject(DefaultName, "1.37"), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsureObject(ctx, dyn, DefaultName); err != nil {
		t.Fatalf("second EnsureObject: %v", err)
	}
	spec, _, err := ReadSpec(ctx, dyn, DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"1.37"}; !reflect.DeepEqual(spec.Targets, want) {
		t.Errorf("EnsureObject clobbered spec: %v, want %v", spec.Targets, want)
	}
}

func TestWriteStatus(t *testing.T) {
	ctx := context.Background()
	dyn := newDynFake(newCRObject(DefaultName))
	st := Status{
		ObservedServerVersion: "v1.35.2",
		KBVersion:             "kb-v",
		LastEvaluated:         metav1.NewTime(time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)),
		Targets:               []TargetStatus{{Target: "1.36", Score: 88, Ready: true}},
		AgentVersion:          "v0.2.0",
	}
	if err := WriteStatus(ctx, dyn, DefaultName, st); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}
	obj, err := dyn.Resource(GVR()).Get(ctx, DefaultName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil || !found {
		t.Fatalf("status not written: found=%v err=%v", found, err)
	}
	var back Status
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &back); err != nil {
		t.Fatalf("decode written status: %v", err)
	}
	if back.Targets[0].Score != 88 || !back.Targets[0].Ready || back.KBVersion != "kb-v" {
		t.Errorf("written status = %+v, want score 88 ready kb-v", back)
	}
}

func TestWriteStatusRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	dyn := newDynFake(newCRObject(DefaultName))
	conflicts := 0
	dyn.PrependReactor("update", Plural, func(action k8stesting.Action) (bool, runtime.Object, error) {
		ua := action.(k8stesting.UpdateAction)
		if ua.GetSubresource() != "status" {
			return false, nil, nil
		}
		if conflicts == 0 {
			conflicts++
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: Group, Resource: Plural}, DefaultName, errors.New("rv mismatch"))
		}
		return false, nil, nil // fall through to the default reactor
	})
	if err := WriteStatus(ctx, dyn, DefaultName, Status{AgentVersion: "v0.2.0"}); err != nil {
		t.Fatalf("WriteStatus did not retry past one conflict: %v", err)
	}
	if conflicts != 1 {
		t.Errorf("conflict reactor fired %d times, want 1", conflicts)
	}
}

func TestWriteStatusMissingObject(t *testing.T) {
	dyn := newDynFake()
	if err := WriteStatus(context.Background(), dyn, DefaultName, Status{}); err == nil {
		t.Fatal("WriteStatus on absent object: want error, got nil")
	}
}
```

- [ ] **Step 2: Run tests, expect compile failure**

```bash
go test ./internal/crd/
```

Expected: `undefined: ReadSpec`, `undefined: EnsureObject`, `undefined: WriteStatus`.

- [ ] **Step 3: Implement the three functions in apply.go**

Append to `internal/crd/apply.go` (add imports `k8s.io/apimachinery/pkg/apis/meta/v1/unstructured`, `k8s.io/apimachinery/pkg/runtime`, `k8s.io/client-go/dynamic`):

```go
// ReadSpec returns the ClusterReadiness spec. found=false (with nil error)
// means the object does not exist; callers typically EnsureObject then.
func ReadSpec(ctx context.Context, dyn dynamic.Interface, name string) (Spec, bool, error) {
	obj, err := dyn.Resource(GVR()).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Spec{}, false, nil
	}
	if err != nil {
		return Spec{}, false, fmt.Errorf("get clusterreadiness %q: %w", name, err)
	}
	raw, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		return Spec{}, true, fmt.Errorf("read spec of clusterreadiness %q: %w", name, err)
	}
	if !found {
		return Spec{}, true, nil
	}
	var s Spec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &s); err != nil {
		return Spec{}, true, fmt.Errorf("decode spec of clusterreadiness %q: %w", name, err)
	}
	return s, true, nil
}

// EnsureObject creates the ClusterReadiness CR with an empty spec if absent.
// It never overwrites an existing object (user-set spec.targets survive).
func EnsureObject(ctx context.Context, dyn dynamic.Interface, name string) error {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": Group + "/" + Version,
		"kind":       Kind,
		"metadata":   map[string]interface{}{"name": name},
		"spec":       map[string]interface{}{},
	}}
	_, err := dyn.Resource(GVR()).Create(ctx, obj, metav1.CreateOptions{})
	if err == nil || apierrors.IsAlreadyExists(err) {
		return nil
	}
	return fmt.Errorf("create clusterreadiness %q: %w", name, err)
}

// WriteStatus replaces the status subresource, retrying on conflict with a
// fresh read each attempt.
func WriteStatus(ctx context.Context, dyn dynamic.Interface, name string, st Status) error {
	stMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&st)
	if err != nil {
		return fmt.Errorf("convert status: %w", err)
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj, gerr := dyn.Resource(GVR()).Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		obj.Object["status"] = stMap
		_, uerr := dyn.Resource(GVR()).UpdateStatus(ctx, obj, metav1.UpdateOptions{})
		return uerr
	})
	if err != nil {
		return fmt.Errorf("update clusterreadiness %q status: %w", name, err)
	}
	return nil
}
```

- [ ] **Step 4: Run tests, expect pass**

```bash
go test ./internal/crd/ && go vet ./internal/crd/
```

- [ ] **Step 5: Commit**

```bash
git add internal/crd/ && git commit -m "feat(crd): ReadSpec, EnsureObject, WriteStatus via dynamic client with conflict retry" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task A5: Snapshot push client — gzip, bearer, retries, latest-only buffer

**Files:**
- Create: `internal/agent/push.go`
- Test: `internal/agent/push_test.go`

- [ ] **Step 1: Write failing tests against httptest**

Create `internal/agent/push_test.go`:

```go
package agent

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// pushRecorder is an httptest handler that decodes gzipped push payloads and
// replies per script: one status code per request, last repeats.
type pushRecorder struct {
	mu       sync.Mutex
	statuses []int
	payloads []pushPayload
	headers  []http.Header
}

func (rec *pushRecorder) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.headers = append(rec.headers, r.Header.Clone())
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("bad gzip body: %v", err)
			} else {
				var pl pushPayload
				if err := json.NewDecoder(zr).Decode(&pl); err != nil {
					t.Errorf("bad payload JSON: %v", err)
				}
				rec.payloads = append(rec.payloads, pl)
			}
		}
		idx := len(rec.headers) - 1
		if idx >= len(rec.statuses) {
			idx = len(rec.statuses) - 1
		}
		code := rec.statuses[idx]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		switch code {
		case http.StatusAccepted:
			io.WriteString(w, `{"snapshotId": 123}`)
		case http.StatusOK:
			io.WriteString(w, `{"snapshotId": 122, "duplicate": true}`)
		default:
			io.WriteString(w, `{"error": "nope"}`)
		}
	}
}

func (rec *pushRecorder) requests() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.headers)
}

func testPayload(name string) pushPayload {
	return pushPayload{
		SchemaVersion: 1,
		ClusterName:   name,
		AgentVersion:  "v0.2.0",
		KBVersion:     "kb-v",
		Inventory:     json.RawMessage(`{"schemaVersion":1,"clusterId":"uid-123"}`),
	}
}

func newTestPusher(t *testing.T, rec *pushRecorder, statuses ...int) (*pusher, *[]time.Duration) {
	t.Helper()
	rec.statuses = statuses
	srv := httptest.NewServer(rec.handler(t))
	t.Cleanup(srv.Close)
	p := newPusher(srv.URL+"/", "sekret") // trailing slash must be trimmed
	var slept []time.Duration
	p.sleep = func(d time.Duration) { slept = append(slept, d) }
	return p, &slept
}

func TestFlushSendsGzipBearerJSON(t *testing.T) {
	rec := &pushRecorder{}
	p, _ := newTestPusher(t, rec, http.StatusAccepted)
	p.offer(testPayload("prod-eu-1"))
	if err := p.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if rec.requests() != 1 {
		t.Fatalf("requests = %d, want 1", rec.requests())
	}
	h := rec.headers[0]
	if got := h.Get("Authorization"); got != "Bearer sekret" {
		t.Errorf("Authorization = %q", got)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := h.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q", got)
	}
	pl := rec.payloads[0]
	if pl.SchemaVersion != 1 || pl.ClusterName != "prod-eu-1" || pl.KBVersion != "kb-v" {
		t.Errorf("payload = %+v", pl)
	}
	// Pending cleared on success: a second flush is a no-op.
	if err := p.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.requests() != 1 {
		t.Errorf("flush after success re-sent: %d requests", rec.requests())
	}
}

func TestFlushDuplicate200IsSuccess(t *testing.T) {
	rec := &pushRecorder{}
	p, _ := newTestPusher(t, rec, http.StatusOK)
	p.offer(testPayload("c"))
	if err := p.flush(context.Background()); err != nil {
		t.Fatalf("200 duplicate must be success, got %v", err)
	}
}

func TestFlushLatestOnlyBuffer(t *testing.T) {
	rec := &pushRecorder{}
	p, _ := newTestPusher(t, rec, http.StatusAccepted)
	p.offer(testPayload("older"))
	p.offer(testPayload("newer")) // replaces, never queues
	if err := p.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.requests() != 1 || rec.payloads[0].ClusterName != "newer" {
		t.Errorf("got %d requests, first cluster %q; want 1 request of newer", rec.requests(), rec.payloads[0].ClusterName)
	}
}

func TestFlushRetriesTransientWithBackoff(t *testing.T) {
	rec := &pushRecorder{}
	p, slept := newTestPusher(t, rec, http.StatusInternalServerError, http.StatusBadGateway, http.StatusAccepted)
	p.offer(testPayload("c"))
	if err := p.flush(context.Background()); err != nil {
		t.Fatalf("flush should succeed on third attempt: %v", err)
	}
	if rec.requests() != 3 {
		t.Errorf("requests = %d, want 3", rec.requests())
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; len(*slept) != 2 || (*slept)[0] != want[0] || (*slept)[1] != want[1] {
		t.Errorf("backoff sleeps = %v, want %v", *slept, want)
	}
}

func TestFlushExhaustsRetriesKeepsPending(t *testing.T) {
	rec := &pushRecorder{}
	p, slept := newTestPusher(t, rec, http.StatusInternalServerError)
	p.offer(testPayload("c"))
	err := p.flush(context.Background())
	if err == nil {
		t.Fatal("want error after exhausting retries")
	}
	if rec.requests() != 4 { // initial + 3 retries
		t.Errorf("requests = %d, want 4", rec.requests())
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; len(*slept) != 3 {
		t.Errorf("sleeps = %v, want %v", *slept, want)
	}
	// Payload kept buffered: next flush retries it.
	if err := p.flush(context.Background()); err == nil && rec.requests() == 4 {
		t.Error("pending was dropped after transient exhaustion")
	}
}

func TestFlush401DropsPayloadNoRetry(t *testing.T) {
	rec := &pushRecorder{}
	p, slept := newTestPusher(t, rec, http.StatusUnauthorized)
	p.offer(testPayload("c"))
	err := p.flush(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want 401 mention", err)
	}
	if rec.requests() != 1 || len(*slept) != 0 {
		t.Errorf("401 retried: %d requests, sleeps %v", rec.requests(), *slept)
	}
	if err := p.flush(context.Background()); err != nil || rec.requests() != 1 {
		t.Error("payload not dropped after 401")
	}
}

func TestFlush422DropsPayloadWithServerError(t *testing.T) {
	rec := &pushRecorder{}
	p, _ := newTestPusher(t, rec, http.StatusUnprocessableEntity)
	p.offer(testPayload("c"))
	err := p.flush(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want server error body surfaced", err)
	}
	if rec.requests() != 1 {
		t.Errorf("422 retried: %d requests", rec.requests())
	}
}

func TestBackoffCapped(t *testing.T) {
	if got := backoff(10); got != time.Minute {
		t.Errorf("backoff(10) = %v, want 1m cap", got)
	}
	if got := backoff(0); got != time.Second {
		t.Errorf("backoff(0) = %v, want 1s", got)
	}
}
```

- [ ] **Step 2: Run tests, expect compile failure**

```bash
go test ./internal/agent/
```

Expected: `undefined: pushPayload`, `undefined: newPusher`, `undefined: backoff`.

- [ ] **Step 3: Implement push.go**

Create `internal/agent/push.go`:

```go
package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// pushPayload is the snapshot envelope (plan: "Snapshot push protocol").
type pushPayload struct {
	SchemaVersion int             `json:"schemaVersion"`
	ClusterName   string          `json:"clusterName"`
	AgentVersion  string          `json:"agentVersion"`
	KBVersion     string          `json:"kbVersion"`
	Inventory     json.RawMessage `json:"inventory"`
}

const (
	pushRetries    = 3 // retries after the initial attempt
	maxPushBackoff = time.Minute
)

// backoff is the delay before retry n (0-based): 1s, 2s, 4s, ... capped at 1m.
func backoff(attempt int) time.Duration {
	d := time.Second << uint(min(attempt, 20))
	if d > maxPushBackoff {
		return maxPushBackoff
	}
	return d
}

// pusher delivers snapshots to the server. It buffers at most one payload:
// offering a newer snapshot replaces the pending one (latest-only — spec §9:
// the agent never queues history, the server reconstructs it from pushes).
type pusher struct {
	url   string // base server URL, trailing slash trimmed
	token string
	hc    *http.Client
	sleep func(time.Duration) // injectable for deterministic tests

	mu      sync.Mutex
	pending *pushPayload
}

func newPusher(serverURL, token string) *pusher {
	return &pusher{
		url:   strings.TrimRight(serverURL, "/"),
		token: token,
		hc:    &http.Client{Timeout: 30 * time.Second},
		sleep: time.Sleep,
	}
}

// offer replaces any pending snapshot with the newer one.
func (p *pusher) offer(pl pushPayload) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = &pl
}

// flush sends the pending snapshot, if any. Transient failures (network,
// 5xx) retry up to pushRetries times with exponential backoff; the payload
// stays buffered on exhaustion so the next tick retries. Permanent failures
// (401 bad token, 422 invalid body) drop the payload — resending identical
// bytes cannot succeed — and return the error for logging.
func (p *pusher) flush(ctx context.Context) error {
	p.mu.Lock()
	pl := p.pending
	p.mu.Unlock()
	if pl == nil {
		return nil
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		permanent, err := p.send(ctx, *pl)
		if err == nil {
			p.clear(pl)
			return nil
		}
		if permanent {
			p.clear(pl)
			return err
		}
		lastErr = err
		if attempt == pushRetries {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.sleep(backoff(attempt))
	}
	return fmt.Errorf("push snapshot after %d attempts (kept buffered): %w", pushRetries+1, lastErr)
}

// clear drops pl iff it is still the pending payload — a newer offer made
// during a slow send must survive.
func (p *pusher) clear(pl *pushPayload) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == pl {
		p.pending = nil
	}
}

func (p *pusher) send(ctx context.Context, pl pushPayload) (permanent bool, err error) {
	body, err := json.Marshal(pl)
	if err != nil {
		return true, fmt.Errorf("marshal snapshot: %w", err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		return true, fmt.Errorf("gzip snapshot: %w", err)
	}
	if err := zw.Close(); err != nil {
		return true, fmt.Errorf("gzip snapshot: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+"/api/v1/snapshots", &buf)
	if err != nil {
		return true, fmt.Errorf("build push request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := p.hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("push snapshot: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK: // 202 accepted, 200 duplicate
		return false, nil
	case http.StatusUnauthorized:
		return true, fmt.Errorf("server rejected push (401): check --server-token")
	case http.StatusUnprocessableEntity:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return true, fmt.Errorf("server rejected snapshot (422): %s", strings.TrimSpace(string(msg)))
	default:
		return false, fmt.Errorf("server returned %s", resp.Status)
	}
}
```

- [ ] **Step 4: Run tests, expect pass**

```bash
go test ./internal/agent/
```

- [ ] **Step 5: Commit**

```bash
git add internal/agent/ && git commit -m "feat(agent): snapshot push client with gzip, bearer, backoff, latest-only buffer" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task A6: agent Config defaults/validation + target resolution

**Files:**
- Create: `internal/agent/agent.go`
- Test: `internal/agent/agent_test.go`

- [ ] **Step 1: Write failing tests for applyDefaults and resolveTargets**

Create `internal/agent/agent_test.go`:

```go
package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

func TestConfigApplyDefaults(t *testing.T) {
	cfg := Config{}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults on zero config: %v", err)
	}
	if cfg.Interval != 10*time.Minute {
		t.Errorf("Interval = %v, want 10m", cfg.Interval)
	}
	if cfg.CRName != crd.DefaultName {
		t.Errorf("CRName = %q, want %q", cfg.CRName, crd.DefaultName)
	}
	if cfg.TeamLabel != "team" {
		t.Errorf("TeamLabel = %q, want team", cfg.TeamLabel)
	}
	if cfg.ForceSyncEvery != time.Hour {
		t.Errorf("ForceSyncEvery = %v, want 1h", cfg.ForceSyncEvery)
	}
}

func TestConfigIntervalMinimum(t *testing.T) {
	cfg := Config{Interval: 30 * time.Second}
	err := cfg.applyDefaults()
	if err == nil || !strings.Contains(err.Error(), "1m") {
		t.Fatalf("err = %v, want minimum-interval error", err)
	}
}

func TestConfigServerURLRequiresToken(t *testing.T) {
	cfg := Config{ServerURL: "http://server:8080"}
	if err := cfg.applyDefaults(); err == nil {
		t.Fatal("server-url without server-token must error")
	}
	cfg = Config{ServerURL: "http://server:8080", ServerToken: "t"}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatalf("valid server config rejected: %v", err)
	}
}

func TestResolveTargetsFromSpec(t *testing.T) {
	targets, notes, err := resolveTargets(
		crd.Spec{Targets: []string{"1.36", "1.37"}},
		inventory.Inventory{ServerVersion: "v1.35.2"},
	)
	if err != nil || len(notes) != 0 {
		t.Fatalf("err=%v notes=%v", err, notes)
	}
	want := []inventory.Version{{Major: 1, Minor: 36}, {Major: 1, Minor: 37}}
	if len(targets) != 2 || targets[0] != want[0] || targets[1] != want[1] {
		t.Errorf("targets = %v, want %v", targets, want)
	}
}

func TestResolveTargetsSkipsInvalidWithNote(t *testing.T) {
	targets, notes, err := resolveTargets(
		crd.Spec{Targets: []string{"latest", "1.37"}},
		inventory.Inventory{ServerVersion: "v1.35.2"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != (inventory.Version{Major: 1, Minor: 37}) {
		t.Errorf("targets = %v, want [1.37]", targets)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "latest") {
		t.Errorf("notes = %v, want one mentioning %q", notes, "latest")
	}
}

func TestResolveTargetsDefaultNextMinor(t *testing.T) {
	targets, _, err := resolveTargets(crd.Spec{}, inventory.Inventory{ServerVersion: "v1.35.2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != (inventory.Version{Major: 1, Minor: 36}) {
		t.Errorf("targets = %v, want [1.36] (next minor above observed)", targets)
	}
}

func TestResolveTargetsNoServerVersion(t *testing.T) {
	if _, _, err := resolveTargets(crd.Spec{}, inventory.Inventory{}); err == nil {
		t.Fatal("no targets and no server version: want error")
	}
}

func TestResolveTargetsUnparseableServerVersion(t *testing.T) {
	_, _, err := resolveTargets(crd.Spec{}, inventory.Inventory{ServerVersion: "v1.34.2-gke.100"})
	if err == nil || !strings.Contains(err.Error(), "gke") {
		t.Fatalf("err = %v, want unparseable-version error naming the version", err)
	}
}
```

- [ ] **Step 2: Run tests, expect compile failure**

```bash
go test ./internal/agent/
```

Expected: `undefined: Config`, `undefined: resolveTargets`.

- [ ] **Step 3: Implement Config + resolveTargets in agent.go**

Create `internal/agent/agent.go`:

```go
// Package agent runs the in-cluster continuous loop: collect → evaluate per
// target → ClusterReadiness CRD status (always) → push snapshot to the server
// on content change. The agent's local value never depends on server
// availability (spec §3).
package agent

import (
	"fmt"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
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
```

- [ ] **Step 4: Run tests, expect pass**

```bash
go test ./internal/agent/
```

- [ ] **Step 5: Commit**

```bash
git add internal/agent/ && git commit -m "feat(agent): config defaults/validation and per-tick target resolution" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task A7: tick pipeline — collect, evaluate, CRD status, dedup push

**Files:**
- Modify: `internal/agent/agent.go`
- Test: `internal/agent/tick_test.go`

- [ ] **Step 1: Write failing tests for tick with fake clientset + dynamic fake + httptest**

Create `internal/agent/tick_test.go`:

```go
package agent

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// fakeClients builds collect.Clients over a fake clientset reporting the given
// server version. Metadata/RESTClient stay nil — those capabilities degrade,
// which is exactly the "not assessed" path we want exercised.
func fakeClients(t *testing.T, serverVersion string) collect.Clients {
	t.Helper()
	cs := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-123")}},
	)
	disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
	disc.FakedServerVersion = &version.Info{GitVersion: serverVersion}
	return collect.Clients{Kube: cs, Discovery: disc}
}

func fakeDyn(objects ...runtime.Object) dynamic.Interface {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{crd.GVR(): crd.Kind + "List"},
		objects...,
	)
}

func mustKB(t *testing.T) kb.KB {
	t.Helper()
	k, err := kb.Load()
	if err != nil {
		t.Fatalf("kb.Load: %v", err)
	}
	return k
}

func readCRStatus(t *testing.T, dyn dynamic.Interface, name string) crd.Status {
	t.Helper()
	obj, err := dyn.Resource(crd.GVR()).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get CR %q: %v", name, err)
	}
	raw, found, err := unstructured.NestedMap(obj.Object, "status")
	if err != nil || !found {
		t.Fatalf("status not written: found=%v err=%v", found, err)
	}
	var st crd.Status
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return st
}

// snapServer records decoded push payloads and always answers 202.
type snapServer struct {
	mu       sync.Mutex
	payloads []pushPayload
	srv      *httptest.Server
}

func newSnapServer(t *testing.T) *snapServer {
	s := &snapServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("push body not gzipped: %v", err)
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		var pl pushPayload
		if err := json.NewDecoder(zr).Decode(&pl); err != nil {
			t.Errorf("push body not JSON: %v", err)
		}
		s.mu.Lock()
		s.payloads = append(s.payloads, pl)
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"snapshotId": 1}`)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *snapServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.payloads)
}

func testRunner(t *testing.T, dyn dynamic.Interface, serverURL string) *runner {
	t.Helper()
	cfg := Config{ServerURL: serverURL, ServerToken: "tok"}
	if serverURL == "" {
		cfg.ServerToken = ""
	}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	r := newRunner(fakeClients(t, "v1.35.2"), dyn, mustKB(t), cfg)
	if r.pusher != nil {
		r.pusher.sleep = func(time.Duration) {} // never really sleep in tests
	}
	return r
}

func TestTickWritesStatusWithDefaultTarget(t *testing.T) {
	ctx := context.Background()
	dyn := fakeDyn()
	srv := newSnapServer(t)
	r := testRunner(t, dyn, srv.srv.URL)

	if err := r.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	st := readCRStatus(t, dyn, crd.DefaultName)
	if st.ObservedServerVersion != "v1.35.2" {
		t.Errorf("ObservedServerVersion = %q", st.ObservedServerVersion)
	}
	if len(st.Targets) != 1 || st.Targets[0].Target != "1.36" {
		t.Fatalf("Targets = %+v, want default next minor 1.36", st.Targets)
	}
	if st.Targets[0].Score < 0 || st.Targets[0].Score > 100 {
		t.Errorf("Score = %d, want 0..100", st.Targets[0].Score)
	}
	if st.KBVersion == "" || st.AgentVersion == "" {
		t.Errorf("KBVersion/AgentVersion empty: %+v", st)
	}
	if len(st.NotAssessed) == 0 {
		t.Error("nil Metadata client should surface notAssessed entries")
	}
}

func TestTickPushesWithClusterIDWhenNameUnset(t *testing.T) {
	ctx := context.Background()
	srv := newSnapServer(t)
	r := testRunner(t, fakeDyn(), srv.srv.URL)
	if err := r.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if srv.count() != 1 {
		t.Fatalf("pushes = %d, want 1", srv.count())
	}
	pl := srv.payloads[0]
	if pl.ClusterName != "uid-123" {
		t.Errorf("ClusterName = %q, want cluster UID fallback uid-123", pl.ClusterName)
	}
	if pl.SchemaVersion != 1 || pl.KBVersion == "" || len(pl.Inventory) == 0 {
		t.Errorf("payload = %+v", pl)
	}
}

func TestTickDedupsUnchangedInventory(t *testing.T) {
	ctx := context.Background()
	srv := newSnapServer(t)
	r := testRunner(t, fakeDyn(), srv.srv.URL)
	base := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	cur := base
	r.now = func() time.Time { return cur }

	if err := r.tick(ctx); err != nil {
		t.Fatal(err)
	}
	cur = cur.Add(10 * time.Minute)
	if err := r.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if srv.count() != 1 {
		t.Fatalf("pushes = %d, want 1 (unchanged inventory deduped)", srv.count())
	}

	// ForceSyncEvery (1h default) elapsed → push despite unchanged hash.
	cur = cur.Add(2 * time.Hour)
	if err := r.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if srv.count() != 2 {
		t.Fatalf("pushes = %d, want 2 (force sync elapsed)", srv.count())
	}
}

func TestTickHonorsSpecTargets(t *testing.T) {
	ctx := context.Background()
	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": crd.Group + "/" + crd.Version,
		"kind":       crd.Kind,
		"metadata":   map[string]interface{}{"name": crd.DefaultName},
		"spec":       map[string]interface{}{"targets": []interface{}{"1.36", "1.37"}},
	}}
	dyn := fakeDyn(cr)
	r := testRunner(t, dyn, "") // CRD-only mode
	if err := r.tick(ctx); err != nil {
		t.Fatal(err)
	}
	st := readCRStatus(t, dyn, crd.DefaultName)
	if len(st.Targets) != 2 || st.Targets[0].Target != "1.36" || st.Targets[1].Target != "1.37" {
		t.Fatalf("Targets = %+v, want spec targets [1.36 1.37]", st.Targets)
	}
}

func TestTickCRDOnlyModeNoPusher(t *testing.T) {
	r := testRunner(t, fakeDyn(), "")
	if r.pusher != nil {
		t.Fatal("pusher built without ServerURL")
	}
	if err := r.tick(context.Background()); err != nil {
		t.Fatalf("CRD-only tick: %v", err)
	}
}

// fakeClientsInventory collects a realistic inventory from the fakes — the
// hash test must exercise the real wire shape, not a hand-built struct.
func fakeClientsInventory(t *testing.T) inventory.Inventory {
	t.Helper()
	return collect.Collect(context.Background(), fakeClients(t, "v1.35.2"), mustKB(t), collect.Options{TeamLabel: "team"})
}

func TestSnapshotHashIgnoresCollectedAt(t *testing.T) {
	inv := fakeClientsInventory(t)
	h1, _, err := snapshotHash(inv)
	if err != nil {
		t.Fatal(err)
	}
	inv.CollectedAt = inv.CollectedAt.Add(time.Hour)
	h2, _, err := snapshotHash(inv)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Error("hash changed when only CollectedAt changed — dedup would never fire")
	}
	inv.ServerVersion = "v1.99.0"
	h3, _, _ := snapshotHash(inv)
	if h3 == h1 {
		t.Error("hash did not change when content changed")
	}
}
```

- [ ] **Step 2: Run tests, expect compile failure**

```bash
go test ./internal/agent/
```

Expected: `undefined: newRunner`, `undefined: snapshotHash`, and `r.tick` undefined.

- [ ] **Step 3: Implement runner, tick, push decision, snapshotHash**

Append to `internal/agent/agent.go` (imports grow to include `context`, `crypto/sha256`, `encoding/hex`, `encoding/json`, `errors`, `log/slog`, `k8s.io/client-go/dynamic`, `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"`, and the `collect`, `engine`, `kb` packages):

```go
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
```

- [ ] **Step 4: Run tests, expect pass**

```bash
go test ./internal/agent/ ./internal/crd/
```

If `TestTickWritesStatusWithDefaultTarget` fails on `NotAssessed` being empty, check what `collect.Collect` reports with nil Metadata/RESTClient (read `internal/collect/collector.go` capability handling) and adjust the assertion to the actual degraded capabilities — the invariant under test is "degraded capabilities surface in CRD status", not a specific count.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/ && git commit -m "feat(agent): tick pipeline — collect, evaluate per target, CRD status, dedup push" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task A8: Run loop — startup, jittered interval, graceful stop

**Files:**
- Modify: `internal/agent/agent.go`
- Test: `internal/agent/run_test.go`

- [ ] **Step 1: Write failing tests for jitter bounds and Run lifecycle**

Create `internal/agent/run_test.go`:

```go
package agent

import (
	"context"
	"testing"
	"time"

	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
)

func TestJitterBounds(t *testing.T) {
	d := 10 * time.Minute
	lo, hi := 9*time.Minute, 11*time.Minute
	for i := 0; i < 200; i++ {
		got := jitter(d)
		if got < lo || got > hi {
			t.Fatalf("jitter(%v) = %v, outside [%v, %v]", d, got, lo, hi)
		}
	}
}

func TestRunInvalidConfig(t *testing.T) {
	err := Run(context.Background(), fakeClients(t, "v1.35.2"), fakeDyn(),
		apiextfake.NewSimpleClientset(), mustKB(t), Config{Interval: time.Second})
	if err == nil {
		t.Fatal("Run with sub-minimum interval: want error")
	}
}

// TestRunFirstTickThenGracefulStop: Run ensures the CRD, ticks once
// synchronously before the first wait, then blocks on the (≥1m, jittered)
// timer. We poll the fake for the first tick's status write, cancel, and
// require a nil return. No timing dependence: the first tick happens before
// any timer, and 1m never elapses inside the test.
func TestRunFirstTickThenGracefulStop(t *testing.T) {
	dyn := fakeDyn()
	apiext := apiextfake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, fakeClients(t, "v1.35.2"), dyn, apiext, mustKB(t), Config{})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		obj, err := dyn.Resource(crd.GVR()).Get(context.Background(), crd.DefaultName, metav1.GetOptions{})
		if err == nil {
			if _, found, _ := unstructured.NestedMap(obj.Object, "status"); found {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("first tick never wrote CRD status")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// EnsureCRD ran at startup against the apiextensions fake.
	if _, err := apiext.ApiextensionsV1().CustomResourceDefinitions().Get(
		context.Background(), "clusterreadinesses.upgradescope.dev", metav1.GetOptions{}); err != nil {
		t.Errorf("EnsureCRD did not install the CRD: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v on cancel, want nil (graceful stop)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
```

- [ ] **Step 2: Run tests, expect compile failure**

```bash
go test ./internal/agent/
```

Expected: `undefined: Run`, `undefined: jitter`.

- [ ] **Step 3: Implement Run + jitter**

Append to `internal/agent/agent.go` (add imports `math/rand/v2`, `apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"`):

```go
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
```

- [ ] **Step 4: Run tests, expect pass**

```bash
go test ./internal/agent/ -count=1 && go vet ./internal/agent/
```

- [ ] **Step 5: Commit**

```bash
git add internal/agent/ && git commit -m "feat(agent): Run loop with jittered interval, startup EnsureCRD, graceful stop" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task A9: `upgradescope agent` CLI command + root wiring

**Files:**
- Create: `internal/cli/agent.go`
- Modify: `internal/cli/root.go`
- Test: `internal/cli/agent_test.go`

- [ ] **Step 1: Write failing tests for flags, in-cluster fallback, root wiring**

Create `internal/cli/agent_test.go`:

```go
package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: abc
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAgentCmdFlagDefaults(t *testing.T) {
	orig := runAgent
	defer func() { runAgent = orig }()
	var got agentOptions
	runAgent = func(_ context.Context, opts agentOptions) error {
		got = opts
		return nil
	}

	root := Root()
	root.SetArgs([]string{"agent"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got.interval != 10*time.Minute {
		t.Errorf("interval = %v, want 10m", got.interval)
	}
	if got.crName != "cluster" || got.teamLabel != "team" {
		t.Errorf("crName/teamLabel = %q/%q, want cluster/team", got.crName, got.teamLabel)
	}
	if got.forceSyncEvery != time.Hour {
		t.Errorf("forceSyncEvery = %v, want 1h", got.forceSyncEvery)
	}
	if got.serverURL != "" || got.serverToken != "" {
		t.Errorf("server flags should default empty: %+v", got)
	}
}

func TestAgentCmdFlagsParsed(t *testing.T) {
	orig := runAgent
	defer func() { runAgent = orig }()
	var got agentOptions
	runAgent = func(_ context.Context, opts agentOptions) error {
		got = opts
		return nil
	}

	root := Root()
	root.SetArgs([]string{"agent",
		"--interval", "5m",
		"--server-url", "http://scope:8080",
		"--server-token", "tok",
		"--cluster-name", "prod-eu-1",
		"--cr-name", "main",
		"--team-label", "squad",
		"--force-sync-every", "30m",
		"--kubeconfig", "/tmp/kc",
		"--context", "ctx1",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := agentOptions{
		interval: 5 * time.Minute, serverURL: "http://scope:8080", serverToken: "tok",
		clusterName: "prod-eu-1", crName: "main", teamLabel: "squad",
		forceSyncEvery: 30 * time.Minute, kubeconfig: "/tmp/kc", kubecontext: "ctx1",
	}
	if got != want {
		t.Errorf("opts = %+v, want %+v", got, want)
	}
}

func TestRootHasAgentSubcommand(t *testing.T) {
	for _, c := range Root().Commands() {
		if c.Name() == "agent" {
			return
		}
	}
	t.Fatal("root command has no agent subcommand")
}

func TestBuildAgentRESTConfigExplicitKubeconfig(t *testing.T) {
	cfg, err := buildAgentRESTConfig(writeKubeconfig(t), "")
	if err != nil {
		t.Fatalf("buildAgentRESTConfig: %v", err)
	}
	if cfg.Host != "https://127.0.0.1:6443" {
		t.Errorf("Host = %q, want https://127.0.0.1:6443", cfg.Host)
	}
}

// Outside a pod, rest.InClusterConfig fails (even with the env vars set there
// is no service-account token file) and the loader must fall back to
// kubeconfig loading rules ($KUBECONFIG here).
func TestBuildAgentRESTConfigFallsBackFromInCluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	t.Setenv("KUBECONFIG", writeKubeconfig(t))
	cfg, err := buildAgentRESTConfig("", "")
	if err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if cfg.Host != "https://127.0.0.1:6443" {
		t.Errorf("Host = %q, want kubeconfig host (fallback path)", cfg.Host)
	}
}
```

- [ ] **Step 2: Run tests, expect compile failure**

```bash
go test ./internal/cli/
```

Expected: `undefined: runAgent`, `undefined: agentOptions`, `undefined: buildAgentRESTConfig`.

- [ ] **Step 3: Implement internal/cli/agent.go**

Create `internal/cli/agent.go`:

```go
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abd-ulbasit/upgradescope/internal/agent"
	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

type agentOptions struct {
	interval       time.Duration
	serverURL      string
	serverToken    string
	clusterName    string
	crName         string
	teamLabel      string
	forceSyncEvery time.Duration
	kubeconfig     string
	kubecontext    string
}

// runAgent is the real I/O pipeline behind `upgradescope agent`. A package
// var so command tests can stub it (same pattern as runScan).
var runAgent = func(ctx context.Context, opts agentOptions) error {
	kbData, err := kb.Load()
	if err != nil {
		return fmt.Errorf("load knowledge base: %w", err)
	}
	cfg, err := buildAgentRESTConfig(opts.kubeconfig, opts.kubecontext)
	if err != nil {
		return err
	}
	clients, err := collect.NewClients(cfg)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build dynamic client: %w", err)
	}
	apiext, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build apiextensions client: %w", err)
	}
	agent.AgentVersion = version
	return agent.Run(ctx, clients, dyn, apiext, kbData, agent.Config{
		Interval:       opts.interval,
		ServerURL:      opts.serverURL,
		ServerToken:    opts.serverToken,
		ClusterName:    opts.clusterName,
		CRName:         opts.crName,
		TeamLabel:      opts.teamLabel,
		ForceSyncEvery: opts.forceSyncEvery,
	})
}

// buildAgentRESTConfig prefers in-cluster config (the agent's normal home)
// and falls back to kubeconfig loading rules — the same rules as scan. An
// explicit --kubeconfig or --context skips the in-cluster attempt entirely.
var buildAgentRESTConfig = func(kubeconfig, kubecontext string) (*rest.Config, error) {
	if kubeconfig == "" && kubecontext == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubecontext}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig (not in-cluster, no kubeconfig found): %w", err)
	}
	return cfg, nil
}

func newAgentCmd() *cobra.Command {
	var opts agentOptions
	cmd := &cobra.Command{
		Use:           "agent",
		Short:         "Run the in-cluster continuous upgrade-readiness agent",
		Long:          "Continuously collects cluster inventory, evaluates upgrade readiness, writes the ClusterReadiness CRD status, and (optionally) pushes snapshots to an upgradescope server.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runAgent(ctx, opts)
		},
	}
	cmd.Flags().DurationVar(&opts.interval, "interval", 10*time.Minute, "evaluation interval (minimum 1m)")
	cmd.Flags().StringVar(&opts.serverURL, "server-url", "", "upgradescope server base URL (empty = CRD-only mode)")
	cmd.Flags().StringVar(&opts.serverToken, "server-token", "", "bearer token for snapshot pushes (required with --server-url)")
	cmd.Flags().StringVar(&opts.clusterName, "cluster-name", "", "cluster label sent to the server (default: cluster UID)")
	cmd.Flags().StringVar(&opts.crName, "cr-name", "cluster", "ClusterReadiness object name")
	cmd.Flags().StringVar(&opts.teamLabel, "team-label", "team", "namespace label used for team attribution")
	cmd.Flags().DurationVar(&opts.forceSyncEvery, "force-sync-every", time.Hour, "push a snapshot even if unchanged after this long")
	cmd.Flags().StringVar(&opts.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: in-cluster config, then standard loading rules)")
	cmd.Flags().StringVar(&opts.kubecontext, "context", "", "kubeconfig context to use")
	return cmd
}
```

- [ ] **Step 4: Wire into root.go**

In `internal/cli/root.go`, change:

```go
	root.AddCommand(newScanCmd())
	return root
```

to:

```go
	root.AddCommand(newScanCmd())
	root.AddCommand(newAgentCmd())
	return root
```

- [ ] **Step 5: Run tests, expect pass; build the binary**

```bash
go test ./internal/cli/ ./internal/agent/ ./internal/crd/ && go build ./... && go run ./cmd/upgradescope agent --help
```

Expected: tests pass; help text lists all nine flags.

- [ ] **Step 6: Full-suite check and commit**

```bash
go test ./... && go vet ./...
```

```bash
git add internal/cli/ && git commit -m "feat(cli): upgradescope agent command with in-cluster config fallback" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

## Section: internal/server/store

**Files:** `internal/server/store/store.go`, `internal/server/store/migrate.go`, `internal/server/store/sqlite.go`, `internal/server/store/migrations/0001_init.sql`, `internal/server/store/storetest/storetest.go` (+ tests).

**Dependency order:** S1 → S2 → S3 → S4, strictly sequential. New module dependency: `modernc.org/sqlite` (CGO-free SQLite), driver name `"sqlite"`.

### Section invariants (pinned by tests below — every other section codes against these)

- **Time storage:** TEXT columns, always UTC, RFC 3339 with **fixed nine-digit fractional seconds** — Go layout `2006-01-02T15:04:05.000000000Z07:00`. Rationale: `time.RFC3339Nano` trims trailing zeros, so `"…05Z"` sorts lexicographically *after* `"…05.5Z"` even though the instant is earlier; the fixed-width form makes string order equal instant order, which the SQL `ORDER BY created_at` clauses rely on. Reads are parsed with `time.RFC3339Nano` (it accepts the fixed-width form) and returned in `time.UTC`.
- **Zero times default:** a zero `FirstSeen`/`LastSeen`/`ReceivedAt`/`CreatedAt` is filled with `time.Now().UTC()` by the store. Tests always pass explicit times.
- **"Latest" snapshot** = highest `id` for the cluster (AUTOINCREMENT ⇒ insertion order). **"Latest" evaluation** = `ORDER BY created_at DESC, id DESC LIMIT 1`.
- **Snapshot dedup:** `InsertSnapshot` is a duplicate **iff** the hash equals the hash of the cluster's *latest* snapshot — returns `(latestID, true, nil)` with no insert. An older same-hash snapshot that has since been superseded by a different hash is **not** a duplicate (a re-push of hash A after B means the cluster changed back; that is a new row).
- **ScoreHistory:** returned **oldest-first, ascending by `created_at`**. `limit > 0` selects the **most recent N** rows (still returned oldest-first); `limit <= 0` means no limit. Unknown cluster/target → empty slice, `nil` error (not `ErrNotFound`).
- **ListClusters:** ascending by name.
- **`ErrNotFound`:** exported sentinel, returned (possibly wrapped) by `GetCluster`, `LatestSnapshot`, `LatestEvaluation` when no row matches. Callers use `errors.Is`.

---

### Task S1: Store interface, record types, ErrNotFound, time codec

**Files:**
- Create: `internal/server/store/store.go`
- Test: `internal/server/store/store_test.go`

- [ ] **Step 1: Add the modernc.org/sqlite dependency**

```bash
go get modernc.org/sqlite@latest
```

Expected: go.mod gains `require modernc.org/sqlite vX.Y.Z` (plus indirect modernc.org deps in go.sum). Nothing imports it until S2 — do **not** run `go mod tidy` before S2 lands or it will be dropped again.

- [ ] **Step 2: Write failing test for the sentinel, JSON tags and time codec**

Create `internal/server/store/store_test.go`:

```go
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestErrNotFoundIsSentinel(t *testing.T) {
	if ErrNotFound == nil {
		t.Fatal("ErrNotFound is nil")
	}
	wrapped := fmt.Errorf("get cluster 42: %w", ErrNotFound)
	if !errors.Is(wrapped, ErrNotFound) {
		t.Fatal("wrapped ErrNotFound is not matched by errors.Is")
	}
}

func TestBlobFieldsExcludedFromJSON(t *testing.T) {
	snap, err := json.Marshal(Snapshot{ID: 1, ClusterID: 2, Hash: "abc", Inventory: []byte(`{"secret":1}`)})
	if err != nil {
		t.Fatalf("marshal Snapshot: %v", err)
	}
	if strings.Contains(string(snap), "inventory") || strings.Contains(string(snap), "secret") {
		t.Errorf("Snapshot JSON leaks inventory blob: %s", snap)
	}
	if !strings.Contains(string(snap), `"hash":"abc"`) {
		t.Errorf("Snapshot JSON missing hash: %s", snap)
	}

	eval, err := json.Marshal(Evaluation{ID: 1, Target: "1.36", Report: []byte(`{"big":2}`)})
	if err != nil {
		t.Fatalf("marshal Evaluation: %v", err)
	}
	if strings.Contains(string(eval), "report") || strings.Contains(string(eval), "big") {
		t.Errorf("Evaluation JSON leaks report blob: %s", eval)
	}
	if !strings.Contains(string(eval), `"target":"1.36"`) {
		t.Errorf("Evaluation JSON missing target: %s", eval)
	}

	cl, err := json.Marshal(Cluster{ClusterUID: "u-1"})
	if err != nil {
		t.Fatalf("marshal Cluster: %v", err)
	}
	if !strings.Contains(string(cl), `"clusterUid":"u-1"`) {
		t.Errorf("Cluster JSON tag wrong: %s", cl)
	}
}

func TestTimeFormatFixedWidthUTC(t *testing.T) {
	pkt := time.FixedZone("PKT", 5*3600)
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{"whole second", time.Date(2026, 6, 10, 12, 0, 5, 0, time.UTC), "2026-06-10T12:00:05.000000000Z"},
		{"sub-second", time.Date(2026, 6, 10, 12, 0, 5, 500000000, time.UTC), "2026-06-10T12:00:05.500000000Z"},
		{"non-UTC normalized to Z", time.Date(2026, 6, 10, 17, 0, 5, 0, pkt), "2026-06-10T12:00:05.000000000Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatTime(tt.in); got != tt.want {
				t.Errorf("formatTime(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	// The reason for fixed width: with RFC3339Nano, "…05Z" sorts AFTER
	// "…05.5Z" as a string although it is the earlier instant.
	whole := formatTime(tests[0].in) // 12:00:05.000000000Z
	half := formatTime(tests[1].in)  // 12:00:05.500000000Z
	if !(whole < half) {
		t.Errorf("string order broken: %q must sort before %q", whole, half)
	}
}

func TestParseStoredTimeRoundTrip(t *testing.T) {
	in := time.Date(2026, 6, 10, 12, 0, 5, 123456789, time.FixedZone("X", -7*3600))
	got, err := parseStoredTime(formatTime(in))
	if err != nil {
		t.Fatalf("parseStoredTime: %v", err)
	}
	if !got.Equal(in) {
		t.Errorf("round trip lost the instant: got %v, want %v", got, in)
	}
	if got.Location() != time.UTC {
		t.Errorf("round trip returned location %v, want UTC", got.Location())
	}
	if _, err := parseStoredTime("not-a-time"); err == nil {
		t.Error("parseStoredTime accepted garbage")
	}
}
```

- [ ] **Step 3: Run test, expect compile failure**

Run: `go test ./internal/server/store/`

Expected output (test-only package builds, symbols undefined):
```
# github.com/abd-ulbasit/upgradescope/internal/server/store [github.com/abd-ulbasit/upgradescope/internal/server/store.test]
internal/server/store/store_test.go:13:5: undefined: ErrNotFound
internal/server/store/store_test.go:23:32: undefined: Snapshot
...
FAIL	github.com/abd-ulbasit/upgradescope/internal/server/store [build failed]
```

- [ ] **Step 4: Write store.go — interface and types verbatim from the contract**

Create `internal/server/store/store.go`:

```go
// Package store persists clusters, snapshots and evaluations for the
// upgradescope server. Store is the seam P3's Postgres implementation
// fills in; P2 ships the SQLite implementation in this package.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned (possibly wrapped) by lookups that match no row.
// Test with errors.Is.
var ErrNotFound = errors.New("store: not found")

// Store is the persistence contract. SQLite implements it in P2; P3 adds
// Postgres. Behavioral semantics are pinned by storetest.RunStoreConformance.
type Store interface {
	UpsertCluster(ctx context.Context, c Cluster) (int64, error)        // by name; returns id
	InsertSnapshot(ctx context.Context, s Snapshot) (int64, bool, error) // (id, duplicate, err) — duplicate iff same cluster+hash as latest
	LatestSnapshot(ctx context.Context, clusterID int64) (Snapshot, error)
	ListClusters(ctx context.Context) ([]Cluster, error)
	GetCluster(ctx context.Context, id int64) (Cluster, error)
	InsertEvaluation(ctx context.Context, e Evaluation) (int64, error)
	LatestEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error)
	ScoreHistory(ctx context.Context, clusterID int64, target string, limit int) ([]ScorePoint, error)
	Close() error
}

type Cluster struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`       // unique
	ClusterUID string    `json:"clusterUid"` // inventory.ClusterID
	FirstSeen  time.Time `json:"firstSeen"`
	LastSeen   time.Time `json:"lastSeen"`
}

type Snapshot struct {
	ID           int64     `json:"id"`
	ClusterID    int64     `json:"clusterId"`
	Hash         string    `json:"hash"` // sha256 of canonical inventory JSON
	KBVersion    string    `json:"kbVersion"`
	AgentVersion string    `json:"agentVersion"`
	ReceivedAt   time.Time `json:"receivedAt"`
	Inventory    []byte    `json:"-"` // raw canonical JSON
}

type Evaluation struct {
	ID         int64     `json:"id"`
	ClusterID  int64     `json:"clusterId"`
	SnapshotID int64     `json:"snapshotId"`
	Target     string    `json:"target"`
	KBVersion  string    `json:"kbVersion"`
	Score      int       `json:"score"`
	Ready      bool      `json:"ready"`
	Blockers   int       `json:"blockers"`
	Warnings   int       `json:"warnings"`
	Report     []byte    `json:"-"` // full engine.Report JSON
	CreatedAt  time.Time `json:"createdAt"`
}

type ScorePoint struct {
	At    time.Time `json:"at"`
	Score int       `json:"score"`
	Ready bool      `json:"ready"`
}

// timeFormat is RFC 3339 with a fixed nine-digit fractional second so that
// stored UTC strings sort lexicographically in instant order.
// time.RFC3339Nano trims trailing zeros, which would make "…05Z" sort after
// "…05.5Z"; the SQL ORDER BY clauses depend on string order being correct.
const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// formatTime renders t for storage: UTC, RFC 3339, fixed width.
func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }

// parseStoredTime parses a stored timestamp back to a UTC time.Time.
func parseStoredTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored time %q: %w", s, err)
	}
	return t.UTC(), nil
}
```

- [ ] **Step 5: Run test, expect pass**

Run: `go test ./internal/server/store/ -v`

Expected: `TestErrNotFoundIsSentinel`, `TestBlobFieldsExcludedFromJSON`, `TestTimeFormatFixedWidthUTC` (3 subtests), `TestParseStoredTimeRoundTrip` all PASS; `ok github.com/abd-ulbasit/upgradescope/internal/server/store`.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/server/store/ && git commit -m "feat: add server store contract — Store interface, records, ErrNotFound" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task S2: Embedded migrations runner

**Files:**
- Create: `internal/server/store/migrate.go`
- Test: `internal/server/store/migrate_test.go`

The runner is deliberately driver-agnostic (`*sql.DB` + `fs.FS`): P3's Postgres store reuses it with its own migration directory. modernc.org/sqlite executes multi-statement SQL in a single `Exec`, and SQLite supports transactional DDL — each migration file is fully applied or fully rolled back.

- [ ] **Step 1: Write failing test — order, idempotence, new-file pickup, atomic rollback**

Create `internal/server/store/migrate_test.go`:

```go
package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"
)

// openRawDB opens a plain (un-migrated, no pragmas) SQLite database in a
// temp dir. A file database is used deliberately: an in-memory DSN gives
// every pooled connection its own empty database.
func openRawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migrate-test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// migFS: 0002 depends on 0001's table, proving lexicographic apply order.
// The README must be ignored (only *.sql counts).
func migFS() fstest.MapFS {
	return fstest.MapFS{
		"0001_users.sql": {Data: []byte(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL);`)},
		"0002_seed.sql": {Data: []byte(`INSERT INTO users (name) VALUES ('alpha');
CREATE TABLE widgets (id INTEGER PRIMARY KEY);`)},
		"README.md": {Data: []byte("not a migration")},
	}
}

func TestMigrateAppliesAllInOrder(t *testing.T) {
	db := openRawDB(t)
	applied, err := Migrate(context.Background(), db, migFS())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	want := []string{"0001_users.sql", "0002_seed.sql"}
	if !slices.Equal(applied, want) {
		t.Errorf("applied = %v, want %v", applied, want)
	}
	var users, recorded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Errorf("users rows = %d, want 1 (seed must run after create)", users)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&recorded); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if recorded != 2 {
		t.Errorf("schema_migrations rows = %d, want 2", recorded)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	db := openRawDB(t)
	fsys := migFS()
	if _, err := Migrate(context.Background(), db, fsys); err != nil {
		t.Fatalf("first run: %v", err)
	}
	applied, err := Migrate(context.Background(), db, fsys)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("second run applied %v, want nothing", applied)
	}
	var users int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Errorf("users rows = %d, want 1 (seed must not run twice)", users)
	}
}

func TestMigrateAppliesOnlyNewFiles(t *testing.T) {
	db := openRawDB(t)
	first := fstest.MapFS{"0001_users.sql": migFS()["0001_users.sql"]}
	if _, err := Migrate(context.Background(), db, first); err != nil {
		t.Fatalf("first run: %v", err)
	}
	applied, err := Migrate(context.Background(), db, migFS())
	if err != nil {
		t.Fatalf("second run with extra file: %v", err)
	}
	if !slices.Equal(applied, []string{"0002_seed.sql"}) {
		t.Errorf("applied = %v, want [0002_seed.sql]", applied)
	}
}

func TestMigrateRollsBackFailedMigration(t *testing.T) {
	db := openRawDB(t)
	bad := fstest.MapFS{
		"0001_bad.sql": {Data: []byte(`CREATE TABLE good (id INTEGER PRIMARY KEY);
INSERT INTO does_not_exist VALUES (1);`)},
	}
	if _, err := Migrate(context.Background(), db, bad); err == nil {
		t.Fatal("Migrate succeeded on a failing migration")
	}
	// The CREATE from the same file must have been rolled back.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM good`).Scan(&n); err == nil {
		t.Error("table 'good' exists after failed migration — not transactional")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if n != 0 {
		t.Errorf("failed migration was recorded (%d rows), want 0", n)
	}
}
```

- [ ] **Step 2: Run test, expect compile failure**

Run: `go test ./internal/server/store/`

Expected:
```
internal/server/store/migrate_test.go:38:18: undefined: Migrate
...
FAIL	github.com/abd-ulbasit/upgradescope/internal/server/store [build failed]
```

- [ ] **Step 3: Write migrate.go**

Create `internal/server/store/migrate.go`:

```go
package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"slices"
	"time"
)

// Migrate applies every "*.sql" file in fsys that has not been applied yet,
// in lexicographic filename order. Each migration runs in its own
// transaction and its filename is recorded in schema_migrations inside that
// same transaction, so a failed migration leaves no trace. Running Migrate
// again is a no-op for already-recorded files. Returns the filenames
// applied on this run.
//
// Driver-agnostic on purpose (plain *sql.DB): P3's Postgres store reuses it.
func Migrate(ctx context.Context, db *sql.DB, fsys fs.FS) ([]string, error) {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("glob migrations: %w", err)
	}
	slices.Sort(names)
	var applied []string
	for _, name := range names {
		ok, err := applyMigration(ctx, db, fsys, name)
		if err != nil {
			return applied, err
		}
		if ok {
			applied = append(applied, name)
		}
	}
	return applied, nil
}

func applyMigration(ctx context.Context, db *sql.DB, fsys fs.FS, name string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, name).Scan(&n); err != nil {
		return false, fmt.Errorf("check migration %s: %w", name, err)
	}
	if n > 0 {
		return false, nil
	}
	src, err := fs.ReadFile(fsys, name)
	if err != nil {
		return false, fmt.Errorf("read migration %s: %w", name, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin migration %s: %w", name, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.ExecContext(ctx, string(src)); err != nil {
		return false, fmt.Errorf("apply migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		name, formatTime(time.Now())); err != nil {
		return false, fmt.Errorf("record migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit migration %s: %w", name, err)
	}
	return true, nil
}
```

- [ ] **Step 4: Run test, expect pass**

Run: `go test ./internal/server/store/ -v -run TestMigrate`

Expected: `TestMigrateAppliesAllInOrder`, `TestMigrateIdempotent`, `TestMigrateAppliesOnlyNewFiles`, `TestMigrateRollsBackFailedMigration` PASS; the S1 tests still pass under the full run (`go test ./internal/server/store/`).

- [ ] **Step 5: Commit**

```bash
git add internal/server/store/ && git commit -m "feat: add embedded SQL migration runner for server store" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task S3: SQLite implementation — schema, Open, all Store methods

**Files:**
- Create: `internal/server/store/migrations/0001_init.sql`
- Create: `internal/server/store/sqlite.go`
- Test: `internal/server/store/sqlite_test.go`

One red→green cycle: the whole test file fails to build until `Open` and the methods exist. Tests run against real files in `t.TempDir()`; raw-SQL assertions (`s.db` is accessible — tests live in `package store`) pin the storage representation, not just the interface behavior.

- [ ] **Step 1: Write failing tests for Open, pragmas, and every Store method**

Create `internal/server/store/sqlite_test.go`:

```go
package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

var tBase = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

func tPlus(h int) time.Time { return tBase.Add(time.Duration(h) * time.Hour) }

func newTestStore(t *testing.T) *SQLite {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustCluster(t *testing.T, s *SQLite, name string) int64 {
	t.Helper()
	id, err := s.UpsertCluster(context.Background(),
		Cluster{Name: name, ClusterUID: "uid-" + name, FirstSeen: tBase, LastSeen: tBase})
	if err != nil {
		t.Fatalf("UpsertCluster(%s): %v", name, err)
	}
	return id
}

func mustSnapshot(t *testing.T, s *SQLite, clusterID int64, hash string, at time.Time) int64 {
	t.Helper()
	id, dup, err := s.InsertSnapshot(context.Background(), Snapshot{
		ClusterID: clusterID, Hash: hash, KBVersion: "kb-1", AgentVersion: "v0.2.0",
		ReceivedAt: at, Inventory: []byte(`{"hash":"` + hash + `"}`),
	})
	if err != nil {
		t.Fatalf("InsertSnapshot(%s): %v", hash, err)
	}
	if dup {
		t.Fatalf("InsertSnapshot(%s): unexpected duplicate", hash)
	}
	return id
}

func TestOpenSetsPragmas(t *testing.T) {
	s := newTestStore(t)
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var fk int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
	var busy int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if busy != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busy)
	}
}

func TestOpenIdempotentAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	id, err := s1.UpsertCluster(ctx, Cluster{Name: "prod", FirstSeen: tBase, LastSeen: tBase})
	if err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path) // migrations must be idempotent on an existing file
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer s2.Close()
	got, err := s2.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster after reopen: %v", err)
	}
	if got.Name != "prod" {
		t.Errorf("Name = %q, want prod", got.Name)
	}
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if n != 1 {
		t.Errorf("schema_migrations rows = %d, want 1 (0001 only, applied once)", n)
	}
	for _, table := range []string{"clusters", "snapshots", "evaluations"} {
		var name string
		if err := s2.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
}

func TestUpsertClusterInsertThenUpdate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id, err := s.UpsertCluster(ctx, Cluster{Name: "prod-eu-1", ClusterUID: "uid-1", FirstSeen: tBase, LastSeen: tBase})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id <= 0 {
		t.Fatalf("insert returned id %d, want > 0", id)
	}

	id2, err := s.UpsertCluster(ctx, Cluster{Name: "prod-eu-1", ClusterUID: "uid-1b", FirstSeen: tPlus(1), LastSeen: tPlus(1)})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if id2 != id {
		t.Errorf("upsert by existing name returned id %d, want %d", id2, id)
	}

	got, err := s.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if !got.FirstSeen.Equal(tBase) {
		t.Errorf("FirstSeen = %v, want %v (must not move on update)", got.FirstSeen, tBase)
	}
	if !got.LastSeen.Equal(tPlus(1)) {
		t.Errorf("LastSeen = %v, want %v (must bump on update)", got.LastSeen, tPlus(1))
	}
	if got.ClusterUID != "uid-1b" {
		t.Errorf("ClusterUID = %q, want uid-1b", got.ClusterUID)
	}

	if _, err := s.UpsertCluster(ctx, Cluster{Name: "dev-1", FirstSeen: tBase, LastSeen: tBase}); err != nil {
		t.Fatalf("second cluster: %v", err)
	}
	list, err := s.ListClusters(ctx)
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(list) != 2 || list[0].Name != "dev-1" || list[1].Name != "prod-eu-1" {
		t.Errorf("ListClusters = %+v, want [dev-1 prod-eu-1] sorted by name", list)
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM clusters WHERE name = 'prod-eu-1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("rows for prod-eu-1 = %d, want 1 (name UNIQUE upsert)", n)
	}
}

func TestUpsertClusterZeroTimesDefaultToNow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	before := time.Now().UTC().Add(-time.Minute)
	id, err := s.UpsertCluster(ctx, Cluster{Name: "zero-times"})
	if err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}
	after := time.Now().UTC().Add(time.Minute)
	got, err := s.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	for name, ts := range map[string]time.Time{"FirstSeen": got.FirstSeen, "LastSeen": got.LastSeen} {
		if ts.Before(before) || ts.After(after) {
			t.Errorf("%s = %v, want within [%v, %v]", name, ts, before, after)
		}
	}
}

func TestInsertSnapshotDedup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cid := mustCluster(t, s, "prod")

	idA := mustSnapshot(t, s, cid, "aaa", tBase)

	// Same hash as latest → duplicate, no insert, existing id returned.
	dupID, dup, err := s.InsertSnapshot(ctx, Snapshot{
		ClusterID: cid, Hash: "aaa", ReceivedAt: tPlus(1), Inventory: []byte(`{"hash":"aaa"}`),
	})
	if err != nil {
		t.Fatalf("duplicate push: %v", err)
	}
	if !dup {
		t.Error("duplicate = false, want true (same hash as latest)")
	}
	if dupID != idA {
		t.Errorf("duplicate id = %d, want existing %d", dupID, idA)
	}

	// Different hash → new row.
	idB := mustSnapshot(t, s, cid, "bbb", tPlus(2))
	if idB == idA {
		t.Errorf("new hash reused id %d", idB)
	}

	// Hash "aaa" again: it is no longer the LATEST (superseded by "bbb"),
	// so this is NOT a duplicate — the cluster changed back.
	idA2, dup, err := s.InsertSnapshot(ctx, Snapshot{
		ClusterID: cid, Hash: "aaa", ReceivedAt: tPlus(3), Inventory: []byte(`{"hash":"aaa"}`),
	})
	if err != nil {
		t.Fatalf("superseded re-push: %v", err)
	}
	if dup {
		t.Error("duplicate = true for superseded hash, want false")
	}
	if idA2 == idA || idA2 == idB {
		t.Errorf("superseded re-push id = %d, want a fresh row", idA2)
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE cluster_id = ?`, cid).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Errorf("snapshot rows = %d, want 3 (aaa, bbb, aaa-again; dup not stored)", n)
	}
}

func TestLatestSnapshotRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cid := mustCluster(t, s, "prod")
	mustSnapshot(t, s, cid, "aaa", tBase)
	idB := mustSnapshot(t, s, cid, "bbb", tPlus(1))

	got, err := s.LatestSnapshot(ctx, cid)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if got.ID != idB || got.Hash != "bbb" || got.ClusterID != cid {
		t.Errorf("got id=%d hash=%q cluster=%d, want id=%d hash=bbb cluster=%d",
			got.ID, got.Hash, got.ClusterID, idB, cid)
	}
	if got.KBVersion != "kb-1" || got.AgentVersion != "v0.2.0" {
		t.Errorf("versions = %q/%q, want kb-1/v0.2.0", got.KBVersion, got.AgentVersion)
	}
	if !got.ReceivedAt.Equal(tPlus(1)) || got.ReceivedAt.Location() != time.UTC {
		t.Errorf("ReceivedAt = %v (%v), want %v UTC", got.ReceivedAt, got.ReceivedAt.Location(), tPlus(1))
	}
	if !bytes.Equal(got.Inventory, []byte(`{"hash":"bbb"}`)) {
		t.Errorf("Inventory = %s, want raw bytes back", got.Inventory)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	s := newTestStore(t)
	_, _, err := s.InsertSnapshot(context.Background(), Snapshot{
		ClusterID: 12345, Hash: "x", ReceivedAt: tBase, Inventory: []byte("{}"),
	})
	if err == nil {
		t.Fatal("insert with bogus cluster_id succeeded — foreign_keys pragma not effective")
	}
}

func TestEvaluationsLatestPerTarget(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", tBase)

	ins := func(target string, score int, ready bool, at time.Time, report []byte) {
		t.Helper()
		_, err := s.InsertEvaluation(ctx, Evaluation{
			ClusterID: cid, SnapshotID: sid, Target: target, KBVersion: "kb-1",
			Score: score, Ready: ready, Blockers: 2, Warnings: 3,
			Report: report, CreatedAt: at,
		})
		if err != nil {
			t.Fatalf("InsertEvaluation(%s,%d): %v", target, score, err)
		}
	}
	ins("1.36", 70, false, tBase, []byte(`{"score":70}`))
	ins("1.36", 80, true, tPlus(1), []byte(`{"score":80}`))
	ins("1.37", 55, false, tBase, []byte(`{"score":55}`))

	got, err := s.LatestEvaluation(ctx, cid, "1.36")
	if err != nil {
		t.Fatalf("LatestEvaluation(1.36): %v", err)
	}
	if got.Score != 80 || !got.Ready || !got.CreatedAt.Equal(tPlus(1)) {
		t.Errorf("latest 1.36 = score %d ready %v at %v, want 80/true/%v", got.Score, got.Ready, got.CreatedAt, tPlus(1))
	}
	if got.ClusterID != cid || got.SnapshotID != sid || got.Target != "1.36" || got.KBVersion != "kb-1" {
		t.Errorf("identity fields wrong: %+v", got)
	}
	if got.Blockers != 2 || got.Warnings != 3 {
		t.Errorf("Blockers/Warnings = %d/%d, want 2/3", got.Blockers, got.Warnings)
	}
	if !bytes.Equal(got.Report, []byte(`{"score":80}`)) {
		t.Errorf("Report = %s, want {\"score\":80}", got.Report)
	}

	got37, err := s.LatestEvaluation(ctx, cid, "1.37")
	if err != nil {
		t.Fatalf("LatestEvaluation(1.37): %v", err)
	}
	if got37.Score != 55 {
		t.Errorf("latest 1.37 score = %d, want 55", got37.Score)
	}

	if _, err := s.LatestEvaluation(ctx, cid, "1.38"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LatestEvaluation(1.38) err = %v, want ErrNotFound", err)
	}
}

func TestScoreHistoryOrderingAndLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", tBase)

	scores := []struct {
		score int
		ready bool
		at    time.Time
	}{
		{70, false, tBase}, {75, false, tPlus(1)}, {80, false, tPlus(2)}, {92, true, tPlus(3)},
	}
	for _, e := range scores {
		if _, err := s.InsertEvaluation(ctx, Evaluation{
			ClusterID: cid, SnapshotID: sid, Target: "1.36",
			Score: e.score, Ready: e.ready, CreatedAt: e.at,
		}); err != nil {
			t.Fatalf("InsertEvaluation: %v", err)
		}
	}
	// Different target must not leak into 1.36 history.
	if _, err := s.InsertEvaluation(ctx, Evaluation{
		ClusterID: cid, SnapshotID: sid, Target: "1.37", Score: 10, CreatedAt: tBase,
	}); err != nil {
		t.Fatalf("InsertEvaluation(1.37): %v", err)
	}

	tests := []struct {
		name       string
		limit      int
		wantScores []int
	}{
		{"zero limit means all", 0, []int{70, 75, 80, 92}},
		{"negative limit means all", -1, []int{70, 75, 80, 92}},
		{"limit larger than rows", 10, []int{70, 75, 80, 92}},
		{"most recent 2, returned oldest-first", 2, []int{80, 92}},
		{"most recent 1", 1, []int{92}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ScoreHistory(ctx, cid, "1.36", tt.limit)
			if err != nil {
				t.Fatalf("ScoreHistory: %v", err)
			}
			if len(got) != len(tt.wantScores) {
				t.Fatalf("got %d points, want %d: %+v", len(got), len(tt.wantScores), got)
			}
			for i, p := range got {
				if p.Score != tt.wantScores[i] {
					t.Errorf("point %d score = %d, want %d", i, p.Score, tt.wantScores[i])
				}
				if i > 0 && !got[i-1].At.Before(p.At) {
					t.Errorf("points not ascending by At: %v then %v", got[i-1].At, p.At)
				}
			}
			if last := got[len(got)-1]; last.Score == 92 && !last.Ready {
				t.Error("ready flag lost on final point")
			}
		})
	}

	// Unknown cluster: empty history, nil error — NOT ErrNotFound.
	empty, err := s.ScoreHistory(ctx, 999, "1.36", 5)
	if err != nil {
		t.Errorf("ScoreHistory(unknown) err = %v, want nil", err)
	}
	if len(empty) != 0 {
		t.Errorf("ScoreHistory(unknown) = %+v, want empty", empty)
	}
}

func TestNotFoundSentinels(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	tests := []struct {
		name string
		call func() error
	}{
		{"GetCluster", func() error { _, err := s.GetCluster(ctx, 999); return err }},
		{"LatestSnapshot", func() error { _, err := s.LatestSnapshot(ctx, 999); return err }},
		{"LatestEvaluation", func() error { _, err := s.LatestEvaluation(ctx, 999, "1.36"); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v, want errors.Is(err, ErrNotFound)", err)
			}
		})
	}
}

func TestTimesStoredUTCFixedWidth(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	pkt := time.FixedZone("PKT", 5*3600)
	zoned := time.Date(2026, 6, 10, 17, 0, 0, 123456789, pkt) // == 12:00:00.123456789Z

	id, err := s.UpsertCluster(ctx, Cluster{Name: "tz", FirstSeen: zoned, LastSeen: zoned})
	if err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}
	got, err := s.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if !got.FirstSeen.Equal(zoned) {
		t.Errorf("FirstSeen = %v, want same instant as %v", got.FirstSeen, zoned)
	}
	if got.FirstSeen.Location() != time.UTC {
		t.Errorf("FirstSeen location = %v, want UTC", got.FirstSeen.Location())
	}

	var raw string
	if err := s.db.QueryRow(`SELECT first_seen FROM clusters WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	const want = "2026-06-10T12:00:00.123456789Z"
	if raw != want {
		t.Errorf("stored text = %q, want %q (UTC, RFC 3339, fixed 9-digit nanos)", raw, want)
	}
}

func TestCloseThenOperationsFail(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.GetCluster(context.Background(), 1); err == nil {
		t.Error("GetCluster after Close succeeded, want error")
	}
}
```

- [ ] **Step 2: Run test, expect compile failure**

Run: `go test ./internal/server/store/`

Expected:
```
internal/server/store/sqlite_test.go:18:12: undefined: Open
internal/server/store/sqlite_test.go:18:36: undefined: SQLite
...
FAIL	github.com/abd-ulbasit/upgradescope/internal/server/store [build failed]
```

- [ ] **Step 3: Write the schema**

Create `internal/server/store/migrations/0001_init.sql`:

```sql
-- 0001_init.sql — clusters / snapshots / evaluations.
-- Times are TEXT: RFC 3339 UTC with fixed nine-digit fractional seconds,
-- so lexicographic order == instant order. Booleans are INTEGER 0/1.
-- AUTOINCREMENT guarantees ids are strictly monotonic (insertion order):
-- "latest snapshot" relies on max(id).

CREATE TABLE clusters (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    cluster_uid TEXT    NOT NULL DEFAULT '',
    first_seen  TEXT    NOT NULL,
    last_seen   TEXT    NOT NULL
);

CREATE TABLE snapshots (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    cluster_id    INTEGER NOT NULL REFERENCES clusters(id),
    hash          TEXT    NOT NULL,
    kb_version    TEXT    NOT NULL DEFAULT '',
    agent_version TEXT    NOT NULL DEFAULT '',
    received_at   TEXT    NOT NULL,
    inventory     BLOB    NOT NULL
);

CREATE INDEX idx_snapshots_cluster_received
    ON snapshots (cluster_id, received_at);

CREATE TABLE evaluations (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    cluster_id  INTEGER NOT NULL REFERENCES clusters(id),
    snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
    target      TEXT    NOT NULL,
    kb_version  TEXT    NOT NULL DEFAULT '',
    score       INTEGER NOT NULL,
    ready       INTEGER NOT NULL,
    blockers    INTEGER NOT NULL,
    warnings    INTEGER NOT NULL,
    report      BLOB,
    created_at  TEXT    NOT NULL
);

CREATE INDEX idx_evaluations_cluster_target_created
    ON evaluations (cluster_id, target, created_at);
```

- [ ] **Step 4: Write sqlite.go**

Create `internal/server/store/sqlite.go`:

```go
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	_ "modernc.org/sqlite" // database/sql driver, registered as "sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// SQLite is the Store implementation backed by a single SQLite database
// file (modernc.org/sqlite — pure Go, CGO-free).
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// Open opens (creating if needed) the database at path and applies all
// embedded migrations. Every pooled connection gets WAL journaling, a 5s
// busy timeout and foreign-key enforcement via DSN pragmas.
//
// path must not contain '?' — it is interpolated into a SQLite URI.
func Open(path string) (*SQLite, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("embedded migrations: %w", err)
	}
	if _, err := Migrate(context.Background(), db, sub); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &SQLite{db: db}, nil
}

// Close closes the underlying database.
func (s *SQLite) Close() error { return s.db.Close() }

// UpsertCluster inserts the cluster or, if a row with the same name exists,
// updates cluster_uid and last_seen (first_seen never moves). Zero
// FirstSeen/LastSeen default to time.Now().UTC().
func (s *SQLite) UpsertCluster(ctx context.Context, c Cluster) (int64, error) {
	now := time.Now().UTC()
	first, last := c.FirstSeen, c.LastSeen
	if first.IsZero() {
		first = now
	}
	if last.IsZero() {
		last = now
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO clusters (name, cluster_uid, first_seen, last_seen)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			cluster_uid = excluded.cluster_uid,
			last_seen   = excluded.last_seen
		RETURNING id`,
		c.Name, c.ClusterUID, formatTime(first), formatTime(last)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert cluster %q: %w", c.Name, err)
	}
	return id, nil
}

// rowScanner abstracts *sql.Row and *sql.Rows for shared scan helpers.
type rowScanner interface{ Scan(dest ...any) error }

func scanCluster(rs rowScanner) (Cluster, error) {
	var c Cluster
	var first, last string
	if err := rs.Scan(&c.ID, &c.Name, &c.ClusterUID, &first, &last); err != nil {
		return Cluster{}, err
	}
	var err error
	if c.FirstSeen, err = parseStoredTime(first); err != nil {
		return Cluster{}, err
	}
	if c.LastSeen, err = parseStoredTime(last); err != nil {
		return Cluster{}, err
	}
	return c, nil
}

// GetCluster returns the cluster by id, or ErrNotFound.
func (s *SQLite) GetCluster(ctx context.Context, id int64) (Cluster, error) {
	c, err := scanCluster(s.db.QueryRowContext(ctx,
		`SELECT id, name, cluster_uid, first_seen, last_seen FROM clusters WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Cluster{}, fmt.Errorf("cluster %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Cluster{}, fmt.Errorf("get cluster %d: %w", id, err)
	}
	return c, nil
}

// ListClusters returns all clusters, ascending by name.
func (s *SQLite) ListClusters(ctx context.Context) ([]Cluster, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, cluster_uid, first_seen, last_seen FROM clusters ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	defer rows.Close()
	var out []Cluster
	for rows.Next() {
		c, err := scanCluster(rows)
		if err != nil {
			return nil, fmt.Errorf("list clusters: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	return out, nil
}

// InsertSnapshot stores snap unless its hash equals the hash of the
// cluster's LATEST snapshot, in which case it returns (latestID, true, nil)
// without writing. An older same-hash snapshot superseded by a different
// one does NOT count as a duplicate. Zero ReceivedAt defaults to now (UTC).
func (s *SQLite) InsertSnapshot(ctx context.Context, snap Snapshot) (int64, bool, error) {
	received := snap.ReceivedAt
	if received.IsZero() {
		received = time.Now().UTC()
	}
	inv := snap.Inventory
	if inv == nil {
		inv = []byte{}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("insert snapshot: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var latestID int64
	var latestHash string
	err = tx.QueryRowContext(ctx,
		`SELECT id, hash FROM snapshots WHERE cluster_id = ? ORDER BY id DESC LIMIT 1`,
		snap.ClusterID).Scan(&latestID, &latestHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// first snapshot for this cluster — fall through to insert
	case err != nil:
		return 0, false, fmt.Errorf("insert snapshot: query latest: %w", err)
	case latestHash == snap.Hash:
		return latestID, true, nil
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO snapshots (cluster_id, hash, kb_version, agent_version, received_at, inventory)
		VALUES (?, ?, ?, ?, ?, ?)`,
		snap.ClusterID, snap.Hash, snap.KBVersion, snap.AgentVersion, formatTime(received), inv)
	if err != nil {
		return 0, false, fmt.Errorf("insert snapshot: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("insert snapshot: id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("insert snapshot: commit: %w", err)
	}
	return id, false, nil
}

// LatestSnapshot returns the most recently inserted snapshot for the
// cluster (highest id), or ErrNotFound.
func (s *SQLite) LatestSnapshot(ctx context.Context, clusterID int64) (Snapshot, error) {
	var snap Snapshot
	var received string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, cluster_id, hash, kb_version, agent_version, received_at, inventory
		FROM snapshots WHERE cluster_id = ? ORDER BY id DESC LIMIT 1`, clusterID).
		Scan(&snap.ID, &snap.ClusterID, &snap.Hash, &snap.KBVersion, &snap.AgentVersion, &received, &snap.Inventory)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("latest snapshot for cluster %d: %w", clusterID, ErrNotFound)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("latest snapshot for cluster %d: %w", clusterID, err)
	}
	if snap.ReceivedAt, err = parseStoredTime(received); err != nil {
		return Snapshot{}, fmt.Errorf("latest snapshot for cluster %d: %w", clusterID, err)
	}
	return snap, nil
}

// InsertEvaluation stores e. Zero CreatedAt defaults to now (UTC).
func (s *SQLite) InsertEvaluation(ctx context.Context, e Evaluation) (int64, error) {
	created := e.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO evaluations (cluster_id, snapshot_id, target, kb_version, score, ready, blockers, warnings, report, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ClusterID, e.SnapshotID, e.Target, e.KBVersion, e.Score, e.Ready, e.Blockers, e.Warnings, e.Report, formatTime(created))
	if err != nil {
		return 0, fmt.Errorf("insert evaluation: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert evaluation: id: %w", err)
	}
	return id, nil
}

// LatestEvaluation returns the newest evaluation for (cluster, target) by
// created_at (ties broken by id), or ErrNotFound.
func (s *SQLite) LatestEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error) {
	var e Evaluation
	var created string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, cluster_id, snapshot_id, target, kb_version, score, ready, blockers, warnings, report, created_at
		FROM evaluations WHERE cluster_id = ? AND target = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, clusterID, target).
		Scan(&e.ID, &e.ClusterID, &e.SnapshotID, &e.Target, &e.KBVersion,
			&e.Score, &e.Ready, &e.Blockers, &e.Warnings, &e.Report, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Evaluation{}, fmt.Errorf("latest evaluation for cluster %d target %s: %w", clusterID, target, ErrNotFound)
	}
	if err != nil {
		return Evaluation{}, fmt.Errorf("latest evaluation for cluster %d target %s: %w", clusterID, target, err)
	}
	if e.CreatedAt, err = parseStoredTime(created); err != nil {
		return Evaluation{}, fmt.Errorf("latest evaluation for cluster %d target %s: %w", clusterID, target, err)
	}
	return e, nil
}

// ScoreHistory returns score points for (cluster, target), oldest-first
// ascending by created_at. limit > 0 selects the most recent N rows (still
// returned oldest-first); limit <= 0 returns all. An unknown cluster or
// target yields an empty slice and nil error.
func (s *SQLite) ScoreHistory(ctx context.Context, clusterID int64, target string, limit int) ([]ScorePoint, error) {
	lim := int64(limit)
	if limit <= 0 {
		lim = -1 // SQLite: LIMIT -1 == no limit
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT created_at, score, ready FROM (
			SELECT id, created_at, score, ready FROM evaluations
			WHERE cluster_id = ? AND target = ?
			ORDER BY created_at DESC, id DESC LIMIT ?
		) ORDER BY created_at ASC, id ASC`, clusterID, target, lim)
	if err != nil {
		return nil, fmt.Errorf("score history cluster %d target %s: %w", clusterID, target, err)
	}
	defer rows.Close()
	var out []ScorePoint
	for rows.Next() {
		var p ScorePoint
		var created string
		if err := rows.Scan(&created, &p.Score, &p.Ready); err != nil {
			return nil, fmt.Errorf("score history cluster %d target %s: %w", clusterID, target, err)
		}
		if p.At, err = parseStoredTime(created); err != nil {
			return nil, fmt.Errorf("score history cluster %d target %s: %w", clusterID, target, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("score history cluster %d target %s: %w", clusterID, target, err)
	}
	return out, nil
}
```

- [ ] **Step 5: Run test, expect pass**

Run: `go test ./internal/server/store/ -v`

Expected PASS: `TestOpenSetsPragmas`, `TestOpenIdempotentAcrossReopen`, `TestUpsertClusterInsertThenUpdate`, `TestUpsertClusterZeroTimesDefaultToNow`, `TestInsertSnapshotDedup`, `TestLatestSnapshotRoundTrip`, `TestForeignKeysEnforced`, `TestEvaluationsLatestPerTarget`, `TestScoreHistoryOrderingAndLimit` (5 subtests), `TestNotFoundSentinels` (3 subtests), `TestTimesStoredUTCFixedWidth`, `TestCloseThenOperationsFail` — plus all S1/S2 tests; `ok github.com/abd-ulbasit/upgradescope/internal/server/store`.

Also run `go vet ./internal/server/store/` — clean.

- [ ] **Step 6: Commit**

```bash
git add internal/server/store/ && git commit -m "feat: add SQLite store — WAL, snapshot dedup, score history" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task S4: storetest conformance suite (reused by P3's Postgres store)

**Files:**
- Create: `internal/server/store/storetest/storetest.go`
- Test: `internal/server/store/conformance_test.go` (external test package — `package store_test`; an internal test file cannot import storetest because storetest imports store, which would be a test import cycle)

The suite exercises ONLY the `store.Store` interface — no pragmas, no raw SQL, no file paths. Overlap with S3's tests is deliberate: S3 pins the SQLite storage representation; storetest pins the portable behavioral contract that P3's Postgres implementation must also pass.

- [ ] **Step 1: Write the failing conformance invocation**

Create `internal/server/store/conformance_test.go`:

```go
package store_test

import (
	"path/filepath"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
	"github.com/abd-ulbasit/upgradescope/internal/server/store/storetest"
)

func TestSQLiteConformance(t *testing.T) {
	storetest.RunStoreConformance(t, func(t *testing.T) store.Store {
		s, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
		if err != nil {
			t.Fatalf("open sqlite store: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
```

- [ ] **Step 2: Run test, expect compile failure**

Run: `go test ./internal/server/store/...`

Expected:
```
internal/server/store/conformance_test.go:8:2: no required module provides package github.com/abd-ulbasit/upgradescope/internal/server/store/storetest
FAIL	github.com/abd-ulbasit/upgradescope/internal/server/store [setup failed]
```

- [ ] **Step 3: Write the storetest package**

Create `internal/server/store/storetest/storetest.go`:

```go
// Package storetest pins the behavioral contract of store.Store.
//
// Any implementation (SQLite in P2, Postgres in P3) must pass
// RunStoreConformance. The suite touches only the exported interface —
// nothing driver-specific (pragmas, raw SQL, file layout) is asserted here;
// implementation packages pin their own storage representation.
package storetest

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// NewStoreFunc returns a fresh, empty Store for one subtest. Implementations
// must register cleanup on t (t.Cleanup / t.TempDir); the suite never calls
// Close except where Close semantics are themselves under test.
type NewStoreFunc func(t *testing.T) store.Store

var base = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

func at(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }

// RunStoreConformance runs the full behavioral suite against implementations
// produced by newStore. Each subtest gets its own fresh store.
func RunStoreConformance(t *testing.T, newStore NewStoreFunc) {
	t.Run("UpsertClusterInsertThenUpdate", func(t *testing.T) { testUpsertCluster(t, newStore(t)) })
	t.Run("UpsertClusterZeroTimesDefault", func(t *testing.T) { testZeroTimes(t, newStore(t)) })
	t.Run("SnapshotDedup", func(t *testing.T) { testSnapshotDedup(t, newStore(t)) })
	t.Run("LatestSnapshotRoundTrip", func(t *testing.T) { testLatestSnapshot(t, newStore(t)) })
	t.Run("EvaluationsLatestPerTarget", func(t *testing.T) { testEvaluations(t, newStore(t)) })
	t.Run("ScoreHistoryOldestFirstLimitNewest", func(t *testing.T) { testScoreHistory(t, newStore(t)) })
	t.Run("NotFound", func(t *testing.T) { testNotFound(t, newStore(t)) })
	t.Run("Close", func(t *testing.T) { testClose(t, newStore(t)) })
}

func mustCluster(t *testing.T, s store.Store, name string) int64 {
	t.Helper()
	id, err := s.UpsertCluster(context.Background(),
		store.Cluster{Name: name, ClusterUID: "uid-" + name, FirstSeen: base, LastSeen: base})
	if err != nil {
		t.Fatalf("UpsertCluster(%s): %v", name, err)
	}
	return id
}

func mustSnapshot(t *testing.T, s store.Store, clusterID int64, hash string, received time.Time) int64 {
	t.Helper()
	id, dup, err := s.InsertSnapshot(context.Background(), store.Snapshot{
		ClusterID: clusterID, Hash: hash, KBVersion: "kb-1", AgentVersion: "v0.2.0",
		ReceivedAt: received, Inventory: []byte(`{"hash":"` + hash + `"}`),
	})
	if err != nil {
		t.Fatalf("InsertSnapshot(%s): %v", hash, err)
	}
	if dup {
		t.Fatalf("InsertSnapshot(%s): unexpected duplicate", hash)
	}
	return id
}

func testUpsertCluster(t *testing.T, s store.Store) {
	ctx := context.Background()
	id, err := s.UpsertCluster(ctx, store.Cluster{Name: "prod-eu-1", ClusterUID: "uid-1", FirstSeen: base, LastSeen: base})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id <= 0 {
		t.Fatalf("insert returned id %d, want > 0", id)
	}
	id2, err := s.UpsertCluster(ctx, store.Cluster{Name: "prod-eu-1", ClusterUID: "uid-1b", FirstSeen: at(1), LastSeen: at(1)})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if id2 != id {
		t.Errorf("upsert by existing name returned id %d, want %d", id2, id)
	}
	got, err := s.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if !got.FirstSeen.Equal(base) {
		t.Errorf("FirstSeen = %v, want %v (must not move on update)", got.FirstSeen, base)
	}
	if !got.LastSeen.Equal(at(1)) {
		t.Errorf("LastSeen = %v, want %v (must bump on update)", got.LastSeen, at(1))
	}
	if got.ClusterUID != "uid-1b" {
		t.Errorf("ClusterUID = %q, want uid-1b", got.ClusterUID)
	}
	if got.FirstSeen.Location() != time.UTC || got.LastSeen.Location() != time.UTC {
		t.Errorf("times must come back UTC, got %v / %v", got.FirstSeen.Location(), got.LastSeen.Location())
	}
	if _, err := s.UpsertCluster(ctx, store.Cluster{Name: "dev-1", FirstSeen: base, LastSeen: base}); err != nil {
		t.Fatalf("second cluster: %v", err)
	}
	list, err := s.ListClusters(ctx)
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	if len(list) != 2 || list[0].Name != "dev-1" || list[1].Name != "prod-eu-1" {
		t.Errorf("ListClusters = %+v, want [dev-1 prod-eu-1] ascending by name", list)
	}
}

func testZeroTimes(t *testing.T, s store.Store) {
	ctx := context.Background()
	before := time.Now().UTC().Add(-time.Minute)
	id, err := s.UpsertCluster(ctx, store.Cluster{Name: "zero-times"})
	if err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}
	after := time.Now().UTC().Add(time.Minute)
	got, err := s.GetCluster(ctx, id)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	for name, ts := range map[string]time.Time{"FirstSeen": got.FirstSeen, "LastSeen": got.LastSeen} {
		if ts.Before(before) || ts.After(after) {
			t.Errorf("%s = %v, want defaulted to now (within [%v, %v])", name, ts, before, after)
		}
	}
}

func testSnapshotDedup(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	idA := mustSnapshot(t, s, cid, "aaa", base)

	dupID, dup, err := s.InsertSnapshot(ctx, store.Snapshot{
		ClusterID: cid, Hash: "aaa", ReceivedAt: at(1), Inventory: []byte(`{"hash":"aaa"}`),
	})
	if err != nil {
		t.Fatalf("duplicate push: %v", err)
	}
	if !dup {
		t.Error("duplicate = false, want true (same hash as latest)")
	}
	if dupID != idA {
		t.Errorf("duplicate id = %d, want existing %d", dupID, idA)
	}
	if latest, err := s.LatestSnapshot(ctx, cid); err != nil || latest.ID != idA {
		t.Errorf("latest after dup = (%+v, %v), want id %d", latest, err, idA)
	}

	idB := mustSnapshot(t, s, cid, "bbb", at(2))
	if idB == idA {
		t.Errorf("new hash reused id %d", idB)
	}

	// "aaa" again: superseded by "bbb", so NOT a duplicate — new row.
	idA2, dup, err := s.InsertSnapshot(ctx, store.Snapshot{
		ClusterID: cid, Hash: "aaa", ReceivedAt: at(3), Inventory: []byte(`{"hash":"aaa"}`),
	})
	if err != nil {
		t.Fatalf("superseded re-push: %v", err)
	}
	if dup {
		t.Error("duplicate = true for superseded hash, want false")
	}
	if idA2 == idA || idA2 == idB {
		t.Errorf("superseded re-push id = %d, want a fresh row", idA2)
	}
	latest, err := s.LatestSnapshot(ctx, cid)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if latest.ID != idA2 || latest.Hash != "aaa" {
		t.Errorf("latest = id %d hash %q, want id %d hash aaa", latest.ID, latest.Hash, idA2)
	}
}

func testLatestSnapshot(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	mustSnapshot(t, s, cid, "aaa", base)
	idB := mustSnapshot(t, s, cid, "bbb", at(1))

	got, err := s.LatestSnapshot(ctx, cid)
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if got.ID != idB || got.Hash != "bbb" || got.ClusterID != cid {
		t.Errorf("got id=%d hash=%q cluster=%d, want id=%d hash=bbb cluster=%d",
			got.ID, got.Hash, got.ClusterID, idB, cid)
	}
	if got.KBVersion != "kb-1" || got.AgentVersion != "v0.2.0" {
		t.Errorf("versions = %q/%q, want kb-1/v0.2.0", got.KBVersion, got.AgentVersion)
	}
	if !got.ReceivedAt.Equal(at(1)) || got.ReceivedAt.Location() != time.UTC {
		t.Errorf("ReceivedAt = %v (%v), want %v UTC", got.ReceivedAt, got.ReceivedAt.Location(), at(1))
	}
	if !bytes.Equal(got.Inventory, []byte(`{"hash":"bbb"}`)) {
		t.Errorf("Inventory = %s, want raw bytes back", got.Inventory)
	}
}

func testEvaluations(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", base)

	ins := func(target string, score int, ready bool, created time.Time, report []byte) {
		t.Helper()
		if _, err := s.InsertEvaluation(ctx, store.Evaluation{
			ClusterID: cid, SnapshotID: sid, Target: target, KBVersion: "kb-1",
			Score: score, Ready: ready, Blockers: 2, Warnings: 3,
			Report: report, CreatedAt: created,
		}); err != nil {
			t.Fatalf("InsertEvaluation(%s,%d): %v", target, score, err)
		}
	}
	ins("1.36", 70, false, base, []byte(`{"score":70}`))
	ins("1.36", 80, true, at(1), []byte(`{"score":80}`))
	ins("1.37", 55, false, base, []byte(`{"score":55}`))

	got, err := s.LatestEvaluation(ctx, cid, "1.36")
	if err != nil {
		t.Fatalf("LatestEvaluation(1.36): %v", err)
	}
	if got.Score != 80 || !got.Ready || !got.CreatedAt.Equal(at(1)) {
		t.Errorf("latest 1.36 = score %d ready %v at %v, want 80/true/%v", got.Score, got.Ready, got.CreatedAt, at(1))
	}
	if got.ClusterID != cid || got.SnapshotID != sid || got.Target != "1.36" || got.KBVersion != "kb-1" {
		t.Errorf("identity fields wrong: %+v", got)
	}
	if got.Blockers != 2 || got.Warnings != 3 {
		t.Errorf("Blockers/Warnings = %d/%d, want 2/3", got.Blockers, got.Warnings)
	}
	if !bytes.Equal(got.Report, []byte(`{"score":80}`)) {
		t.Errorf("Report = %s, want {\"score\":80}", got.Report)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt location = %v, want UTC", got.CreatedAt.Location())
	}

	got37, err := s.LatestEvaluation(ctx, cid, "1.37")
	if err != nil {
		t.Fatalf("LatestEvaluation(1.37): %v", err)
	}
	if got37.Score != 55 {
		t.Errorf("latest 1.37 score = %d, want 55", got37.Score)
	}
	if _, err := s.LatestEvaluation(ctx, cid, "1.38"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("LatestEvaluation(1.38) err = %v, want ErrNotFound", err)
	}
}

func testScoreHistory(t *testing.T, s store.Store) {
	ctx := context.Background()
	cid := mustCluster(t, s, "prod")
	sid := mustSnapshot(t, s, cid, "aaa", base)

	points := []struct {
		score int
		ready bool
		when  time.Time
	}{
		{70, false, base}, {75, false, at(1)}, {80, false, at(2)}, {92, true, at(3)},
	}
	for _, p := range points {
		if _, err := s.InsertEvaluation(ctx, store.Evaluation{
			ClusterID: cid, SnapshotID: sid, Target: "1.36",
			Score: p.score, Ready: p.ready, CreatedAt: p.when,
		}); err != nil {
			t.Fatalf("InsertEvaluation: %v", err)
		}
	}
	if _, err := s.InsertEvaluation(ctx, store.Evaluation{
		ClusterID: cid, SnapshotID: sid, Target: "1.37", Score: 10, CreatedAt: base,
	}); err != nil {
		t.Fatalf("InsertEvaluation(1.37): %v", err)
	}

	tests := []struct {
		name       string
		limit      int
		wantScores []int
	}{
		{"zero limit means all", 0, []int{70, 75, 80, 92}},
		{"limit selects most recent N, returned oldest-first", 2, []int{80, 92}},
		{"limit one", 1, []int{92}},
		{"limit beyond rows", 10, []int{70, 75, 80, 92}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.ScoreHistory(ctx, cid, "1.36", tt.limit)
			if err != nil {
				t.Fatalf("ScoreHistory: %v", err)
			}
			if len(got) != len(tt.wantScores) {
				t.Fatalf("got %d points, want %d: %+v", len(got), len(tt.wantScores), got)
			}
			for i, p := range got {
				if p.Score != tt.wantScores[i] {
					t.Errorf("point %d score = %d, want %d", i, p.Score, tt.wantScores[i])
				}
				if i > 0 && !got[i-1].At.Before(p.At) {
					t.Errorf("points not ascending by At: %v then %v", got[i-1].At, p.At)
				}
			}
		})
	}

	empty, err := s.ScoreHistory(ctx, 999, "1.36", 5)
	if err != nil {
		t.Errorf("ScoreHistory(unknown cluster) err = %v, want nil", err)
	}
	if len(empty) != 0 {
		t.Errorf("ScoreHistory(unknown cluster) = %+v, want empty", empty)
	}
}

func testNotFound(t *testing.T, s store.Store) {
	ctx := context.Background()
	tests := []struct {
		name string
		call func() error
	}{
		{"GetCluster", func() error { _, err := s.GetCluster(ctx, 999); return err }},
		{"LatestSnapshot", func() error { _, err := s.LatestSnapshot(ctx, 999); return err }},
		{"LatestEvaluation", func() error { _, err := s.LatestEvaluation(ctx, 999, "1.36"); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("err = %v, want errors.Is(err, store.ErrNotFound)", err)
			}
		})
	}
}

func testClose(t *testing.T, s store.Store) {
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.UpsertCluster(context.Background(), store.Cluster{Name: "after-close"}); err == nil {
		t.Error("UpsertCluster after Close succeeded, want error")
	}
}
```

- [ ] **Step 4: Run test, expect pass**

Run: `go test ./internal/server/store/... -v -run TestSQLiteConformance`

Expected: `TestSQLiteConformance` with subtests `UpsertClusterInsertThenUpdate`, `UpsertClusterZeroTimesDefault`, `SnapshotDedup`, `LatestSnapshotRoundTrip`, `EvaluationsLatestPerTarget`, `ScoreHistoryOldestFirstLimitNewest` (4 inner subtests), `NotFound` (3 inner subtests), `Close` — all PASS. The storetest package itself reports `ok ... [no test files]`. Full `go test ./internal/server/store/...` stays green; `go vet ./internal/server/store/...` clean.

- [ ] **Step 5: Commit**

```bash
git add internal/server/store/ && git commit -m "test: add storetest conformance suite, run it against SQLite" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### What other sections may rely on (exported surface after S1–S4)

- `store.Store`, `store.Cluster`, `store.Snapshot`, `store.Evaluation`, `store.ScorePoint`, `store.ErrNotFound` — used by `internal/server/api.go`, `server.go`, `whatif.go`.
- `store.Open(path) (*SQLite, error)` — used by `internal/cli/serve.go`.
- `store.Migrate(ctx, *sql.DB, fs.FS)` — reused by P3 Postgres.
- `storetest.RunStoreConformance`, `storetest.NewStoreFunc` — reused by P3 Postgres tests.
- Section invariants above (time format, dedup rule, ScoreHistory ordering/limit, ListClusters order, ErrNotFound wrapping) are the contract; do not re-derive them elsewhere.

## Section: internal/server (api, server, whatif)

Builds the HTTP server: wiring + graceful lifecycle (`server.go`), snapshot ingest and the read API (`api.go`), and on-demand re-evaluation (`whatif.go`). All handler tests use `httptest` against a hand-written in-memory fake of `store.Store` — no sqlite dependency in this package's unit tests.

**Task order note:** the prompt-level coverage list names "read handlers" before "what-if", but `GET /clusters/{id}/report` falls back to what-if, so `whatif.go` lands first: V1 server wiring → V2 ingest → V3 whatif → V4 read handlers.

### Cross-section handoffs (read before executing)

1. **`internal/server/store/store.go` is owned by the STORE section.** If this section runs first, V1 Step 1 creates it verbatim from the shared contract, plus one coordinated addition: `var ErrNotFound = errors.New("store: not found")`. The sqlite implementation (STORE section) MUST return `store.ErrNotFound` (possibly wrapped) from `GetCluster`, `LatestSnapshot`, and `LatestEvaluation` when no row matches, and MUST return `ScoreHistory` points **oldest-first** (ascending `created_at`; limit = most recent N, per the STORE contract). The fake store here mirrors both behaviors.
2. **`internal/server/notify/notify.go` is owned by the NOTIFY-CLI section.** If absent, V1 Step 1 creates the interface file (Event + Notifier, verbatim from the contract). NOTIFY-CLI extends the package with delta computation and the Slack/webhook notifiers — it must not change the interface.
3. **`(*Server).notifyDelta` is a documented no-op in this section** (V2). The NOTIFY-CLI section's "wire notifyDelta" task REPLACES its body (signature stays fixed: `notifyDelta(ctx, cluster, target, prev *store.Evaluation, cur store.Evaluation)`, `prev == nil` ⇔ first-ever evaluation of that cluster+target — that is how "no notification on first evaluation" is conveyed). Ingest tests here do not assert notification behavior.

### Decisions (within the normative contracts)

- **Content-Encoding:** ingest accepts `gzip` and identity (empty or `identity` header). Both are tested. Any other encoding → `415 {"error":...}`.
- **Body limit:** 20 MiB (`maxSnapshotBody = 20 << 20`), enforced on the wire bytes via `http.MaxBytesReader` AND on the decompressed stream (gzip-bomb guard) → `413`.
- **Canonical hash:** the inventory is unmarshaled into `inventory.Inventory` and re-marshaled with `encoding/json` before hashing — Go marshals struct fields in declared order and sorts map keys, so wire key order/whitespace never affects dedup.
- **Evaluation is synchronous** inside the ingest handler, before the `202` is written: deterministic tests, no background goroutine lifecycle, and a snapshot is never "accepted but unevaluated" on crash. Per-target evaluation/storage errors are logged and skipped — they never fail the ingest response (snapshot is already stored).
- **What-if is never stored**; `GET .../report?target=` serves the stored evaluation's report when one exists, else computes from the latest snapshot.
- **Read auth:** `/healthz` is always open. The `/api/v1/clusters*` read endpoints require `Authorization: Bearer <ReadToken>` only when `Config.ReadToken != ""`. Token compares use `crypto/subtle`.
- **`?target=` omitted** on report/findings/history → the cluster's default target (next minor above the latest snapshot's server version); unparseable explicit target → `422`.
- **405s** come from Go 1.22 `ServeMux` method patterns (plain-text body with `Allow` header — the JSON-error rule applies to handler-emitted errors).

---

### Task V1: Server wiring — Config, New, routes, graceful Start/Shutdown

**Files:**
- Create (only if STORE section hasn't): `internal/server/store/store.go`
- Create (only if NOTIFY-CLI section hasn't): `internal/server/notify/notify.go`
- Create: `internal/server/server.go`
- Create: `internal/server/fake_store_test.go`
- Create: `internal/server/server_test.go`

- [ ] **Step 1: Contract prerequisite files (skip any that already exist; if they exist, verify they match the shared contract and do NOT edit them)**

`internal/server/store/store.go`:
```go
// Package store defines the persistence seam between the upgradescope server
// and its database backends. P2 ships a SQLite implementation; P3 adds
// Postgres behind the same interface.
package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned (possibly wrapped) by GetCluster, LatestSnapshot,
// and LatestEvaluation when no matching row exists. Every Store
// implementation must honor this sentinel.
var ErrNotFound = errors.New("store: not found")

type Store interface {
	UpsertCluster(ctx context.Context, c Cluster) (int64, error)         // by name; returns id
	InsertSnapshot(ctx context.Context, s Snapshot) (int64, bool, error) // (id, duplicate, err) — duplicate iff same cluster+hash as latest
	LatestSnapshot(ctx context.Context, clusterID int64) (Snapshot, error)
	ListClusters(ctx context.Context) ([]Cluster, error)
	GetCluster(ctx context.Context, id int64) (Cluster, error)
	InsertEvaluation(ctx context.Context, e Evaluation) (int64, error)
	LatestEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error)
	// ScoreHistory returns points oldest-first (ascending created_at); limit selects the most recent N.
	ScoreHistory(ctx context.Context, clusterID int64, target string, limit int) ([]ScorePoint, error)
	Close() error
}

type Cluster struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`       // unique
	ClusterUID string    `json:"clusterUid"` // inventory.ClusterID
	FirstSeen  time.Time `json:"firstSeen"`
	LastSeen   time.Time `json:"lastSeen"`
}

type Snapshot struct {
	ID           int64     `json:"id"`
	ClusterID    int64     `json:"clusterId"`
	Hash         string    `json:"hash"` // sha256 of canonical inventory JSON
	KBVersion    string    `json:"kbVersion"`
	AgentVersion string    `json:"agentVersion"`
	ReceivedAt   time.Time `json:"receivedAt"`
	Inventory    []byte    `json:"-"` // raw canonical JSON
}

type Evaluation struct {
	ID         int64     `json:"id"`
	ClusterID  int64     `json:"clusterId"`
	SnapshotID int64     `json:"snapshotId"`
	Target     string    `json:"target"`
	KBVersion  string    `json:"kbVersion"`
	Score      int       `json:"score"`
	Ready      bool      `json:"ready"`
	Blockers   int       `json:"blockers"`
	Warnings   int       `json:"warnings"`
	Report     []byte    `json:"-"` // full engine.Report JSON
	CreatedAt  time.Time `json:"createdAt"`
}

type ScorePoint struct {
	At    time.Time `json:"at"`
	Score int       `json:"score"`
	Ready bool      `json:"ready"`
}
```

`internal/server/notify/notify.go`:
```go
// Package notify defines the notification seam fired on finding deltas
// (new blocker, became-ready, add-on entering the EOL window) — never on
// every snapshot. Implementations (Slack, generic webhook) live here too.
package notify

import "context"

type Event struct {
	Cluster string
	Target  string
	Kind    string // "new-blocker" | "eol-approaching" | "became-ready"
	Title   string // human line, e.g. finding title
	Detail  string
}

type Notifier interface {
	Notify(ctx context.Context, ev Event) error
}
```

Run: `go build ./internal/server/...`. Expected: builds clean (the two leaf packages compile).

- [ ] **Step 2: Write the in-memory fake store (test-only)**

`internal/server/fake_store_test.go`:
```go
package server

import (
	"context"
	"sort"
	"sync"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// fakeStore is a hand-written in-memory store.Store so handler tests never
// touch sqlite. IDs are assigned from one shared sequence (cluster 1,
// snapshot 2, eval 3, ... in typical single-cluster tests). It mirrors the
// contract's semantics: ErrNotFound sentinels, duplicate iff same
// cluster+hash as the latest snapshot, ScoreHistory oldest-first.
type fakeStore struct {
	mu     sync.Mutex
	nextID int64

	clusters  map[int64]store.Cluster
	snapshots []store.Snapshot
	evals     []store.Evaluation

	// errs injects failures by method name, e.g. errs["InsertSnapshot"].
	errs map[string]error
}

var _ store.Store = (*fakeStore)(nil)

func newFakeStore() *fakeStore {
	return &fakeStore{clusters: map[int64]store.Cluster{}, errs: map[string]error{}}
}

func (f *fakeStore) id() int64 { f.nextID++; return f.nextID }

func (f *fakeStore) UpsertCluster(_ context.Context, c store.Cluster) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["UpsertCluster"]; err != nil {
		return 0, err
	}
	for id, existing := range f.clusters {
		if existing.Name == c.Name {
			existing.ClusterUID = c.ClusterUID
			existing.LastSeen = c.LastSeen
			f.clusters[id] = existing
			return id, nil
		}
	}
	id := f.id()
	c.ID = id
	c.FirstSeen = c.LastSeen
	f.clusters[id] = c
	return id, nil
}

func (f *fakeStore) latestSnapshotLocked(clusterID int64) (store.Snapshot, bool) {
	for i := len(f.snapshots) - 1; i >= 0; i-- {
		if f.snapshots[i].ClusterID == clusterID {
			return f.snapshots[i], true
		}
	}
	return store.Snapshot{}, false
}

func (f *fakeStore) InsertSnapshot(_ context.Context, sn store.Snapshot) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["InsertSnapshot"]; err != nil {
		return 0, false, err
	}
	if latest, ok := f.latestSnapshotLocked(sn.ClusterID); ok && latest.Hash == sn.Hash {
		return latest.ID, true, nil
	}
	sn.ID = f.id()
	f.snapshots = append(f.snapshots, sn)
	return sn.ID, false, nil
}

func (f *fakeStore) LatestSnapshot(_ context.Context, clusterID int64) (store.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["LatestSnapshot"]; err != nil {
		return store.Snapshot{}, err
	}
	if sn, ok := f.latestSnapshotLocked(clusterID); ok {
		return sn, nil
	}
	return store.Snapshot{}, store.ErrNotFound
}

func (f *fakeStore) ListClusters(_ context.Context) ([]store.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["ListClusters"]; err != nil {
		return nil, err
	}
	out := make([]store.Cluster, 0, len(f.clusters))
	for _, c := range f.clusters {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeStore) GetCluster(_ context.Context, id int64) (store.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["GetCluster"]; err != nil {
		return store.Cluster{}, err
	}
	c, ok := f.clusters[id]
	if !ok {
		return store.Cluster{}, store.ErrNotFound
	}
	return c, nil
}

func (f *fakeStore) InsertEvaluation(_ context.Context, e store.Evaluation) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["InsertEvaluation"]; err != nil {
		return 0, err
	}
	e.ID = f.id()
	f.evals = append(f.evals, e)
	return e.ID, nil
}

func (f *fakeStore) LatestEvaluation(_ context.Context, clusterID int64, target string) (store.Evaluation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["LatestEvaluation"]; err != nil {
		return store.Evaluation{}, err
	}
	for i := len(f.evals) - 1; i >= 0; i-- {
		if f.evals[i].ClusterID == clusterID && f.evals[i].Target == target {
			return f.evals[i], nil
		}
	}
	return store.Evaluation{}, store.ErrNotFound
}

func (f *fakeStore) ScoreHistory(_ context.Context, clusterID int64, target string, limit int) ([]store.ScorePoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs["ScoreHistory"]; err != nil {
		return nil, err
	}
	var all []store.ScorePoint
	for _, e := range f.evals {
		if e.ClusterID == clusterID && e.Target == target {
			all = append(all, store.ScorePoint{At: e.CreatedAt, Score: e.Score, Ready: e.Ready})
		}
	}
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:] // most recent N, still oldest-first
	}
	return all, nil // oldest first — matches the store contract
}

func (f *fakeStore) Close() error { return nil }
```

- [ ] **Step 3: Write the failing server wiring tests**

`internal/server/server_test.go`:
```go
package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// testKB is a tiny deterministic KB: one lifecycle entry (PodSecurityPolicy,
// deprecated 1.30, removed 1.35) and a MaxKnownK8s high enough that kb-stale
// never fires in tests.
func testKB() kb.KB {
	deprecated := inventory.Version{Major: 1, Minor: 30}
	removed := inventory.Version{Major: 1, Minor: 35}
	return kb.KB{
		Version: "test-kb",
		APILifecycle: []kb.APILifecycleEntry{{
			Group:      "policy",
			Version:    "v1beta1",
			Kind:       "PodSecurityPolicy",
			Introduced: inventory.Version{Major: 1, Minor: 10},
			Deprecated: &deprecated,
			Removed:    &removed,
		}},
		Skew:        kb.DefaultSkewPolicy(),
		MaxKnownK8s: inventory.Version{Major: 1, Minor: 99},
	}
}

func TestNewValidation(t *testing.T) {
	st := newFakeStore()
	if _, err := New(Config{Store: nil, KB: testKB(), IngestToken: "tok"}); err == nil {
		t.Fatal("New with nil Store: want error, got nil")
	}
	if _, err := New(Config{Store: st, KB: testKB(), IngestToken: ""}); err == nil {
		t.Fatal("New with empty IngestToken: want error, got nil")
	}
	if _, err := New(Config{Store: st, KB: testKB(), IngestToken: "tok", ExtraTargets: []string{"bogus"}}); err == nil {
		t.Fatal("New with unparseable extra target: want error, got nil")
	}
	s, err := New(Config{Store: st, KB: testKB(), IngestToken: "tok", ExtraTargets: []string{"1.37", "v1.38"}})
	if err != nil {
		t.Fatalf("New with valid config: %v", err)
	}
	if s.Handler() == nil {
		t.Fatal("Handler() returned nil")
	}
}

func TestStartShutdown(t *testing.T) {
	s, err := New(Config{
		Listen:      "127.0.0.1:0",
		Store:       newFakeStore(),
		KB:          testKB(),
		IngestToken: "tok",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Start() }()
	select {
	case <-s.Ready():
	case err := <-errc:
		t.Fatalf("server exited before becoming ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server never became ready")
	}
	resp, err := http.Get(fmt.Sprintf("http://%s/nope", s.Addr()))
	if err != nil {
		t.Fatalf("GET while serving: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /nope status = %d, want 404 (mux serving, route unregistered)", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("Start returned %v after graceful Shutdown, want nil", err)
	}
}
```

- [ ] **Step 4: Run, expect fail** — Run: `go test ./internal/server/`. Expected: `FAIL` with build errors (`undefined: Config`, `undefined: New`).

- [ ] **Step 5: Implement server.go**

`internal/server/server.go`:
```go
// Package server is the upgradescope continuous-mode server: snapshot
// ingest, persisted evaluations, a read API, and on-demand what-if
// re-evaluation. Storage is behind store.Store; the CLI owns flag parsing
// and store construction (DB path never reaches this package).
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// Config wires a Server.
type Config struct {
	Listen       string          // listen address for Start, e.g. ":8080"
	Store        store.Store     // required
	KB           kb.KB           // evaluation knowledge base
	ExtraTargets []string        // minors evaluated for every snapshot, e.g. ["1.37"]
	Notifier     notify.Notifier // nil = notifications disabled
	IngestToken  string          // required bearer for POST /api/v1/snapshots
	ReadToken    string          // optional bearer for the read API; "" = open (document loudly)
}

// Server serves the ingest + read API. Construct with New; a Server is
// single-use (one Start/Shutdown cycle).
type Server struct {
	cfg          Config
	extraTargets []inventory.Version
	mux          *http.ServeMux
	httpSrv      *http.Server
	now          func() time.Time // injected clock: EOL math + timestamps stay testable

	ready chan struct{} // closed once the listener is bound
	mu    sync.Mutex
	addr  string
}

// New validates cfg and builds the route table.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("server: Config.Store is required")
	}
	if cfg.IngestToken == "" {
		return nil, errors.New("server: Config.IngestToken is required")
	}
	s := &Server{
		cfg:   cfg,
		now:   time.Now,
		mux:   http.NewServeMux(),
		ready: make(chan struct{}),
	}
	for _, t := range cfg.ExtraTargets {
		v, err := inventory.ParseVersion(t)
		if err != nil {
			return nil, fmt.Errorf("server: bad extra target %q: %w", t, err)
		}
		s.extraTargets = append(s.extraTargets, v)
	}
	s.routes()
	s.httpSrv = &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// routes registers all endpoints (Go 1.22 method+path patterns).
// Handlers land with their tasks:
//
//	V2 — POST /api/v1/snapshots (ingest)
//	V4 — GET /healthz + the read API
func (s *Server) routes() {
}

// Handler exposes the full route table for httptest and embedding.
func (s *Server) Handler() http.Handler { return s.mux }

// Start binds Config.Listen and serves until Shutdown. It returns nil after
// a clean Shutdown, otherwise the listen/serve error. Once Ready() is
// closed, Addr() reports the bound address (Listen ":0" works in tests).
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", s.cfg.Listen, err)
	}
	s.mu.Lock()
	s.addr = ln.Addr().String()
	s.mu.Unlock()
	close(s.ready)
	if err := s.httpSrv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Ready is closed once the listener is bound.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// Addr returns the bound listen address ("" before Ready is closed).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Shutdown gracefully drains in-flight requests, then unblocks Start.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}
```

- [ ] **Step 6: Run, expect pass** — Run: `go test ./internal/server/ -v && go vet ./internal/server/...`. Expected: `TestNewValidation` and `TestStartShutdown` both `--- PASS`, vet clean.

- [ ] **Step 7: Commit**

```bash
git add internal/server/ && git commit -m "feat(server): Config/New wiring, route table, graceful Start/Shutdown" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task V2: Ingest handler — auth, gzip/identity, canonical hash dedup, evaluation fan-out

**Files:**
- Create: `internal/server/api.go`
- Create: `internal/server/api_ingest_test.go`
- Modify: `internal/server/server.go` (register route)

- [ ] **Step 1: Write the failing protocol tests (helpers + auth + accept + duplicate + validation + encodings)**

`internal/server/api_ingest_test.go`:
```go
package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// newTestServer builds a Server on a fake store with a pinned clock.
func newTestServer(t *testing.T, st *fakeStore, opts ...func(*Config)) *Server {
	t.Helper()
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

func testInventory() inventory.Inventory {
	return inventory.Inventory{
		SchemaVersion: 1,
		ClusterID:     "uid-123",
		CollectedAt:   time.Date(2026, 6, 10, 11, 0, 0, 0, time.UTC),
		ServerVersion: "v1.34.2",
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{
			inventory.CapVersions: {Available: true},
		},
	}
}

// testInventoryWithPSP adds one PodSecurityPolicy residency: with testKB
// (PSP removed in 1.35) this yields exactly 1 blocker for targets >= 1.35
// (score 75, not ready) and 1 warning for target 1.34.
func testInventoryWithPSP() inventory.Inventory {
	inv := testInventory()
	inv.APIUsage = []inventory.APIUsage{{
		Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy",
		Count: 2, Namespaces: map[string]int{"": 2},
	}}
	return inv
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(b); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func pushReqBody(t *testing.T, inv inventory.Inventory) []byte {
	t.Helper()
	invJSON, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	b, err := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"clusterName":   "prod-eu-1",
		"agentVersion":  "v0.2.0-test",
		"kbVersion":     "agent-kb",
		"inventory":     json.RawMessage(invJSON),
	})
	if err != nil {
		t.Fatalf("marshal push request: %v", err)
	}
	return b
}

// postSnapshot POSTs body (gzipped iff gzipped) and decodes the JSON reply.
func postSnapshot(t *testing.T, ts *httptest.Server, token string, body []byte, gzipped bool) (*http.Response, map[string]any) {
	t.Helper()
	payload := body
	if gzipped {
		payload = gzipBytes(t, body)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/snapshots", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/snapshots: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("non-JSON response (status %d): %s", resp.StatusCode, raw)
		}
	}
	return resp, out
}

func TestIngestAuth(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	body := pushReqBody(t, testInventory())
	for _, tc := range []struct{ name, token string }{
		{"missing token", ""},
		{"wrong token", "nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, out := postSnapshot(t, ts, tc.token, body, true)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if msg, _ := out["error"].(string); msg == "" {
				t.Fatalf("want JSON error body, got %v", out)
			}
		})
	}
}

func TestIngestAcceptedGzipAndIdentity(t *testing.T) {
	for _, gzipped := range []bool{true, false} {
		name := "identity"
		if gzipped {
			name = "gzip"
		}
		t.Run(name, func(t *testing.T) {
			st := newFakeStore()
			ts := httptest.NewServer(newTestServer(t, st).Handler())
			defer ts.Close()
			resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventory()), gzipped)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("status = %d (body %v), want 202", resp.StatusCode, out)
			}
			if _, ok := out["snapshotId"].(float64); !ok {
				t.Fatalf("response %v missing numeric snapshotId", out)
			}
			if d, ok := out["duplicate"]; ok && d == true {
				t.Fatalf("first push flagged duplicate: %v", out)
			}
			if len(st.snapshots) != 1 {
				t.Fatalf("stored snapshots = %d, want 1", len(st.snapshots))
			}
			sn := st.snapshots[0]
			if sn.Hash == "" || sn.KBVersion != "agent-kb" || sn.AgentVersion != "v0.2.0-test" {
				t.Fatalf("snapshot fields = %+v", sn)
			}
			var stored inventory.Inventory
			if err := json.Unmarshal(sn.Inventory, &stored); err != nil {
				t.Fatalf("stored inventory is not canonical JSON: %v", err)
			}
			if stored.ClusterID != "uid-123" {
				t.Fatalf("stored ClusterID = %q", stored.ClusterID)
			}
			if len(st.clusters) != 1 {
				t.Fatalf("clusters = %d, want 1", len(st.clusters))
			}
			c := st.clusters[1]
			if c.Name != "prod-eu-1" || c.ClusterUID != "uid-123" {
				t.Fatalf("cluster = %+v", c)
			}
		})
	}
}

func TestIngestDuplicateCanonicalHash(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()

	resp1, out1 := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventory()), true)
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first push status = %d, want 202", resp1.StatusCode)
	}
	evalsAfterFirst := len(st.evals)

	// Same logical inventory, different wire key order and encoding: the
	// canonical re-marshal must produce the same hash → duplicate.
	reordered := []byte(`{
	  "schemaVersion": 1,
	  "clusterName": "prod-eu-1",
	  "agentVersion": "v0.2.0-test",
	  "kbVersion": "agent-kb",
	  "inventory": {
	    "capabilities": {"versions": {"available": true}},
	    "serverVersion": "v1.34.2",
	    "collectedAt": "2026-06-10T11:00:00Z",
	    "clusterId": "uid-123",
	    "schemaVersion": 1
	  }
	}`)
	resp2, out2 := postSnapshot(t, ts, "ingest-tok", reordered, false)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("duplicate push status = %d (body %v), want 200", resp2.StatusCode, out2)
	}
	if out2["duplicate"] != true {
		t.Fatalf("duplicate push body = %v, want duplicate:true", out2)
	}
	if out1["snapshotId"] != out2["snapshotId"] {
		t.Fatalf("snapshotId mismatch: %v vs %v", out1["snapshotId"], out2["snapshotId"])
	}
	if len(st.snapshots) != 1 {
		t.Fatalf("snapshots after duplicate = %d, want 1", len(st.snapshots))
	}
	if len(st.evals) != evalsAfterFirst {
		t.Fatalf("duplicate triggered re-evaluation: evals %d -> %d", evalsAfterFirst, len(st.evals))
	}
}

func TestIngestValidation(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	cases := []struct {
		name string
		body string
	}{
		{"schemaVersion 2", `{"schemaVersion":2,"clusterName":"c","inventory":{}}`},
		{"schemaVersion missing", `{"clusterName":"c","inventory":{}}`},
		{"clusterName missing", `{"schemaVersion":1,"inventory":{}}`},
		{"inventory missing", `{"schemaVersion":1,"clusterName":"c"}`},
		{"inventory wrong type", `{"schemaVersion":1,"clusterName":"c","inventory":42}`},
		{"malformed JSON", `{"schemaVersion":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, out := postSnapshot(t, ts, "ingest-tok", []byte(tc.body), false)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d (body %v), want 422", resp.StatusCode, out)
			}
			if msg, _ := out["error"].(string); msg == "" {
				t.Fatalf("want structured {\"error\":...}, got %v", out)
			}
		})
	}
}

func TestIngestEncodingErrors(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()

	t.Run("unsupported encoding", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/snapshots",
			bytes.NewReader(pushReqBody(t, testInventory())))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer ingest-tok")
		req.Header.Set("Content-Encoding", "br")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415", resp.StatusCode)
		}
	})

	t.Run("bad gzip", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/snapshots",
			bytes.NewReader([]byte("definitely not gzip")))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer ingest-tok")
		req.Header.Set("Content-Encoding", "gzip")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", resp.StatusCode)
		}
	})
}

func TestIngestBodyLimits(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()

	t.Run("identity over 20MiB", func(t *testing.T) {
		huge := bytes.Repeat([]byte("a"), maxSnapshotBody+1)
		resp, _ := postSnapshot(t, ts, "ingest-tok", huge, false)
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
	})

	t.Run("gzip bomb", func(t *testing.T) {
		// Tiny on the wire, >20MiB decompressed: the post-decompression cap
		// must fire.
		bomb := gzipBytes(t, make([]byte, maxSnapshotBody+2))
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/snapshots", bytes.NewReader(bomb))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer ingest-tok")
		req.Header.Set("Content-Encoding", "gzip")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
	})
}
```

- [ ] **Step 2: Write the failing evaluation fan-out tests (append to `internal/server/api_ingest_test.go`)**

```go
func TestIngestEvaluatesDefaultAndExtraTargets(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.ExtraTargets = []string{"1.37"} })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventoryWithPSP()), true)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d (body %v), want 202", resp.StatusCode, out)
	}
	// Default target = next minor above v1.34.2 → 1.35, plus extra 1.37.
	if len(st.evals) != 2 {
		t.Fatalf("evaluations = %d, want 2 (default 1.35 + extra 1.37)", len(st.evals))
	}
	byTarget := map[string]store.Evaluation{}
	for _, e := range st.evals {
		byTarget[e.Target] = e
	}
	e135, ok := byTarget["1.35"]
	if !ok {
		t.Fatalf("no evaluation for default target 1.35; got %v", byTarget)
	}
	if e135.Score != 75 || e135.Ready || e135.Blockers != 1 || e135.Warnings != 0 {
		t.Fatalf("1.35 eval = score %d ready %v blockers %d warnings %d, want 75 false 1 0",
			e135.Score, e135.Ready, e135.Blockers, e135.Warnings)
	}
	if e135.KBVersion != "test-kb" {
		t.Fatalf("eval KBVersion = %q, want server KB version test-kb", e135.KBVersion)
	}
	if e135.SnapshotID == 0 || e135.ClusterID == 0 {
		t.Fatalf("eval missing FK linkage: %+v", e135)
	}
	var rep engine.Report
	if err := json.Unmarshal(e135.Report, &rep); err != nil {
		t.Fatalf("stored report is not engine.Report JSON: %v", err)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Severity != engine.SevBlocker ||
		rep.Findings[0].Category != engine.CatRemovedAPI {
		t.Fatalf("report findings = %+v, want 1 removed-api blocker", rep.Findings)
	}
	if _, ok := byTarget["1.37"]; !ok {
		t.Fatal("no evaluation for extra target 1.37")
	}
}

func TestIngestDedupsExtraTargetEqualToDefault(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.ExtraTargets = []string{"1.35"} })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	if resp, _ := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventory()), true); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if len(st.evals) != 1 {
		t.Fatalf("evaluations = %d, want 1 (extra target equals default)", len(st.evals))
	}
}

func TestIngestSkipsDefaultTargetWhenServerVersionUnparseable(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	inv := testInventory()
	inv.ServerVersion = "" // versions capability degraded
	resp, _ := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), true)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (snapshot still accepted)", resp.StatusCode)
	}
	if len(st.evals) != 0 {
		t.Fatalf("evaluations = %d, want 0 (no parseable default, no extras)", len(st.evals))
	}
	if len(st.snapshots) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(st.snapshots))
	}
}
```

- [ ] **Step 3: Run, expect fail** — Run: `go test ./internal/server/`. Expected: `FAIL` — compile error `undefined: maxSnapshotBody`; after that the route is unregistered, so ingest tests would see 404s.

- [ ] **Step 4: Implement api.go (ingest) and register the route**

`internal/server/api.go`:
```go
package server

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// maxSnapshotBody caps the snapshot push body — enforced on the wire bytes
// AND on the decompressed stream (gzip-bomb guard).
const maxSnapshotBody = 20 << 20 // 20 MiB

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// bearerOK does a constant-time check of "Authorization: Bearer <token>".
func bearerOK(r *http.Request, token string) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.HasPrefix(h, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(token)) == 1
}

// pushRequest is the snapshot push protocol body (schemaVersion 1).
type pushRequest struct {
	SchemaVersion int             `json:"schemaVersion"`
	ClusterName   string          `json:"clusterName"`
	AgentVersion  string          `json:"agentVersion"`
	KBVersion     string          `json:"kbVersion"`
	Inventory     json.RawMessage `json:"inventory"`
}

// handleIngest implements POST /api/v1/snapshots: bearer auth, gzip or
// identity body, schema validation, canonical-JSON content-hash dedup,
// upsert+insert, then synchronous evaluation fan-out for accepted snapshots.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if !bearerOK(r, s.cfg.IngestToken) {
		errJSON(w, http.StatusUnauthorized, "invalid or missing bearer token")
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxSnapshotBody)
	var reader io.Reader = body
	switch enc := r.Header.Get("Content-Encoding"); enc {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(body)
		if err != nil {
			errJSON(w, http.StatusUnprocessableEntity, "body is not valid gzip")
			return
		}
		defer gz.Close()
		// Cap the decompressed stream too: a tiny gzip bomb must not bypass
		// the wire-byte limit. Read one byte past the cap so overflow is
		// detectable below.
		reader = io.LimitReader(gz, maxSnapshotBody+1)
	default:
		errJSON(w, http.StatusUnsupportedMediaType, fmt.Sprintf("unsupported Content-Encoding %q (use gzip or identity)", enc))
		return
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			errJSON(w, http.StatusRequestEntityTooLarge, "snapshot exceeds the 20MiB limit")
			return
		}
		errJSON(w, http.StatusUnprocessableEntity, "reading body: "+err.Error())
		return
	}
	if len(raw) > maxSnapshotBody {
		errJSON(w, http.StatusRequestEntityTooLarge, "snapshot exceeds the 20MiB limit after decompression")
		return
	}
	var req pushRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		errJSON(w, http.StatusUnprocessableEntity, "invalid JSON: "+err.Error())
		return
	}
	if req.SchemaVersion != 1 {
		errJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf("unsupported schemaVersion %d (want 1)", req.SchemaVersion))
		return
	}
	if req.ClusterName == "" {
		errJSON(w, http.StatusUnprocessableEntity, "clusterName is required")
		return
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(req.Inventory, &inv); err != nil {
		errJSON(w, http.StatusUnprocessableEntity, "invalid inventory: "+err.Error())
		return
	}
	// Canonical form: re-marshal the parsed inventory so wire key order and
	// whitespace never change the dedup hash. Struct fields marshal in
	// declared order; map keys marshal sorted. CollectedAt is zeroed to match
	// the agent's snapshotHash canonical form (it changes every tick; hashing
	// it would make force-sync pushes never dedup to 200 duplicate).
	inv.CollectedAt = time.Time{}
	canonical, err := json.Marshal(inv)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "canonicalizing inventory: "+err.Error())
		return
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(canonical))

	ctx := r.Context()
	now := s.now()
	clusterID, err := s.cfg.Store.UpsertCluster(ctx, store.Cluster{
		Name:       req.ClusterName,
		ClusterUID: inv.ClusterID,
		LastSeen:   now,
	})
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "storing cluster: "+err.Error())
		return
	}
	snapID, duplicate, err := s.cfg.Store.InsertSnapshot(ctx, store.Snapshot{
		ClusterID:    clusterID,
		Hash:         hash,
		KBVersion:    req.KBVersion,
		AgentVersion: req.AgentVersion,
		ReceivedAt:   now,
		Inventory:    canonical,
	})
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "storing snapshot: "+err.Error())
		return
	}
	if duplicate {
		writeJSON(w, http.StatusOK, map[string]any{"snapshotId": snapID, "duplicate": true})
		return
	}
	cluster := store.Cluster{ID: clusterID, Name: req.ClusterName, ClusterUID: inv.ClusterID}
	s.evaluateSnapshot(ctx, cluster, snapID, inv)
	writeJSON(w, http.StatusAccepted, map[string]any{"snapshotId": snapID})
}

// evaluateSnapshot evaluates an accepted snapshot against the default target
// (next minor above the inventory's server version; skipped when
// unparseable) plus every configured extra target (deduped), stores one
// Evaluation per target, and fires the notifier delta. Per-target failures
// are logged and skipped — the snapshot is already stored, and ingest must
// not fail because one target could not be evaluated.
func (s *Server) evaluateSnapshot(ctx context.Context, cluster store.Cluster, snapshotID int64, inv inventory.Inventory) {
	targets := make([]inventory.Version, 0, len(s.extraTargets)+1)
	if server, err := inventory.ParseVersion(inv.ServerVersion); err == nil {
		targets = append(targets, server.Next())
	}
	targets = append(targets, s.extraTargets...)

	seen := map[inventory.Version]bool{}
	now := s.now()
	for _, target := range targets {
		if seen[target] {
			continue
		}
		seen[target] = true
		rep := engine.Evaluate(inv, s.cfg.KB, target, now)
		repJSON, err := json.Marshal(rep)
		if err != nil {
			log.Printf("server: marshaling report (cluster %d, target %s): %v", cluster.ID, target, err)
			continue
		}
		var blockers, warnings int
		for _, f := range rep.Findings {
			switch f.Severity {
			case engine.SevBlocker:
				blockers++
			case engine.SevWarning:
				warnings++
			}
		}
		var prev *store.Evaluation
		if p, err := s.cfg.Store.LatestEvaluation(ctx, cluster.ID, target.String()); err == nil {
			prev = &p
		} else if !errors.Is(err, store.ErrNotFound) {
			log.Printf("server: loading previous evaluation (cluster %d, target %s): %v", cluster.ID, target, err)
		}
		cur := store.Evaluation{
			ClusterID:  cluster.ID,
			SnapshotID: snapshotID,
			Target:     target.String(),
			KBVersion:  s.cfg.KB.Version,
			Score:      rep.Score,
			Ready:      rep.Ready,
			Blockers:   blockers,
			Warnings:   warnings,
			Report:     repJSON,
			CreatedAt:  now,
		}
		id, err := s.cfg.Store.InsertEvaluation(ctx, cur)
		if err != nil {
			log.Printf("server: storing evaluation (cluster %d, target %s): %v", cluster.ID, target, err)
			continue
		}
		cur.ID = id
		s.notifyDelta(ctx, cluster, target.String(), prev, cur)
	}
}

// notifyDelta compares prev (nil ⇔ the cluster's first-ever evaluation for
// this target) against cur and emits Config.Notifier events per the delta
// rule (new-blocker, became-ready, eol-approaching).
//
// This body is a no-op stub: the NOTIFY-CLI section's "wire notifyDelta"
// task replaces it with the real delta computation. The signature is the
// fixed contract between the two sections — do not change it there.
func (s *Server) notifyDelta(ctx context.Context, cluster store.Cluster, target string, prev *store.Evaluation, cur store.Evaluation) {
	_, _, _, _, _ = ctx, cluster, target, prev, cur
}
```

In `internal/server/server.go`, replace the empty `routes` body:
```go
// routes registers all endpoints (Go 1.22 method+path patterns).
// Read API handlers land in V4.
func (s *Server) routes() {
	s.mux.HandleFunc("POST /api/v1/snapshots", s.handleIngest)
}
```

- [ ] **Step 5: Run, expect pass** — Run: `go test ./internal/server/ -v && go vet ./internal/server/...`. Expected: all `TestIngest*` subtests `--- PASS` (plus V1 tests still green), vet clean.

- [ ] **Step 6: Commit**

```bash
git add internal/server/ && git commit -m "feat(server): snapshot ingest with auth, gzip, canonical-hash dedup, evaluation fan-out" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task V3: whatif.go — re-evaluate the latest stored snapshot for an arbitrary target

**Files:**
- Create: `internal/server/whatif.go`
- Create: `internal/server/whatif_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/server/whatif_test.go`:
```go
package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

func seedSnapshot(t *testing.T, st *fakeStore, inv inventory.Inventory) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	id, err := st.UpsertCluster(ctx, store.Cluster{Name: "c1", ClusterUID: inv.ClusterID, LastSeen: now})
	if err != nil {
		t.Fatalf("UpsertCluster: %v", err)
	}
	invJSON, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	if _, _, err := st.InsertSnapshot(ctx, store.Snapshot{
		ClusterID: id, Hash: "h1", ReceivedAt: now, Inventory: invJSON,
	}); err != nil {
		t.Fatalf("InsertSnapshot: %v", err)
	}
	return id
}

func TestWhatIfEvaluatesLatestSnapshot(t *testing.T) {
	st := newFakeStore()
	id := seedSnapshot(t, st, testInventoryWithPSP())
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	// PSP is removed in 1.35: target 1.35 → blocker; target 1.34 → warning
	// (removed exactly at target+1). The target visibly changes the verdict.
	rep, err := WhatIf(context.Background(), st, testKB(), id, inventory.Version{Major: 1, Minor: 35}, now)
	if err != nil {
		t.Fatalf("WhatIf 1.35: %v", err)
	}
	if rep.ClusterID != "uid-123" || rep.KBVersion != "test-kb" {
		t.Fatalf("report identity = %q / %q, want uid-123 / test-kb", rep.ClusterID, rep.KBVersion)
	}
	if rep.Score != 75 || rep.Ready || len(rep.Findings) != 1 || rep.Findings[0].Severity != engine.SevBlocker {
		t.Fatalf("1.35 report = score %d ready %v findings %+v, want 75 false [1 blocker]", rep.Score, rep.Ready, rep.Findings)
	}

	rep34, err := WhatIf(context.Background(), st, testKB(), id, inventory.Version{Major: 1, Minor: 34}, now)
	if err != nil {
		t.Fatalf("WhatIf 1.34: %v", err)
	}
	if rep34.Score != 95 || !rep34.Ready || len(rep34.Findings) != 1 || rep34.Findings[0].Severity != engine.SevWarning {
		t.Fatalf("1.34 report = score %d ready %v findings %+v, want 95 true [1 warning]", rep34.Score, rep34.Ready, rep34.Findings)
	}
}

func TestWhatIfNoSnapshot(t *testing.T) {
	st := newFakeStore()
	_, err := WhatIf(context.Background(), st, testKB(), 42, inventory.Version{Major: 1, Minor: 35}, time.Now())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want store.ErrNotFound (wrapped)", err)
	}
}

func TestWhatIfCorruptStoredInventory(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()
	id, err := st.UpsertCluster(ctx, store.Cluster{Name: "c1", LastSeen: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InsertSnapshot(ctx, store.Snapshot{ClusterID: id, Hash: "h", Inventory: []byte("{not json")}); err != nil {
		t.Fatal(err)
	}
	_, err = WhatIf(ctx, st, testKB(), id, inventory.Version{Major: 1, Minor: 35}, time.Now())
	if err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want non-nil non-NotFound corrupt-inventory error", err)
	}
}
```

- [ ] **Step 2: Run, expect fail** — Run: `go test ./internal/server/`. Expected: `FAIL` with build error `undefined: WhatIf`.

- [ ] **Step 3: Implement whatif.go**

`internal/server/whatif.go`:
```go
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// WhatIf re-evaluates a cluster's latest stored snapshot against an
// arbitrary target using the server's KB. Nothing is stored. now is injected
// by the caller (the server passes s.now()) so EOL-window math stays
// deterministic and testable. Returns store.ErrNotFound (wrapped) when the
// cluster has no snapshots.
func WhatIf(ctx context.Context, st store.Store, k kb.KB, clusterID int64, target inventory.Version, now time.Time) (engine.Report, error) {
	snap, err := st.LatestSnapshot(ctx, clusterID)
	if err != nil {
		return engine.Report{}, fmt.Errorf("what-if for cluster %d: %w", clusterID, err)
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(snap.Inventory, &inv); err != nil {
		return engine.Report{}, fmt.Errorf("what-if for cluster %d: corrupt stored inventory (snapshot %d): %w", clusterID, snap.ID, err)
	}
	return engine.Evaluate(inv, k, target, now), nil
}
```

Note: `ctx` is consumed by the store call; `engine.Evaluate` is pure (no I/O) per its P1 contract.

- [ ] **Step 4: Run, expect pass** — Run: `go test ./internal/server/ -run TestWhatIf -v`. Expected: all three `--- PASS`. Then full package: `go test ./internal/server/`.

- [ ] **Step 5: Commit**

```bash
git add internal/server/ && git commit -m "feat(server): what-if re-evaluation of latest stored snapshot" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task V4: Read API — healthz, clusters, report, findings, history

**Files:**
- Modify: `internal/server/api.go` (read handlers + helpers)
- Modify: `internal/server/server.go` (full route table)
- Create: `internal/server/api_read_test.go`

- [ ] **Step 1: Write the failing tests — healthz, auth, cluster list/detail, error shapes**

`internal/server/api_read_test.go`:
```go
package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// getJSON GETs path with optional bearer token and decodes JSON into `into`
// (skipped when into is nil — e.g. for 405s, whose body is ServeMux plain text).
func getJSON(t *testing.T, ts *httptest.Server, path, token string, into any) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if into != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("GET %s: status %d, non-JSON body %q", path, resp.StatusCode, raw)
		}
	}
	return resp
}

// seedViaPush ingests testInventoryWithPSP through the real ingest endpoint
// so cluster, snapshot, and the 1.35 evaluation all exist. fakeStore assigns
// the first cluster ID 1.
func seedViaPush(t *testing.T, ts *httptest.Server) int64 {
	t.Helper()
	resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventoryWithPSP()), true)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seed push status = %d (body %v), want 202", resp.StatusCode, out)
	}
	return 1
}

func TestHealthz(t *testing.T) {
	// ReadToken configured — /healthz must still be open (probes have no tokens).
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.ReadToken = "read-tok" })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	var out map[string]string
	resp := getJSON(t, ts, "/healthz", "", &out)
	if resp.StatusCode != http.StatusOK || out["status"] != "ok" {
		t.Fatalf("healthz = %d %v, want 200 {status:ok}", resp.StatusCode, out)
	}
}

func TestReadAuth(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.ReadToken = "read-tok" })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	for _, tc := range []struct {
		name  string
		token string
		want  int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"wrong token", "nope", http.StatusUnauthorized},
		{"ingest token is not a read token", "ingest-tok", http.StatusUnauthorized},
		{"correct token", "read-tok", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out json.RawMessage
			resp := getJSON(t, ts, "/api/v1/clusters", tc.token, &out)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}

	t.Run("open when ReadToken empty", func(t *testing.T) {
		tsOpen := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
		defer tsOpen.Close()
		var out json.RawMessage
		if resp := getJSON(t, tsOpen, "/api/v1/clusters", "", &out); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (read API open)", resp.StatusCode)
		}
	})
}

func TestListClusters(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	seedViaPush(t, ts)
	// A second cluster with no snapshots → no latest summary.
	if _, err := st.UpsertCluster(context.Background(), store.Cluster{Name: "empty", LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var got []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Latest *struct {
			Target   string `json:"target"`
			Score    int    `json:"score"`
			Ready    bool   `json:"ready"`
			Blockers int    `json:"blockers"`
		} `json:"latest"`
	}
	resp := getJSON(t, ts, "/api/v1/clusters", "", &got)
	if resp.StatusCode != http.StatusOK || len(got) != 2 {
		t.Fatalf("status %d, %d clusters, want 200 with 2", resp.StatusCode, len(got))
	}
	if got[0].Name != "prod-eu-1" || got[0].Latest == nil {
		t.Fatalf("cluster[0] = %+v, want prod-eu-1 with latest summary", got[0])
	}
	if got[0].Latest.Target != "1.35" || got[0].Latest.Score != 75 || got[0].Latest.Ready || got[0].Latest.Blockers != 1 {
		t.Fatalf("latest = %+v, want target 1.35 score 75 ready false blockers 1", got[0].Latest)
	}
	if got[1].Name != "empty" || got[1].Latest != nil {
		t.Fatalf("cluster[1] = %+v, want empty cluster without latest", got[1])
	}
}

func TestGetCluster(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.ExtraTargets = []string{"1.37"} })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	id := seedViaPush(t, ts)

	var got struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		Capabilities map[string]struct {
			Available bool `json:"available"`
		} `json:"capabilities"`
		Evaluations []struct {
			Target string `json:"target"`
			Score  int    `json:"score"`
		} `json:"evaluations"`
	}
	resp := getJSON(t, ts, "/api/v1/clusters/1", "", &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.ID != id || got.Name != "prod-eu-1" {
		t.Fatalf("cluster = %+v", got)
	}
	if cap, ok := got.Capabilities["versions"]; !ok || !cap.Available {
		t.Fatalf("capabilities = %+v, want versions available", got.Capabilities)
	}
	targets := map[string]bool{}
	for _, e := range got.Evaluations {
		targets[e.Target] = true
	}
	if !targets["1.35"] || !targets["1.37"] || len(got.Evaluations) != 2 {
		t.Fatalf("evaluations = %+v, want default 1.35 + extra 1.37", got.Evaluations)
	}

	t.Run("unknown id is JSON 404", func(t *testing.T) {
		var out map[string]string
		resp := getJSON(t, ts, "/api/v1/clusters/999", "", &out)
		if resp.StatusCode != http.StatusNotFound || out["error"] == "" {
			t.Fatalf("status %d body %v, want 404 with error", resp.StatusCode, out)
		}
	})
	t.Run("non-numeric id is 400", func(t *testing.T) {
		var out map[string]string
		resp := getJSON(t, ts, "/api/v1/clusters/abc", "", &out)
		if resp.StatusCode != http.StatusBadRequest || out["error"] == "" {
			t.Fatalf("status %d body %v, want 400 with error", resp.StatusCode, out)
		}
	})
}
```

- [ ] **Step 2: Write the failing tests — report, findings, history, 405 (append to `internal/server/api_read_test.go`)**

```go
func TestReport(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	seedViaPush(t, ts)

	// Overwrite the stored 1.35 report with a marker so the stored-vs-what-if
	// path is distinguishable (a fresh what-if would carry kbVersion test-kb).
	st.mu.Lock()
	for i := range st.evals {
		if st.evals[i].Target == "1.35" {
			st.evals[i].Report = []byte(`{"clusterId":"uid-123","target":"1.35","kbVersion":"stored-marker","score":42,"ready":false,"findings":[]}`)
		}
	}
	st.mu.Unlock()

	t.Run("stored evaluation wins", func(t *testing.T) {
		var rep engine.Report
		resp := getJSON(t, ts, "/api/v1/clusters/1/report?target=1.35", "", &rep)
		if resp.StatusCode != http.StatusOK || rep.KBVersion != "stored-marker" || rep.Score != 42 {
			t.Fatalf("status %d kbVersion %q score %d, want 200 stored-marker 42", resp.StatusCode, rep.KBVersion, rep.Score)
		}
	})
	t.Run("missing target falls back to default target", func(t *testing.T) {
		var rep engine.Report
		resp := getJSON(t, ts, "/api/v1/clusters/1/report", "", &rep)
		if resp.StatusCode != http.StatusOK || rep.KBVersion != "stored-marker" {
			t.Fatalf("status %d kbVersion %q, want 200 stored-marker (default target 1.35)", resp.StatusCode, rep.KBVersion)
		}
	})
	t.Run("what-if for unstored target", func(t *testing.T) {
		var rep engine.Report
		resp := getJSON(t, ts, "/api/v1/clusters/1/report?target=1.40", "", &rep)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if rep.KBVersion != "test-kb" || rep.Score != 75 || len(rep.Findings) != 1 {
			t.Fatalf("what-if report = kb %q score %d findings %d, want test-kb 75 1", rep.KBVersion, rep.Score, len(rep.Findings))
		}
	})
	t.Run("unparseable target is 422", func(t *testing.T) {
		var out map[string]string
		resp := getJSON(t, ts, "/api/v1/clusters/1/report?target=bogus", "", &out)
		if resp.StatusCode != http.StatusUnprocessableEntity || out["error"] == "" {
			t.Fatalf("status %d body %v, want 422 with error", resp.StatusCode, out)
		}
	})
	t.Run("unknown cluster is 404", func(t *testing.T) {
		var out map[string]string
		resp := getJSON(t, ts, "/api/v1/clusters/999/report?target=1.35", "", &out)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
	t.Run("cluster without snapshots is 404", func(t *testing.T) {
		if _, err := st.UpsertCluster(context.Background(), store.Cluster{Name: "bare", LastSeen: time.Now()}); err != nil {
			t.Fatal(err)
		}
		var out map[string]string
		// fakeStore IDs are sequential; the bare cluster is the next ID after
		// seed (cluster 1, snapshot 2, eval 3 → bare cluster 4).
		resp := getJSON(t, ts, "/api/v1/clusters/4/report?target=1.40", "", &out)
		if resp.StatusCode != http.StatusNotFound || out["error"] == "" {
			t.Fatalf("status %d body %v, want 404 with error", resp.StatusCode, out)
		}
	})
}

func TestFindingsFilters(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	seedViaPush(t, ts) // one removed-api blocker at target 1.35

	type findingsResp struct {
		Target   string           `json:"target"`
		Findings []engine.Finding `json:"findings"`
	}
	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"no filter", "", 1},
		{"severity match", "&severity=blocker", 1},
		{"severity no match", "&severity=info", 0},
		{"category match", "&category=removed-api", 1},
		{"category no match", "&category=eol-addon", 0},
		{"both match", "&severity=blocker&category=removed-api", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got findingsResp
			resp := getJSON(t, ts, "/api/v1/clusters/1/findings?target=1.35"+tc.query, "", &got)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if got.Target != "1.35" || len(got.Findings) != tc.want {
				t.Fatalf("target %q findings %d, want 1.35 with %d", got.Target, len(got.Findings), tc.want)
			}
			if got.Findings == nil {
				t.Fatal(`findings must render as [] (non-nil), not null`)
			}
		})
	}
}

func TestHistory(t *testing.T) {
	st := newFakeStore()
	ts := httptest.NewServer(newTestServer(t, st).Handler())
	defer ts.Close()
	// Three distinct snapshots (PSP object count varies → different canonical
	// hashes; CollectedAt is zeroed for hashing) → three evaluations for target 1.35.
	for i := 0; i < 3; i++ {
		inv := testInventoryWithPSP()
		inv.APIUsage[0].Count = i + 1 // vary real content so each push is a new snapshot
		resp, _ := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, inv), true)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("push %d status = %d, want 202", i, resp.StatusCode)
		}
	}

	t.Run("default limit", func(t *testing.T) {
		var pts []store.ScorePoint
		resp := getJSON(t, ts, "/api/v1/clusters/1/history?target=1.35", "", &pts)
		if resp.StatusCode != http.StatusOK || len(pts) != 3 {
			t.Fatalf("status %d points %d, want 200 with 3", resp.StatusCode, len(pts))
		}
		for _, p := range pts {
			if p.Score != 75 || p.Ready {
				t.Fatalf("point = %+v, want score 75 ready false", p)
			}
		}
	})
	t.Run("limit applies", func(t *testing.T) {
		var pts []store.ScorePoint
		resp := getJSON(t, ts, "/api/v1/clusters/1/history?target=1.35&limit=2", "", &pts)
		if resp.StatusCode != http.StatusOK || len(pts) != 2 {
			t.Fatalf("status %d points %d, want 200 with 2", resp.StatusCode, len(pts))
		}
	})
	t.Run("missing target uses default", func(t *testing.T) {
		var pts []store.ScorePoint
		resp := getJSON(t, ts, "/api/v1/clusters/1/history", "", &pts)
		if resp.StatusCode != http.StatusOK || len(pts) != 3 {
			t.Fatalf("status %d points %d, want 200 with 3", resp.StatusCode, len(pts))
		}
	})
	t.Run("bad limit is 422", func(t *testing.T) {
		for _, bad := range []string{"abc", "0", "-3"} {
			var out map[string]string
			resp := getJSON(t, ts, "/api/v1/clusters/1/history?target=1.35&limit="+bad, "", &out)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("limit=%s status = %d, want 422", bad, resp.StatusCode)
			}
		}
	})
}

func TestMethodNotAllowed(t *testing.T) {
	ts := httptest.NewServer(newTestServer(t, newFakeStore()).Handler())
	defer ts.Close()
	cases := []struct{ method, path string }{
		{http.MethodDelete, "/api/v1/clusters"},
		{http.MethodGet, "/api/v1/snapshots"},
		{http.MethodPost, "/healthz"},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(tc.method, ts.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s status = %d, want 405 (ServeMux method patterns)", tc.method, tc.path, resp.StatusCode)
		}
		if resp.Header.Get("Allow") == "" {
			t.Fatalf("%s %s: missing Allow header on 405", tc.method, tc.path)
		}
	}
}
```

- [ ] **Step 3: Run, expect fail** — Run: `go test ./internal/server/`. Expected: tests compile (all referenced handler symbols are reached via HTTP, not Go identifiers) but `FAIL` — read routes are unregistered, so every read test sees 404 where it expects 200/401/405.

- [ ] **Step 4: Implement the read handlers and full route table**

In `internal/server/server.go`, replace `routes`:
```go
// routes registers all endpoints (Go 1.22 method+path patterns — ServeMux
// emits 405 + Allow for wrong methods on registered paths).
func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("POST /api/v1/snapshots", s.handleIngest)
	s.mux.HandleFunc("GET /api/v1/clusters", s.readAuth(s.handleListClusters))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}", s.readAuth(s.handleGetCluster))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}/report", s.readAuth(s.handleReport))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}/findings", s.readAuth(s.handleFindings))
	s.mux.HandleFunc("GET /api/v1/clusters/{id}/history", s.readAuth(s.handleHistory))
}
```

In `internal/server/api.go`, update the import block to:
```go
import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)
```

and append:
```go
// ----- read API -----

// readAuth gates a read handler behind Config.ReadToken when configured;
// an empty ReadToken leaves the read API open (the CLI documents this loudly).
func (s *Server) readAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.ReadToken != "" && !bearerOK(r, s.cfg.ReadToken) {
			errJSON(w, http.StatusUnauthorized, "invalid or missing bearer token")
			return
		}
		next(w, r)
	}
}

// handleHealthz is always unauthenticated: liveness probes carry no tokens.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// pathClusterID parses the {id} path value, writing the 400 itself on failure.
func (s *Server) pathClusterID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid cluster id")
		return 0, false
	}
	return id, true
}

// requireCluster 404s (JSON) for unknown clusters so every per-cluster
// endpoint shares one existence check.
func (s *Server) requireCluster(w http.ResponseWriter, r *http.Request) (store.Cluster, bool) {
	id, ok := s.pathClusterID(w, r)
	if !ok {
		return store.Cluster{}, false
	}
	c, err := s.cfg.Store.GetCluster(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "cluster not found")
		return store.Cluster{}, false
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "loading cluster: "+err.Error())
		return store.Cluster{}, false
	}
	return c, true
}

// defaultTarget computes a cluster's default evaluation target (next minor
// above the latest snapshot's server version) and returns the parsed latest
// inventory alongside so callers don't unmarshal twice. Errors:
// store.ErrNotFound (no snapshots) or a corrupt/unparseable-version error.
func (s *Server) defaultTarget(ctx context.Context, clusterID int64) (inventory.Version, inventory.Inventory, error) {
	snap, err := s.cfg.Store.LatestSnapshot(ctx, clusterID)
	if err != nil {
		return inventory.Version{}, inventory.Inventory{}, err
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(snap.Inventory, &inv); err != nil {
		return inventory.Version{}, inventory.Inventory{}, fmt.Errorf("stored inventory for cluster %d is corrupt: %w", clusterID, err)
	}
	server, err := inventory.ParseVersion(inv.ServerVersion)
	if err != nil {
		return inventory.Version{}, inv, fmt.Errorf("latest snapshot has no parseable server version: %w", err)
	}
	return server.Next(), inv, nil
}

// resolveTarget picks the evaluation target: explicit ?target= (422 when
// unparseable), else the cluster's default target (404 when there is no
// snapshot to derive one from). Writes the error response itself.
func (s *Server) resolveTarget(w http.ResponseWriter, r *http.Request, clusterID int64) (inventory.Version, bool) {
	if q := r.URL.Query().Get("target"); q != "" {
		v, err := inventory.ParseVersion(q)
		if err != nil {
			errJSON(w, http.StatusUnprocessableEntity, "invalid target: "+err.Error())
			return inventory.Version{}, false
		}
		return v, true
	}
	target, _, err := s.defaultTarget(r.Context(), clusterID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			errJSON(w, http.StatusNotFound, "no snapshots for cluster")
		} else {
			errJSON(w, http.StatusUnprocessableEntity, "cannot derive default target: "+err.Error())
		}
		return inventory.Version{}, false
	}
	return target, true
}

// evalSummary is the read API's compact evaluation view.
type evalSummary struct {
	Target      string    `json:"target"`
	Score       int       `json:"score"`
	Ready       bool      `json:"ready"`
	Blockers    int       `json:"blockers"`
	Warnings    int       `json:"warnings"`
	KBVersion   string    `json:"kbVersion"`
	EvaluatedAt time.Time `json:"evaluatedAt"`
}

func summarize(e store.Evaluation) evalSummary {
	return evalSummary{
		Target:      e.Target,
		Score:       e.Score,
		Ready:       e.Ready,
		Blockers:    e.Blockers,
		Warnings:    e.Warnings,
		KBVersion:   e.KBVersion,
		EvaluatedAt: e.CreatedAt,
	}
}

type clusterSummary struct {
	store.Cluster
	Latest *evalSummary `json:"latest,omitempty"` // default-target evaluation, if any
}

// handleListClusters: GET /api/v1/clusters — every cluster plus its latest
// default-target score summary (omitted when no snapshot/evaluation exists).
func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	clusters, err := s.cfg.Store.ListClusters(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "listing clusters: "+err.Error())
		return
	}
	out := make([]clusterSummary, 0, len(clusters))
	for _, c := range clusters {
		cs := clusterSummary{Cluster: c}
		if target, _, err := s.defaultTarget(ctx, c.ID); err == nil {
			if e, err := s.cfg.Store.LatestEvaluation(ctx, c.ID, target.String()); err == nil {
				sum := summarize(e)
				cs.Latest = &sum
			}
		}
		out = append(out, cs)
	}
	writeJSON(w, http.StatusOK, out)
}

type clusterDetail struct {
	store.Cluster
	Capabilities map[inventory.Capability]inventory.CapabilityStatus `json:"capabilities,omitempty"`
	Evaluations  []evalSummary                                       `json:"evaluations"`
}

// handleGetCluster: GET /api/v1/clusters/{id} — cluster row, the latest
// snapshot's capability map, and latest evaluation summaries for the default
// target plus every configured extra target.
func (s *Server) handleGetCluster(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireCluster(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	detail := clusterDetail{Cluster: c, Evaluations: []evalSummary{}}
	targets := make([]string, 0, len(s.extraTargets)+1)
	if snap, err := s.cfg.Store.LatestSnapshot(ctx, c.ID); err == nil {
		var inv inventory.Inventory
		if json.Unmarshal(snap.Inventory, &inv) == nil {
			detail.Capabilities = inv.Capabilities
			if server, err := inventory.ParseVersion(inv.ServerVersion); err == nil {
				targets = append(targets, server.Next().String())
			}
		}
	}
	for _, t := range s.extraTargets {
		targets = append(targets, t.String())
	}
	seen := map[string]bool{}
	for _, t := range targets {
		if seen[t] {
			continue
		}
		seen[t] = true
		if e, err := s.cfg.Store.LatestEvaluation(ctx, c.ID, t); err == nil {
			detail.Evaluations = append(detail.Evaluations, summarize(e))
		}
	}
	writeJSON(w, http.StatusOK, detail)
}

// loadOrComputeReport returns the stored evaluation's report for (cluster,
// target) when one exists, else computes a what-if from the latest snapshot.
// A store.ErrNotFound result means the cluster has no snapshots at all.
func (s *Server) loadOrComputeReport(ctx context.Context, clusterID int64, target inventory.Version) (engine.Report, error) {
	if e, err := s.cfg.Store.LatestEvaluation(ctx, clusterID, target.String()); err == nil {
		var rep engine.Report
		if err := json.Unmarshal(e.Report, &rep); err != nil {
			return engine.Report{}, fmt.Errorf("stored report for evaluation %d is corrupt: %w", e.ID, err)
		}
		return rep, nil
	}
	return WhatIf(ctx, s.cfg.Store, s.cfg.KB, clusterID, target, s.now())
}

// reportForRequest is the shared resolve-cluster → resolve-target → load/
// compute pipeline behind the report and findings endpoints.
func (s *Server) reportForRequest(w http.ResponseWriter, r *http.Request) (engine.Report, bool) {
	c, ok := s.requireCluster(w, r)
	if !ok {
		return engine.Report{}, false
	}
	target, ok := s.resolveTarget(w, r, c.ID)
	if !ok {
		return engine.Report{}, false
	}
	rep, err := s.loadOrComputeReport(r.Context(), c.ID, target)
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "no snapshots for cluster")
		return engine.Report{}, false
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return engine.Report{}, false
	}
	return rep, true
}

// handleReport: GET /api/v1/clusters/{id}/report?target= — full engine.Report.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	rep, ok := s.reportForRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// handleFindings: GET /api/v1/clusters/{id}/findings?target=&severity=&category=
// — the report's findings, exact-match filtered. Unknown filter values simply
// match nothing.
func (s *Server) handleFindings(w http.ResponseWriter, r *http.Request) {
	rep, ok := s.reportForRequest(w, r)
	if !ok {
		return
	}
	severity := r.URL.Query().Get("severity")
	category := r.URL.Query().Get("category")
	findings := []engine.Finding{} // non-nil so JSON renders []
	for _, f := range rep.Findings {
		if severity != "" && string(f.Severity) != severity {
			continue
		}
		if category != "" && string(f.Category) != category {
			continue
		}
		findings = append(findings, f)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"target":   rep.Target.String(),
		"findings": findings,
	})
}

// handleHistory: GET /api/v1/clusters/{id}/history?target=&limit= —
// []store.ScorePoint, oldest first, default limit 100.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireCluster(w, r)
	if !ok {
		return
	}
	target, ok := s.resolveTarget(w, r, c.ID)
	if !ok {
		return
	}
	limit := 100
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			errJSON(w, http.StatusUnprocessableEntity, "limit must be a positive integer")
			return
		}
		limit = n
	}
	points, err := s.cfg.Store.ScoreHistory(r.Context(), c.ID, target.String(), limit)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "loading history: "+err.Error())
		return
	}
	if points == nil {
		points = []store.ScorePoint{}
	}
	writeJSON(w, http.StatusOK, points)
}
```

- [ ] **Step 5: Run, expect pass** — Run: `go test ./internal/server/ -v && go vet ./internal/server/... && go build ./...`. Expected: every test in the package `--- PASS` (V1–V4), vet and build clean.

- [ ] **Step 6: Commit**

```bash
git add internal/server/ && git commit -m "feat(server): read API — clusters, report, findings, history with optional bearer auth" -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Section exit criteria

- `go test ./internal/server/` green with zero sqlite involvement (fake store only).
- Exported surface of `package server`: `Config`, `Server`, `New`, `(*Server).Handler/Start/Ready/Addr/Shutdown`, `WhatIf`. The NOTIFY-CLI section may replace exactly one method body: `(*Server).notifyDelta`.
- If V1 Step 1 created `store.go`/`notify.go`, the STORE and NOTIFY-CLI sections build on those files unchanged (interfaces are the contract; only implementations are added).

## Section: internal/server/notify + serve command

**Tasks N1–N4.** Implements the Notifier contract (`internal/server/notify/notify.go`, `slack.go`), the delta computation + ingest wiring in `internal/server` (`notify_delta.go`, modifies `api.go`), and `upgradescope serve` (`internal/cli/serve.go` + registration in `root.go`).

**Depends on:** STORE section (store.Open, Store interface, sqlite impl) and SERVER-API section (Server, Config, New, Start, Handler, the ingest handler with its no-op `notifyDelta` seam). N1 and N2 have **no** dependency on either and may run as soon as the repo compiles; N3 and N4 must run after SERVER-API is complete.

**Cross-section contract this section codes against** (normative; SERVER-API must have produced exactly these — if a name differs, fix it *there*, not here):

```go
// internal/server/server.go (SERVER-API)
type Config struct {
	Listen       string
	Store        store.Store
	KB           kb.KB
	Notifier     notify.Notifier        // never nil; serve passes notify.Multi(...)
	IngestToken  string
	ReadToken    string                 // "" = read API open
	ExtraTargets []inventory.Version    // evaluated on every snapshot in addition to default
}
func New(cfg Config) (*Server, error)
func (s *Server) Handler() http.Handler         // full mux (tests mount it on httptest)
func (s *Server) Start(ctx context.Context) error // ListenAndServe; graceful Shutdown on ctx done; nil on clean stop
// Server has unexported fields `store store.Store` and `notifier notify.Notifier`.
// api.go's ingest flow ends each per-target evaluation with the no-op seam:
//   func (s *Server) notifyDelta(ctx context.Context, clusterName string, prev *engine.Report, curr engine.Report) {}
// internal/server/store (STORE)
func Open(path string) (Store, error)
```

Delta rules implemented here are the normative ones from "Notifier" in the shared contracts: blocker-title diff → `new-blocker` (cap 5 + one "and N more"), blockers >0→0 → `became-ready`, new `eol-approaching` warnings → `eol-approaching`, `nil` prev (first-ever evaluation) → no events. Notification failures are logged, never block ingestion.

---

### Task N1: Notifier interface, Multi fan-out, NopNotifier

**Files:**
- Create: `internal/server/notify/notify.go`
- Create: `internal/server/notify/notify_test.go`

- [ ] **Write the failing test** — `internal/server/notify/notify_test.go`:

```go
package notify

import (
	"context"
	"errors"
	"testing"
)

// fakeNotifier records every event it receives and optionally fails.
type fakeNotifier struct {
	events []Event
	err    error
}

func (f *fakeNotifier) Notify(_ context.Context, ev Event) error {
	f.events = append(f.events, ev)
	return f.err
}

func TestMultiFansOutToAll(t *testing.T) {
	a, b := &fakeNotifier{}, &fakeNotifier{}
	ev := Event{Cluster: "prod-eu-1", Target: "1.37", Kind: KindNewBlocker, Title: "x removed"}

	if err := Multi(a, b).Notify(context.Background(), ev); err != nil {
		t.Fatalf("Multi.Notify: %v", err)
	}
	if len(a.events) != 1 || len(b.events) != 1 {
		t.Fatalf("want 1 event each, got a=%d b=%d", len(a.events), len(b.events))
	}
	if a.events[0] != ev || b.events[0] != ev {
		t.Fatalf("event mutated in fan-out: a=%+v b=%+v", a.events[0], b.events[0])
	}
}

func TestMultiContinuesPastFailures(t *testing.T) {
	failing := &fakeNotifier{err: errors.New("boom")}
	ok := &fakeNotifier{}
	ev := Event{Cluster: "c", Target: "1.36", Kind: KindBecameReady, Title: "ready"}

	// A failing notifier must be logged-and-skipped: the healthy one still
	// fires and Multi never propagates the error (ingestion must not block).
	if err := Multi(failing, ok).Notify(context.Background(), ev); err != nil {
		t.Fatalf("Multi must swallow individual failures, got %v", err)
	}
	if len(ok.events) != 1 {
		t.Fatalf("healthy notifier skipped after earlier failure: got %d events", len(ok.events))
	}
}

func TestMultiEmptyIsHarmless(t *testing.T) {
	if err := Multi().Notify(context.Background(), Event{}); err != nil {
		t.Fatalf("empty Multi: %v", err)
	}
}

func TestNopNotifier(t *testing.T) {
	if err := (NopNotifier{}).Notify(context.Background(), Event{Kind: KindNewBlocker}); err != nil {
		t.Fatalf("NopNotifier: %v", err)
	}
}
```

- [ ] **Run, expect failure** (package does not exist yet — compile error is the expected failure):

```sh
go test ./internal/server/notify/
# want: build fails: undefined: Event, Multi, NopNotifier, Kind*
```

- [ ] **Implement** — `internal/server/notify/notify.go` (Event + Notifier verbatim from the shared contract):

```go
// Package notify delivers upgrade-readiness change notifications.
//
// The server computes *what* changed (delta rules live in internal/server);
// this package only knows how to deliver an Event somewhere. All notifiers
// are best-effort: delivery failures must never block snapshot ingestion,
// so Multi logs individual failures and always returns nil.
package notify

import (
	"context"
	"log/slog"
)

// Kind values for Event.Kind (the only three the delta rules emit).
const (
	KindNewBlocker     = "new-blocker"
	KindEOLApproaching = "eol-approaching"
	KindBecameReady    = "became-ready"
)

type Event struct {
	Cluster  string
	Target   string
	Kind     string // "new-blocker" | "eol-approaching" | "became-ready"
	Title    string // human line, e.g. finding title
	Detail   string
}

type Notifier interface{ Notify(ctx context.Context, ev Event) error }

// Multi fans one event out to every notifier. Individual failures are
// logged and skipped — the remaining notifiers still fire and the error is
// never propagated (best-effort delivery, contract: "failures logged,
// never block ingestion"). Multi() with no arguments is a valid no-op.
func Multi(notifiers ...Notifier) Notifier { return multi(notifiers) }

type multi []Notifier

func (m multi) Notify(ctx context.Context, ev Event) error {
	for _, n := range m {
		if err := n.Notify(ctx, ev); err != nil {
			slog.Warn("notifier failed",
				"cluster", ev.Cluster, "target", ev.Target, "kind", ev.Kind, "err", err)
		}
	}
	return nil
}

// NopNotifier discards every event. Useful default when no webhook is configured.
type NopNotifier struct{}

func (NopNotifier) Notify(context.Context, Event) error { return nil }
```

- [ ] **Run, expect pass:**

```sh
go test ./internal/server/notify/
go vet ./internal/server/notify/
```

- [ ] **Commit:**

```sh
git add internal/server/notify/
git commit -m "feat(notify): Event, Notifier, Multi fan-out, NopNotifier

Multi logs-and-continues on individual notifier failures so a dead
webhook can never block snapshot ingestion.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task N2: Slack and generic webhook notifiers

**Files:**
- Create: `internal/server/notify/slack.go`
- Create: `internal/server/notify/slack_test.go`

- [ ] **Write the failing test** — `internal/server/notify/slack_test.go` (httptest only, no real network):

```go
package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testEvent() Event {
	return Event{
		Cluster: "prod-eu-1",
		Target:  "1.37",
		Kind:    KindNewBlocker,
		Title:   "policy/v1beta1 PodSecurityPolicy removed",
		Detail:  "3 objects in 2 namespaces",
	}
}

func TestSlackPostsFormattedText(t *testing.T) {
	var gotBody []byte
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := NewSlack(srv.URL).Notify(context.Background(), testEvent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	var payload map[string]string
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, gotBody)
	}
	want := "[upgradescope] prod-eu-1 → 1.37: new-blocker: policy/v1beta1 PodSecurityPolicy removed"
	if payload["text"] != want {
		t.Errorf("text = %q\nwant   %q", payload["text"], want)
	}
	if len(payload) != 1 {
		t.Errorf("payload has extra keys: %v", payload)
	}
}

func TestSlackDefaultTimeoutIsTwoSeconds(t *testing.T) {
	if got := NewSlack("http://example.invalid").Client.Timeout; got != 2*time.Second {
		t.Fatalf("default timeout = %v, want 2s", got)
	}
}

func TestSlackTimesOutOnSlowServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Same code path as the 2s default; shortened so the test stays fast.
	n := &SlackNotifier{URL: srv.URL, Client: &http.Client{Timeout: 50 * time.Millisecond}}
	if err := n.Notify(context.Background(), testEvent()); err == nil {
		t.Fatal("want timeout error from slow webhook, got nil")
	}
}

func TestSlackNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no_service", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := NewSlack(srv.URL).Notify(context.Background(), testEvent())
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("want status-500 error, got %v", err)
	}
}

func TestGenericWebhookPostsEventJSON(t *testing.T) {
	var gotBody []byte
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted) // any 2xx is success
	}))
	defer srv.Close()

	ev := testEvent()
	if err := NewGenericWebhook(srv.URL).Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	var got Event
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("payload not Event JSON: %v (%s)", err, gotBody)
	}
	if got != ev {
		t.Errorf("round-tripped event = %+v, want %+v", got, ev)
	}
}

func TestGenericWebhookNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	err := NewGenericWebhook(srv.URL).Notify(context.Background(), testEvent())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("want status-403 error, got %v", err)
	}
}
```

- [ ] **Run, expect failure** (compile error: `SlackNotifier`, `NewSlack`, `NewGenericWebhook` undefined):

```sh
go test ./internal/server/notify/
```

- [ ] **Implement** — `internal/server/notify/slack.go`:

```go
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// webhookTimeout bounds each delivery attempt. Notifications run inside the
// ingest flow, so a hung webhook must not hold a snapshot hostage.
const webhookTimeout = 2 * time.Second

// FormatLine renders the canonical one-line notification text:
//
//	[upgradescope] <cluster> → <target>: <kind>: <title>
func FormatLine(ev Event) string {
	return fmt.Sprintf("[upgradescope] %s → %s: %s: %s", ev.Cluster, ev.Target, ev.Kind, ev.Title)
}

// SlackNotifier posts events to a Slack incoming webhook as plain text.
type SlackNotifier struct {
	URL    string
	Client *http.Client // exported so tests shorten the timeout; never nil from NewSlack
}

// NewSlack returns a SlackNotifier with the 2s delivery timeout.
func NewSlack(url string) *SlackNotifier {
	return &SlackNotifier{URL: url, Client: &http.Client{Timeout: webhookTimeout}}
}

func (s *SlackNotifier) Notify(ctx context.Context, ev Event) error {
	payload, err := json.Marshal(map[string]string{"text": FormatLine(ev)})
	if err != nil {
		return fmt.Errorf("slack: encode payload: %w", err)
	}
	return postJSON(ctx, s.Client, "slack webhook", s.URL, payload)
}

// GenericWebhook POSTs the raw Event as JSON to any HTTP endpoint.
type GenericWebhook struct {
	URL    string
	Client *http.Client
}

// NewGenericWebhook returns a GenericWebhook with the 2s delivery timeout.
func NewGenericWebhook(url string) *GenericWebhook {
	return &GenericWebhook{URL: url, Client: &http.Client{Timeout: webhookTimeout}}
}

func (g *GenericWebhook) Notify(ctx context.Context, ev Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("webhook: encode event: %w", err)
	}
	return postJSON(ctx, g.Client, "webhook", g.URL, payload)
}

// postJSON sends one JSON POST and treats any non-2xx status as an error.
func postJSON(ctx context.Context, client *http.Client, label, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", label, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // drain for connection reuse
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s: unexpected status %d", label, resp.StatusCode)
	}
	return nil
}
```

> Note: Event is kept tag-free (the contract struct is verbatim law), so GenericWebhook's wire keys are the Go field names (`Cluster`, `Target`, `Kind`, `Title`, `Detail`). The round-trip test pins that.

- [ ] **Run, expect pass:**

```sh
go test ./internal/server/notify/
go vet ./internal/server/notify/
```

- [ ] **Commit:**

```sh
git add internal/server/notify/slack.go internal/server/notify/slack_test.go
git commit -m "feat(notify): Slack incoming-webhook and generic webhook notifiers

Plain-text Slack payload {\"text\": \"[upgradescope] cluster → target:
kind: title\"}, generic webhook POSTs the Event JSON. 2s timeout,
non-2xx is an error; Multi upstream keeps failures non-fatal.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task N3: Delta computation + wiring into the ingest flow

**Files:**
- Create: `internal/server/notify_delta.go`
- Create: `internal/server/notify_delta_test.go`
- Create: `internal/server/notify_delta_wire_test.go`
- Modify: `internal/server/api.go` (replace SERVER-API's no-op `notifyDelta` seam; capture prev evaluation **before** `InsertEvaluation`)

**Prerequisite:** SERVER-API section complete (`go test ./internal/server/...` green with the no-op seam).

- [ ] **Write the failing table tests for ComputeDelta** — `internal/server/notify_delta_test.go`:

```go
package server

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
)

// rep builds a minimal report for delta tests. ClusterID/Target feed the
// Event fields; Score feeds the became-ready detail line.
func rep(t *testing.T, score int, findings ...engine.Finding) engine.Report {
	t.Helper()
	target, err := inventory.ParseVersion("1.36")
	if err != nil {
		t.Fatal(err)
	}
	return engine.Report{ClusterID: "uid-1", Target: target, Score: score, Findings: findings}
}

func blocker(title string) engine.Finding {
	return engine.Finding{Category: engine.CatRemovedAPI, Severity: engine.SevBlocker, Title: title, Detail: "detail: " + title}
}

func eolWarn(title string) engine.Finding {
	return engine.Finding{Category: engine.CatEOLApproaching, Severity: engine.SevWarning, Title: title, Detail: "detail: " + title}
}

func TestComputeDelta(t *testing.T) {
	prevOneBlocker := rep(t, 40, blocker("psp removed"))

	manyNew := make([]engine.Finding, 7)
	for i := range manyNew {
		manyNew[i] = blocker(fmt.Sprintf("blocker-%d", i))
	}

	cases := []struct {
		name string
		prev *engine.Report
		curr engine.Report
		want []notify.Event
	}{
		{
			name: "first evaluation: nil prev emits nothing even with blockers",
			prev: nil,
			curr: rep(t, 40, blocker("psp removed")),
			want: nil,
		},
		{
			name: "no change emits nothing",
			prev: &prevOneBlocker,
			curr: rep(t, 40, blocker("psp removed")),
			want: nil,
		},
		{
			name: "new blocker emits new-blocker with title and detail",
			prev: &prevOneBlocker,
			curr: rep(t, 25, blocker("psp removed"), blocker("flowcontrol v1beta3 removed")),
			want: []notify.Event{{
				Cluster: "uid-1", Target: "1.36", Kind: notify.KindNewBlocker,
				Title: "flowcontrol v1beta3 removed", Detail: "detail: flowcontrol v1beta3 removed",
			}},
		},
		{
			name: "blocker resolved but others remain: no events",
			prev: func() *engine.Report {
				r := rep(t, 25, blocker("a"), blocker("b"))
				return &r
			}(),
			curr: rep(t, 40, blocker("a")),
			want: nil,
		},
		{
			name: "all blockers resolved emits became-ready",
			prev: &prevOneBlocker,
			curr: rep(t, 92),
			want: []notify.Event{{
				Cluster: "uid-1", Target: "1.36", Kind: notify.KindBecameReady,
				Title: "ready for 1.36: all blockers resolved", Detail: "score 92",
			}},
		},
		{
			name: "new eol-approaching warning emits eol-approaching",
			prev: func() *engine.Report {
				r := rep(t, 80, eolWarn("ingress-nginx EOL 2026-03"))
				return &r
			}(),
			curr: rep(t, 70, eolWarn("ingress-nginx EOL 2026-03"), eolWarn("chart foo EOL 2026-09")),
			want: []notify.Event{{
				Cluster: "uid-1", Target: "1.36", Kind: notify.KindEOLApproaching,
				Title: "chart foo EOL 2026-09", Detail: "detail: chart foo EOL 2026-09",
			}},
		},
		{
			name: "non-blocker severities never produce new-blocker events",
			prev: func() *engine.Report {
				r := rep(t, 90)
				return &r
			}(),
			curr: rep(t, 85, engine.Finding{Category: engine.CatVersionSkew, Severity: engine.SevWarning, Title: "skew"}),
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeDelta(tc.prev, tc.curr)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ComputeDelta:\n got  %+v\n want %+v", got, tc.want)
			}
		})
	}

	t.Run("seven new blockers capped at 5 plus and-N-more", func(t *testing.T) {
		prev := rep(t, 90)
		got := ComputeDelta(&prev, rep(t, 10, manyNew...))
		if len(got) != 6 {
			t.Fatalf("want 5 events + 1 summary = 6, got %d: %+v", len(got), got)
		}
		for i := 0; i < 5; i++ {
			if got[i].Kind != notify.KindNewBlocker || got[i].Title != fmt.Sprintf("blocker-%d", i) {
				t.Errorf("event %d = %+v, want new-blocker blocker-%d", i, got[i], i)
			}
		}
		last := got[5]
		if last.Kind != notify.KindNewBlocker || last.Title != "and 2 more new blockers" {
			t.Errorf("summary event = %+v, want \"and 2 more new blockers\"", last)
		}
	})
}
```

- [ ] **Run, expect failure** (compile error: `ComputeDelta` undefined):

```sh
go test ./internal/server/ -run TestComputeDelta
```

- [ ] **Implement** — `internal/server/notify_delta.go`:

```go
package server

import (
	"fmt"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
)

// maxBlockerEvents caps per-evaluation new-blocker noise; the overflow is
// summarized as one "and N more new blockers" event.
const maxBlockerEvents = 5

// ComputeDelta implements the notification delta rules (shared contract):
//
//   - prev == nil (first-ever evaluation of this cluster+target) → no events.
//   - Blocker findings added since prev (diff by title) → one new-blocker
//     event each, capped at maxBlockerEvents, then a single "and N more".
//   - Blocker count went >0 → 0 → one became-ready event.
//   - eol-approaching warnings added since prev (by title) → one event each.
//
// Event.Cluster is filled with curr.ClusterID (the inventory UID); the
// caller (notifyDelta) overwrites it with the human cluster name from the
// push envelope before delivery. Order is deterministic: new-blockers in
// report order, summary, became-ready, eol-approaching in report order.
func ComputeDelta(prev *engine.Report, curr engine.Report) []notify.Event {
	if prev == nil {
		return nil
	}
	cluster, target := curr.ClusterID, curr.Target.String()

	prevBlockers := titleSet(prev.Findings, isBlocker)
	prevEOL := titleSet(prev.Findings, isEOLApproaching)

	var events []notify.Event

	var newBlockers []engine.Finding
	currBlockerCount := 0
	for _, f := range curr.Findings {
		if !isBlocker(f) {
			continue
		}
		currBlockerCount++
		if !prevBlockers[f.Title] {
			newBlockers = append(newBlockers, f)
		}
	}
	for i, f := range newBlockers {
		if i == maxBlockerEvents {
			events = append(events, notify.Event{
				Cluster: cluster, Target: target, Kind: notify.KindNewBlocker,
				Title: fmt.Sprintf("and %d more new blockers", len(newBlockers)-maxBlockerEvents),
			})
			break
		}
		events = append(events, notify.Event{
			Cluster: cluster, Target: target, Kind: notify.KindNewBlocker,
			Title: f.Title, Detail: f.Detail,
		})
	}

	if len(prevBlockers) > 0 && currBlockerCount == 0 {
		events = append(events, notify.Event{
			Cluster: cluster, Target: target, Kind: notify.KindBecameReady,
			Title:  fmt.Sprintf("ready for %s: all blockers resolved", target),
			Detail: fmt.Sprintf("score %d", curr.Score),
		})
	}

	for _, f := range curr.Findings {
		if isEOLApproaching(f) && !prevEOL[f.Title] {
			events = append(events, notify.Event{
				Cluster: cluster, Target: target, Kind: notify.KindEOLApproaching,
				Title: f.Title, Detail: f.Detail,
			})
		}
	}
	return events
}

func isBlocker(f engine.Finding) bool { return f.Severity == engine.SevBlocker }

func isEOLApproaching(f engine.Finding) bool {
	return f.Category == engine.CatEOLApproaching && f.Severity == engine.SevWarning
}

func titleSet(fs []engine.Finding, keep func(engine.Finding) bool) map[string]bool {
	s := make(map[string]bool)
	for _, f := range fs {
		if keep(f) {
			s[f.Title] = true
		}
	}
	return s
}
```

- [ ] **Run, expect pass:**

```sh
go test ./internal/server/ -run TestComputeDelta
```

- [ ] **Write the failing ingest-wiring test** — `internal/server/notify_delta_wire_test.go`. This is the test that pins the **ordering trap**: the previous evaluation must be loaded via `store.LatestEvaluation` **before** `InsertEvaluation` stores the new one. With a real SQLite store, doing it in the wrong order makes "previous" the evaluation just inserted → every delta compares a report to itself → the became-ready event below never fires:

```go
package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

type recordingNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (r *recordingNotifier) Notify(_ context.Context, ev notify.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingNotifier) all() []notify.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notify.Event(nil), r.events...)
}

// postSnapshot pushes one inventory through the real ingest endpoint
// (gzip JSON + bearer, per the snapshot push protocol) and requires 202.
func postSnapshot(t *testing.T, baseURL, token string, inv inventory.Inventory) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"clusterName":   "prod-test",
		"agentVersion":  "test",
		"kbVersion":     "test",
		"inventory":     inv,
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/snapshots", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ingest status = %d, want 202", resp.StatusCode)
	}
}

// TestIngestEmitsDeltaNotifications drives two snapshots end-to-end:
// snapshot 1 carries PodSecurityPolicy usage (removed 1.25 → blocker for
// target 1.36) and must NOT notify (first-ever evaluation); snapshot 2 has
// the usage gone (blockers 1→0) and must emit exactly one became-ready
// event carrying the envelope cluster name. The real SQLite store makes
// this fail if prev is loaded after InsertEvaluation.
func TestIngestEmitsDeltaNotifications(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	kbData, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingNotifier{}
	srv, err := New(Config{Store: st, KB: kbData, Notifier: rec, IngestToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	base := inventory.Inventory{
		SchemaVersion: 1,
		ClusterID:     "uid-1",
		CollectedAt:   time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		ServerVersion: "v1.35.0", // default target = next minor = 1.36
		Capabilities:  map[inventory.Capability]inventory.CapabilityStatus{},
	}

	withPSP := base
	withPSP.APIUsage = []inventory.APIUsage{
		{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 1},
	}
	postSnapshot(t, ts.URL, "tok", withPSP)
	if evs := rec.all(); len(evs) != 0 {
		t.Fatalf("first evaluation must not notify, got %+v", evs)
	}

	clean := base // content differs from withPSP (no PSP usage) => new hash
	postSnapshot(t, ts.URL, "tok", clean)

	evs := rec.all()
	if len(evs) != 1 {
		t.Fatalf("want exactly 1 became-ready event, got %d: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.Kind != notify.KindBecameReady {
		t.Errorf("Kind = %q, want %q", ev.Kind, notify.KindBecameReady)
	}
	if ev.Cluster != "prod-test" {
		t.Errorf("Cluster = %q, want envelope clusterName %q (not inventory UID)", ev.Cluster, "prod-test")
	}
	if ev.Target != "1.36" {
		t.Errorf("Target = %q, want 1.36", ev.Target)
	}
}
```

> Assumption pinned by SERVER-API: ingest evaluation + notification run **synchronously** in the handler before the 202 is written. If SERVER-API made it async, this test (not the wiring) must gain a wait loop — flag it in review rather than silently sleeping.

- [ ] **Run, expect failure** (the seam is still a no-op → 0 events after snapshot 2):

```sh
go test ./internal/server/ -run TestIngestEmitsDeltaNotifications
# want: "want exactly 1 became-ready event, got 0"
```

- [ ] **Implement the wiring** — modify `internal/server/api.go`. Two changes:

**(a)** In the ingest flow's per-target evaluation loop (the function SERVER-API's ingest task built around `InsertEvaluation` — its plan left `s.notifyDelta(...)` as the explicit seam), capture the previous report **before** inserting, and notify **after** the insert succeeds. The loop body around `InsertEvaluation` becomes exactly:

```go
		// ORDERING: load the previous evaluation BEFORE inserting the new
		// one — LatestEvaluation after the insert would return the row we
		// just wrote and every delta would be empty.
		prev := s.loadPrevReport(ctx, clusterID, target.String())

		evalRow := store.Evaluation{
			ClusterID:  clusterID,
			SnapshotID: snapshotID,
			Target:     target.String(),
			KBVersion:  report.KBVersion,
			Score:      report.Score,
			Ready:      report.Ready,
			Blockers:   countSeverity(report, engine.SevBlocker),
			Warnings:   countSeverity(report, engine.SevWarning),
			Report:     reportJSON,
		}
		if _, err := s.store.InsertEvaluation(ctx, evalRow); err != nil {
			return fmt.Errorf("store evaluation for %s: %w", target, err)
		}

		s.notifyDelta(ctx, clusterName, prev, report)
```

(`evalRow`/`reportJSON`/`countSeverity` are SERVER-API's existing locals/helpers — keep its names; the *normative* change is the three marked pieces: `loadPrevReport` before `InsertEvaluation`, then `notifyDelta` after, with `prev` threaded through. `clusterName` is the push-envelope name SERVER-API already has in scope for `UpsertCluster`.)

**(b)** Replace the no-op `notifyDelta` body and add `loadPrevReport`, in `internal/server/api.go`:

```go
// loadPrevReport returns the decoded report of the latest stored evaluation
// for (cluster, target), or nil when none exists or it cannot be decoded —
// nil means "first evaluation" and ComputeDelta emits nothing.
//
// MUST be called before InsertEvaluation for the same (cluster, target);
// see the ordering comment at the call site.
func (s *Server) loadPrevReport(ctx context.Context, clusterID int64, target string) *engine.Report {
	prevEval, err := s.store.LatestEvaluation(ctx, clusterID, target)
	if err != nil {
		return nil // not-found (first evaluation) or read error: treat as first
	}
	var r engine.Report
	if err := json.Unmarshal(prevEval.Report, &r); err != nil {
		slog.Warn("decode previous evaluation report",
			"clusterID", clusterID, "target", target, "err", err)
		return nil
	}
	return &r
}

// notifyDelta emits delta notifications for one (cluster, target)
// evaluation. prev == nil (first-ever evaluation) emits nothing. Cluster is
// stamped with the human name from the push envelope (ComputeDelta only has
// the inventory UID). Failures are logged by the notifier chain and never
// fail ingestion — this method deliberately returns nothing.
func (s *Server) notifyDelta(ctx context.Context, clusterName string, prev *engine.Report, curr engine.Report) {
	for _, ev := range ComputeDelta(prev, curr) {
		ev.Cluster = clusterName
		if err := s.notifier.Notify(ctx, ev); err != nil {
			slog.Warn("notification failed",
				"cluster", clusterName, "target", ev.Target, "kind", ev.Kind, "err", err)
		}
	}
}
```

Add `encoding/json` and `log/slog` to api.go's imports if not already present. If SERVER-API's no-op seam had a different parameter list, change the seam to **this** signature (`clusterName string, prev *engine.Report, curr engine.Report`) — this section is normative for it.

- [ ] **Run, expect pass — wiring test, then the whole server tree (proves SERVER-API's existing ingest tests still pass with the modified flow):**

```sh
go test ./internal/server/ -run TestIngestEmitsDeltaNotifications
go test ./internal/server/...
go vet ./internal/server/...
```

- [ ] **Commit:**

```sh
git add internal/server/notify_delta.go internal/server/notify_delta_test.go internal/server/notify_delta_wire_test.go internal/server/api.go
git commit -m "feat(server): delta notifications on ingest

ComputeDelta implements the contract rules: new blockers by title diff
(cap 5 + summary), became-ready on blockers >0→0, new eol-approaching
warnings; nil prev (first evaluation) emits nothing. Ingest now loads
the previous evaluation BEFORE InsertEvaluation and notifies after —
wire test against real SQLite pins the ordering.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task N4: `upgradescope serve` command

**Files:**
- Create: `internal/cli/serve.go`
- Create: `internal/cli/serve_test.go`
- Modify: `internal/cli/root.go` (register `newServeCmd()`)

Mirrors scan.go's seam pattern exactly: package var `runServe` holds the real wiring; cobra tests stub it and only exercise flag parsing/validation.

- [ ] **Write the failing tests** — `internal/cli/serve_test.go`:

```go
package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// execServe runs the serve command with args, swapping the wiring for stub
// (same seam pattern as execScan).
func execServe(t *testing.T, args []string, stub func(context.Context, serveOptions) error) error {
	t.Helper()
	orig := runServe
	runServe = stub
	t.Cleanup(func() { runServe = orig })

	cmd := newServeCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	return cmd.Execute()
}

func serveOK() func(context.Context, serveOptions) error {
	return func(context.Context, serveOptions) error { return nil }
}

func TestServeRequiresIngestToken(t *testing.T) {
	err := execServe(t, []string{}, serveOK())
	if err == nil || !strings.Contains(err.Error(), "ingest-token") {
		t.Fatalf("want missing --ingest-token error, got %v", err)
	}
}

func TestServeRejectsBadTargets(t *testing.T) {
	err := execServe(t, []string{"--ingest-token", "t", "--targets", "1.37,banana"}, serveOK())
	if err == nil || !strings.Contains(err.Error(), "--targets") {
		t.Fatalf("want invalid --targets error, got %v", err)
	}
}

// --targets is parsed exactly once, in validateServeOptions; runServe
// receives []inventory.Version, never the raw CSV.
func TestServePassesParsedTargets(t *testing.T) {
	var got []inventory.Version
	err := execServe(t, []string{"--ingest-token", "t", "--targets", "1.37, 1.38"},
		func(_ context.Context, opts serveOptions) error {
			got = opts.parsedTargets
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	want := []inventory.Version{{Major: 1, Minor: 37}, {Major: 1, Minor: 38}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsedTargets = %v, want %v", got, want)
	}
}

func TestServeDefaults(t *testing.T) {
	var got serveOptions
	err := execServe(t, []string{"--ingest-token", "t"},
		func(_ context.Context, opts serveOptions) error {
			got = opts
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if got.listen != ":8080" {
		t.Errorf("listen = %q, want :8080", got.listen)
	}
	if got.db != "upgradescope.db" {
		t.Errorf("db = %q, want upgradescope.db", got.db)
	}
	if got.readToken != "" || got.slackWebhook != "" || got.webhook != "" {
		t.Errorf("optional flags must default empty, got %+v", got)
	}
	if got.parsedTargets != nil {
		t.Errorf("parsedTargets = %v, want nil when --targets omitted", got.parsedTargets)
	}
}

func TestServeReceivesContext(t *testing.T) {
	var got context.Context
	err := execServe(t, []string{"--ingest-token", "t"},
		func(ctx context.Context, _ serveOptions) error {
			got = ctx
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("runServe must receive the signal-aware context, got nil")
	}
}

// TestRunServeCreatesDBParentDir exercises the REAL runServe wiring
// (store.Open → kb.Load → server.New → Start) with an already-cancelled
// context: Start shuts down immediately (SERVER-API graceful-stop
// contract) and the nested --db parent directory must have been created.
func TestRunServeCreatesDBParentDir(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nested", "dir", "db.sqlite")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runServe(ctx, serveOptions{
		listen:      "127.0.0.1:0",
		db:          dbPath,
		ingestToken: "t",
	})
	if err != nil {
		t.Fatalf("runServe with cancelled ctx: %v", err)
	}
	if _, statErr := os.Stat(filepath.Dir(dbPath)); statErr != nil {
		t.Fatalf("db parent dir not created: %v", statErr)
	}
}

func TestRootRegistersServe(t *testing.T) {
	for _, c := range Root().Commands() {
		if c.Name() == "serve" {
			return
		}
	}
	t.Fatal("serve command not registered on root")
}
```

- [ ] **Run, expect failure** (compile error: `serveOptions`, `runServe`, `newServeCmd` undefined):

```sh
go test ./internal/cli/
```

- [ ] **Implement** — `internal/cli/serve.go`:

```go
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

type serveOptions struct {
	listen       string
	db           string
	ingestToken  string
	readToken    string
	slackWebhook string
	webhook      string
	targets      string

	// parsedTargets is opts.targets parsed once by validateServeOptions;
	// runServe consumes it instead of re-parsing the raw CSV.
	parsedTargets []inventory.Version
}

// runServe is the real wiring: store.Open → kb.Load → notify.Multi →
// server.New → Start (blocks until ctx is cancelled, then graceful stop).
// A package var so command tests can stub it, same seam as runScan.
var runServe = func(ctx context.Context, opts serveOptions) error {
	if dir := filepath.Dir(opts.db); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create db directory: %w", err)
		}
	}
	st, err := store.Open(opts.db)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	kbData, err := kb.Load()
	if err != nil {
		return fmt.Errorf("load knowledge base: %w", err)
	}

	var notifiers []notify.Notifier
	if opts.slackWebhook != "" {
		notifiers = append(notifiers, notify.NewSlack(opts.slackWebhook))
	}
	if opts.webhook != "" {
		notifiers = append(notifiers, notify.NewGenericWebhook(opts.webhook))
	}

	srv, err := server.New(server.Config{
		Listen:       opts.listen,
		Store:        st,
		KB:           kbData,
		Notifier:     notify.Multi(notifiers...), // zero notifiers → harmless no-op
		IngestToken:  opts.ingestToken,
		ReadToken:    opts.readToken,
		ExtraTargets: opts.parsedTargets,
	})
	if err != nil {
		return err
	}
	return srv.Start(ctx)
}

func newServeCmd() *cobra.Command {
	var opts serveOptions
	cmd := &cobra.Command{
		Use:           "serve",
		Short:         "Run the upgradescope server: snapshot ingest, REST API, history, notifications",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateServeOptions(&opts); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runServe(ctx, opts)
		},
	}

	cmd.Flags().StringVar(&opts.listen, "listen", ":8080", "address to listen on")
	cmd.Flags().StringVar(&opts.db, "db", "upgradescope.db", "path to the SQLite database (parent directory is created)")
	cmd.Flags().StringVar(&opts.ingestToken, "ingest-token", "", "bearer token agents must present to push snapshots (required)")
	cmd.Flags().StringVar(&opts.readToken, "read-token", "", "bearer token for the read API (empty = OPEN read access)")
	cmd.Flags().StringVar(&opts.slackWebhook, "slack-webhook", "", "Slack incoming-webhook URL for delta notifications")
	cmd.Flags().StringVar(&opts.webhook, "webhook", "", "generic webhook URL (POSTed the raw event JSON)")
	cmd.Flags().StringVar(&opts.targets, "targets", "", "extra target versions evaluated on every snapshot, CSV, e.g. 1.37,1.38")
	_ = cmd.MarkFlagRequired("ingest-token")

	return cmd
}

// validateServeOptions parses --targets once into opts.parsedTargets
// (single parse site — runServe never sees the raw CSV).
func validateServeOptions(opts *serveOptions) error {
	if opts.targets == "" {
		return nil
	}
	for _, raw := range strings.Split(opts.targets, ",") {
		v, err := inventory.ParseVersion(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid --targets entry %q: %w", raw, err)
		}
		opts.parsedTargets = append(opts.parsedTargets, v)
	}
	return nil
}
```

- [ ] **Register in root** — `internal/cli/root.go`, add one line after the scan registration (the CRD-AGENT section adds `newAgentCmd()` the same way; keep both):

```go
	root.AddCommand(newScanCmd())
	root.AddCommand(newServeCmd())
```

- [ ] **Run, expect pass:**

```sh
go test ./internal/cli/
go vet ./internal/cli/
go build ./...
```

- [ ] **Smoke-check the binary by hand** (graceful shutdown: Ctrl-C must exit 0 promptly):

```sh
go run ./cmd/upgradescope serve --ingest-token dev --db /tmp/us-smoke/db.sqlite &
sleep 1 && curl -fsS localhost:8080/healthz && kill -INT %1 && wait %1; echo "exit=$?"
# want: {"status":"ok"} then exit=0
```

- [ ] **Commit:**

```sh
git add internal/cli/serve.go internal/cli/serve_test.go internal/cli/root.go
git commit -m "feat(cli): add serve command

Wires store.Open → kb.Load → notify.Multi → server.New → Start with
SIGINT/SIGTERM graceful shutdown via signal.NotifyContext. --ingest-token
required; --targets CSV parsed once in validation; --db parent directory
auto-created. Flag tests use the same runServe seam pattern as scan.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

**Section exit criteria:** `go test ./internal/server/... ./internal/cli/` green, `go vet ./...` clean, `upgradescope serve --help` shows all seven flags, and the N3 wire test proves: no notification on first evaluation, became-ready on blockers 1→0, prev loaded before insert.

# Section: deploy/chart + e2e

<!-- SECTION:CHART-E2E -->

## Section: deploy/chart + e2e

Packaging and proof: container image (Dockerfile, distroless static), Helm chart
(`deploy/chart` — agent Deployment + RBAC + CRD, optional server), shell-based chart
contract tests (`make chart-test`), the end-to-end demo script
(`hack/demo/agent-e2e.sh`), and a gated Go integration test for agent+CRD.

**Depends on:** CRD-AGENT (internal/crd, internal/agent, `upgradescope agent` command),
SERVER-API + NOTIFY-CLI (`upgradescope serve` command). All chart/template/Dockerfile
tasks (H1–H5) can be written before those land — they only assert rendered YAML — but
H6/H7 need working `agent` and `serve` subcommands.

**Image naming (normative):** `ghcr.io/abd-ulbasit/upgradescope:dev` for all local work
(`make docker-build`, kind load, chart defaults). No goreleaser in P2 — plain
`docker build` via Makefile.

**TDD note:** charts are tested with `hack/test-chart.sh` (helm lint + `helm template`
grep assertions — no cluster needed). Within each chart task, assertions are appended
to the script *first* (red), then templates are written (green). The e2e script and Go
integration test are the cluster-backed layer, gated on `UPGRADESCOPE_IT=1` / explicit
invocation, per spec §10.

---

### Task H1: Dockerfile + image Makefile targets

**Files:**
- `Dockerfile` (new)
- `.dockerignore` (new)
- `Makefile` (extend)

- [ ] Write `.dockerignore` (keep the build context small; never exclude anything under `internal/` — embedded KB/registry data lives there):

```
.git
bin/
dist/
docs/
deploy/
hack/
*.md
```

- [ ] Write `Dockerfile` — multi-stage, CGO-free static binary (modernc.org/sqlite is pure Go, so `CGO_ENABLED=0` holds for `serve` too), distroless static, nonroot. The `-X` path matches the existing `var version = "dev"` in `internal/cli/root.go`:

```dockerfile
# syntax=docker/dockerfile:1
# Multi-stage build: static Go binary -> distroless. One binary, three
# subcommands (scan/agent/serve); the chart picks the subcommand via args.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X github.com/abd-ulbasit/upgradescope/internal/cli.version=${VERSION}" \
    -o /out/upgradescope ./cmd/upgradescope

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/upgradescope /upgradescope
USER 65532:65532
ENTRYPOINT ["/upgradescope"]
```

- [ ] Append to `Makefile`:

```make
IMAGE ?= ghcr.io/abd-ulbasit/upgradescope
TAG ?= dev
VERSION ?= dev

.PHONY: docker-build kind-load
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) .
kind-load: docker-build
	kind load docker-image $(IMAGE):$(TAG) --name upgradescope-demo
```

- [ ] Verify: `make docker-build` succeeds (Colima must be running: `colima status || colima start`). Then `docker run --rm ghcr.io/abd-ulbasit/upgradescope:dev --version` prints the version string. (`make kind-load` is exercised by H6 — it needs the demo cluster.)
- [ ] Commit:

```
git add Dockerfile .dockerignore Makefile
git commit -m "build: Dockerfile (distroless static) + docker-build/kind-load targets" \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task H2: chart scaffold — Chart.yaml, values.yaml, helpers, CRD copy, chart-test harness

**Files:**
- `deploy/chart/Chart.yaml` (new)
- `deploy/chart/values.yaml` (new)
- `deploy/chart/templates/_helpers.tpl` (new)
- `deploy/chart/templates/NOTES.txt` (new)
- `deploy/chart/crds/clusterreadinesses.upgradescope.dev.yaml` (new — copied)
- `hack/test-chart.sh` (new)
- `Makefile` (extend)

- [ ] Write `deploy/chart/Chart.yaml`:

```yaml
apiVersion: v2
name: upgradescope
description: Continuous Kubernetes upgrade-readiness scanner — read-only agent publishing a ClusterReadiness CRD, with an optional in-cluster server for history, what-if, and notifications.
type: application
version: 0.1.0
appVersion: "0.2.0"
home: https://github.com/abd-ulbasit/upgradescope
sources:
  - https://github.com/abd-ulbasit/upgradescope
```

- [ ] Write `deploy/chart/values.yaml` — this is a normative contract file; the RBAC trade-off comment is part of the deliverable, not decoration:

```yaml
# Default values for upgradescope.

image:
  repository: ghcr.io/abd-ulbasit/upgradescope
  tag: dev
  pullPolicy: IfNotPresent

serviceAccount:
  # Create the agent ServiceAccount. Set false to bring your own (set name).
  create: true
  name: ""

rbac:
  # Create the agent ClusterRole/ClusterRoleBinding.
  #
  # HONEST TRADE-OFF — read before installing:
  # The agent ClusterRole grants get/list/watch on ALL resources in ALL API
  # groups (cluster-wide, strictly read-only). Two collectors force this:
  #   1. Helm release detection reads Secrets of type helm.sh/release.v1.
  #      RBAC cannot filter Secrets by type and release names are dynamic,
  #      so Helm detection requires cluster-wide secret get+list. If that is
  #      unacceptable in your environment, set rbac.create=false and bind a
  #      narrower role of your own — the Helm collector then degrades to
  #      "not assessed (secrets list forbidden)" instead of failing.
  #   2. Deprecated-API detection lists objects (metadata-only) at whatever
  #      deprecated group/versions the embedded KB flags. The group set is
  #      data-driven and grows with every KB update, so enumerating groups
  #      in RBAC would silently rot.
  # Write access is limited to exactly one resource: the ClusterReadiness
  # CR and its CRD. Nothing else in the cluster is ever mutated (spec §12).
  create: true

agent:
  # Evaluation interval (minimum 1m).
  interval: 10m
  # Name of the ClusterReadiness object the agent manages.
  crName: cluster
  # Namespace label used for team attribution.
  teamLabel: team
  # Human-readable cluster name sent to the server (default: cluster UID).
  clusterName: ""
  # Target Kubernetes minors written into the ClusterReadiness spec at
  # install time, e.g. ["1.37", "1.38"]. Empty = agent creates the CR with
  # an empty spec and defaults to the next minor above the server version.
  targets: []
  # URL of an upgradescope server to push snapshots to. Empty = CRD-only
  # mode. If server.enabled=true and this is empty, it defaults to the
  # in-chart server Service.
  serverUrl: ""
  # Bearer token for snapshot pushes. Ignored when existingSecret is set.
  # When empty and server.enabled=true, defaults to server.ingestToken.
  serverToken: ""
  # Name of an existing Secret with key "serverToken" (preferred over an
  # inline token for anything beyond dev).
  existingSecret: ""
  resources:
    requests:
      cpu: 50m
      memory: 64Mi
    limits:
      cpu: 200m
      memory: 256Mi

server:
  # Run the upgradescope server in-cluster too (single-cluster combined
  # install). The agent will push to it automatically.
  enabled: false
  # Shared bearer token agents must present on POST /api/v1/snapshots.
  # Required when enabled (unless existingSecret is set).
  ingestToken: ""
  # Optional bearer token for the read API. EMPTY = READ API IS OPEN —
  # acceptable behind a ClusterIP Service on a private cluster, but set one
  # before exposing the Service in any way.
  readToken: ""
  # Name of an existing Secret with key "ingestToken" (and optionally
  # "readToken" — only read when server.readToken is also set; P2 keeps
  # secret wiring simple).
  existingSecret: ""
  # Optional Slack incoming-webhook URL for finding-delta notifications.
  slackWebhook: ""
  # Optional generic webhook URL (POSTs the Event JSON).
  webhook: ""
  # Extra targets evaluated on every accepted snapshot, e.g. ["1.37","1.38"].
  targets: []
  service:
    type: ClusterIP
    port: 8080
  persistence:
    # PVC for the SQLite database. false = emptyDir (history lost on
    # pod restart — demo only).
    enabled: true
    size: 1Gi
    storageClass: ""
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      cpu: 500m
      memory: 512Mi
```

- [ ] Write `deploy/chart/templates/_helpers.tpl`:

```
{{/* Chart name */}}
{{- define "upgradescope.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified app name */}}
{{- define "upgradescope.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* Common labels */}}
{{- define "upgradescope.labels" -}}
app.kubernetes.io/name: {{ include "upgradescope.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/* Agent ServiceAccount name */}}
{{- define "upgradescope.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "upgradescope.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Server resource name */}}
{{- define "upgradescope.serverFullname" -}}
{{- printf "%s-server" (include "upgradescope.fullname" .) -}}
{{- end -}}

{{/* Is the agent pushing to a server at all? Non-empty string = yes. */}}
{{- define "upgradescope.pushEnabled" -}}
{{- if or .Values.agent.serverUrl .Values.server.enabled -}}true{{- end -}}
{{- end -}}

{{/* Effective server URL for the agent */}}
{{- define "upgradescope.serverUrl" -}}
{{- if .Values.agent.serverUrl -}}
{{- .Values.agent.serverUrl -}}
{{- else -}}
{{- printf "http://%s.%s.svc:%d" (include "upgradescope.serverFullname" .) .Release.Namespace (int .Values.server.service.port) -}}
{{- end -}}
{{- end -}}

{{/* Secret holding the agent's push token */}}
{{- define "upgradescope.agentTokenSecretName" -}}
{{- if .Values.agent.existingSecret -}}
{{- .Values.agent.existingSecret -}}
{{- else -}}
{{- printf "%s-agent-token" (include "upgradescope.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* Secret holding the server's tokens */}}
{{- define "upgradescope.serverSecretName" -}}
{{- if .Values.server.existingSecret -}}
{{- .Values.server.existingSecret -}}
{{- else -}}
{{- printf "%s-tokens" (include "upgradescope.serverFullname" .) -}}
{{- end -}}
{{- end -}}
```

- [ ] Write `deploy/chart/templates/NOTES.txt`:

```
upgradescope {{ .Chart.AppVersion }} installed.

Watch readiness:
  kubectl get clusterreadiness {{ .Values.agent.crName }}
  kubectl get clusterreadiness {{ .Values.agent.crName }} -o jsonpath='{.status.targets[0].score}'
{{- if .Values.server.enabled }}

Server API (port-forward):
  kubectl -n {{ .Release.Namespace }} port-forward svc/{{ include "upgradescope.serverFullname" . }} 8080:{{ .Values.server.service.port }}
  curl http://127.0.0.1:8080/api/v1/clusters
{{- if not .Values.server.readToken }}

NOTE: the read API has no token (server.readToken is empty). Fine behind a
ClusterIP Service; set one before exposing it.
{{- end }}
{{- end }}

The agent is READ-ONLY except for the ClusterReadiness resource. For the
RBAC trade-offs (cluster-wide secret read for Helm detection), see the
rbac.create comment in values.yaml.

Uninstall: helm uninstall keeps the CRD (Helm crds/ semantics). Full removal:
  kubectl delete crd clusterreadinesses.upgradescope.dev
```

- [ ] Copy the CRD manifest (owned by section CRD-AGENT — never hand-edit the copy; `chart-test` enforces sync):

```
mkdir -p deploy/chart/crds
cp internal/crd/manifest.yaml deploy/chart/crds/clusterreadinesses.upgradescope.dev.yaml
```

- [ ] Write `hack/test-chart.sh` (initial harness: lint + CRD sync + default-render smoke; H3/H4 append assertion blocks at the marked points). `chmod +x hack/test-chart.sh`:

```bash
#!/usr/bin/env bash
# Chart contract tests: helm lint + rendered-template grep assertions.
# No cluster needed. Run: make chart-test
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$ROOT/deploy/chart"
command -v helm >/dev/null || { echo "ERROR: helm not found in PATH" >&2; exit 1; }

FAILED=0
fail() { echo "FAIL: $*" >&2; FAILED=1; }
pass() { echo "  ok: $*"; }
# Substring assertions (fixed string, safe for leading dashes).
assert_contains()     { grep -qF -- "$2" "$1" && pass "$3" || fail "$3 (missing: $2)"; }
assert_not_contains() { grep -qF -- "$2" "$1" && fail "$3 (unexpected: $2)" || pass "$3"; }
# Exact-line assertions ('kind: Service' must not match 'kind: ServiceAccount').
assert_line()    { grep -qxF -- "$2" "$1" && pass "$3" || fail "$3 (missing line: $2)"; }
assert_no_line() { grep -qxF -- "$2" "$1" && fail "$3 (unexpected line: $2)" || pass "$3"; }

echo "== helm lint"
helm lint "$CHART"

echo "== crds/ copy in sync with internal/crd/manifest.yaml"
if diff -u "$ROOT/internal/crd/manifest.yaml" "$CHART/crds/clusterreadinesses.upgradescope.dev.yaml"; then
  pass "CRD copy in sync"
else
  fail "CRD copy out of sync — run: cp internal/crd/manifest.yaml deploy/chart/crds/clusterreadinesses.upgradescope.dev.yaml"
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "== default render (agent only, CRD-only mode)"
helm template upgradescope "$CHART" --namespace upgradescope > "$TMP/default.yaml"

echo "== server-enabled render (combined single-cluster install)"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=test-token > "$TMP/server.yaml"

echo "== external-server render (agent pushes to remote, token from existing secret)"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set agent.serverUrl=https://uscope.example.com \
  --set agent.existingSecret=my-secret > "$TMP/external.yaml"

echo "== targets render (ClusterReadiness CR created by chart)"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set 'agent.targets={1.37,1.38}' > "$TMP/targets.yaml"

# --- AGENT ASSERTIONS (H3) ---

# --- SERVER ASSERTIONS (H4) ---

[ "$FAILED" -eq 0 ] || { echo "chart-test: FAILED" >&2; exit 1; }
echo "chart-test: all assertions passed"
```

- [ ] Append to `Makefile`:

```make
.PHONY: chart-test
chart-test:
	./hack/test-chart.sh
```

- [ ] Verify: `make chart-test` passes (lint + sync + the four renders succeed; no assertions yet beyond render exit codes).
- [ ] Commit:

```
git add deploy/chart hack/test-chart.sh Makefile
git commit -m "chart: scaffold upgradescope chart (Chart.yaml, values, helpers, CRD copy) + chart-test harness" \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task H3: agent templates — ServiceAccount, RBAC, token Secret, Deployment, CR

**Files:**
- `hack/test-chart.sh` (extend — assertions first)
- `deploy/chart/templates/serviceaccount.yaml` (new)
- `deploy/chart/templates/rbac.yaml` (new)
- `deploy/chart/templates/agent-secret.yaml` (new)
- `deploy/chart/templates/agent-deployment.yaml` (new)
- `deploy/chart/templates/clusterreadiness.yaml` (new)

- [ ] RED — replace the `# --- AGENT ASSERTIONS (H3) ---` marker in `hack/test-chart.sh` with:

```bash
# --- AGENT ASSERTIONS (H3) ---
echo "== agent assertions: default render"
assert_line "$TMP/default.yaml" 'kind: ServiceAccount'     "ServiceAccount rendered"
assert_line "$TMP/default.yaml" 'kind: ClusterRole'        "ClusterRole rendered"
assert_line "$TMP/default.yaml" 'kind: ClusterRoleBinding' "ClusterRoleBinding rendered"
assert_line "$TMP/default.yaml" 'kind: Deployment'         "agent Deployment rendered"
assert_contains "$TMP/default.yaml" 'verbs: ["get", "list", "watch"]'        "broad rule is read-only"
assert_contains "$TMP/default.yaml" 'nonResourceURLs: ["/metrics", "/version"]' "metrics+version nonResourceURLs"
assert_contains "$TMP/default.yaml" 'clusterreadinesses/status'              "status subresource rule"
assert_not_contains "$TMP/default.yaml" '"delete"'           "no delete verb anywhere"
assert_not_contains "$TMP/default.yaml" '"deletecollection"' "no deletecollection verb"
assert_not_contains "$TMP/default.yaml" '"escalate"'         "no escalate verb"
assert_contains "$TMP/default.yaml" 'image: "ghcr.io/abd-ulbasit/upgradescope:dev"' "default image ref"
assert_contains "$TMP/default.yaml" 'imagePullPolicy: IfNotPresent'   "pullPolicy IfNotPresent"
assert_contains "$TMP/default.yaml" 'runAsNonRoot: true'              "runAsNonRoot"
assert_contains "$TMP/default.yaml" 'readOnlyRootFilesystem: true'    "readOnlyRootFilesystem"
assert_contains "$TMP/default.yaml" 'allowPrivilegeEscalation: false' "no privilege escalation"
assert_contains "$TMP/default.yaml" 'drop: ["ALL"]'                   "all capabilities dropped"
assert_contains "$TMP/default.yaml" '--interval=10m'   "default interval flag"
assert_contains "$TMP/default.yaml" '--cr-name=cluster' "default cr-name flag"
assert_contains "$TMP/default.yaml" '--team-label=team' "default team-label flag"
assert_not_contains "$TMP/default.yaml" '--server-url' "CRD-only mode: no server-url flag"
assert_no_line "$TMP/default.yaml" 'kind: Secret'           "no token Secret in CRD-only mode"
assert_no_line "$TMP/default.yaml" 'kind: ClusterReadiness' "no CR without agent.targets"

echo "== agent assertions: external-server render"
assert_contains "$TMP/external.yaml" '--server-url=https://uscope.example.com' "explicit serverUrl wins"
assert_contains "$TMP/external.yaml" 'name: my-secret' "existingSecret referenced"
assert_no_line "$TMP/external.yaml" 'kind: Secret' "no generated Secret when existingSecret set"

echo "== agent assertions: targets render"
assert_line "$TMP/targets.yaml" 'kind: ClusterReadiness' "CR rendered when agent.targets set"
assert_contains "$TMP/targets.yaml" '- "1.37"' "target 1.37 in CR spec"

echo "== render must fail when pushing without any token"
if helm template upgradescope "$CHART" --set agent.serverUrl=https://x.example >/dev/null 2>&1; then
  fail "agent.serverUrl without a token should fail the required check"
else
  pass "push without token fails render"
fi
```

Run `make chart-test` — the new assertions fail (templates don't exist yet).

- [ ] Write `deploy/chart/templates/serviceaccount.yaml`:

```yaml
{{- if .Values.serviceAccount.create }}
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ include "upgradescope.serviceAccountName" . }}
  namespace: {{ .Release.Namespace }}
  labels: {{- include "upgradescope.labels" . | nindent 4 }}
{{- end }}
```

- [ ] Write `deploy/chart/templates/rbac.yaml` — the EXACT rules, with the consumer of every rule documented inline. Note `patch` on CRDs: `EnsureCRD` uses server-side apply, which is HTTP PATCH; `create/get/update` alone would 403:

```yaml
{{- if .Values.rbac.create }}
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ include "upgradescope.fullname" . }}-agent
  labels: {{- include "upgradescope.labels" . | nindent 4 }}
rules:
  # Cluster-wide READ-ONLY. Why a wildcard instead of an enumerated list
  # (nodes, namespaces, pods, secrets, ...):
  #   - versions collector: nodes (kubelet versions), /version
  #   - team attribution: namespaces (labels)
  #   - add-on detection: pods (container images)
  #   - Helm releases: secrets of type helm.sh/release.v1 — RBAC cannot
  #     filter secrets by type, so this is cluster-wide secret read; the
  #     trade-off is documented in values.yaml (rbac.create) and README
  #   - deprecated-API usage: metadata-only lists at KB-flagged deprecated
  #     group/versions — the group set is data-driven, enumerating it here
  #     would rot on every KB update
  - apiGroups: ["*"]
    resources: ["*"]
    verbs: ["get", "list", "watch"]
  # apiserver /metrics (apiserver_requested_deprecated_apis) and /version
  - nonResourceURLs: ["/metrics", "/version"]
    verbs: ["get"]
  # EnsureCRD: server-side apply (= PATCH) of the ClusterReadiness CRD
  - apiGroups: ["apiextensions.k8s.io"]
    resources: ["customresourcedefinitions"]
    verbs: ["create", "get", "update", "patch"]
  # The ONLY thing the agent ever writes: its own CR + status
  - apiGroups: ["upgradescope.dev"]
    resources: ["clusterreadinesses"]
    verbs: ["get", "list", "watch", "create", "update"]
  - apiGroups: ["upgradescope.dev"]
    resources: ["clusterreadinesses/status"]
    verbs: ["get", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{ include "upgradescope.fullname" . }}-agent
  labels: {{- include "upgradescope.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{ include "upgradescope.fullname" . }}-agent
subjects:
  - kind: ServiceAccount
    name: {{ include "upgradescope.serviceAccountName" . }}
    namespace: {{ .Release.Namespace }}
{{- end }}
```

- [ ] Write `deploy/chart/templates/agent-secret.yaml` (generated only when pushing and no existingSecret; token defaults to the in-chart server's ingest token):

```yaml
{{- if and (include "upgradescope.pushEnabled" .) (not .Values.agent.existingSecret) }}
apiVersion: v1
kind: Secret
metadata:
  name: {{ include "upgradescope.agentTokenSecretName" . }}
  namespace: {{ .Release.Namespace }}
  labels: {{- include "upgradescope.labels" . | nindent 4 }}
type: Opaque
stringData:
  serverToken: {{ required "a push token is required: set agent.serverToken, agent.existingSecret, or server.ingestToken" (default .Values.server.ingestToken .Values.agent.serverToken) | quote }}
{{- end }}
```

- [ ] Write `deploy/chart/templates/agent-deployment.yaml`. The token reaches the flag via `$(UPGRADESCOPE_SERVER_TOKEN)` expansion from a Secret-backed env var — it never appears in the pod spec:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "upgradescope.fullname" . }}-agent
  namespace: {{ .Release.Namespace }}
  labels: {{- include "upgradescope.labels" . | nindent 4 }}
    app.kubernetes.io/component: agent
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: {{ include "upgradescope.name" . }}
      app.kubernetes.io/instance: {{ .Release.Name }}
      app.kubernetes.io/component: agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: {{ include "upgradescope.name" . }}
        app.kubernetes.io/instance: {{ .Release.Name }}
        app.kubernetes.io/component: agent
    spec:
      serviceAccountName: {{ include "upgradescope.serviceAccountName" . }}
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: agent
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          args:
            - agent
            - --interval={{ .Values.agent.interval }}
            - --cr-name={{ .Values.agent.crName }}
            - --team-label={{ .Values.agent.teamLabel }}
            {{- if .Values.agent.clusterName }}
            - --cluster-name={{ .Values.agent.clusterName }}
            {{- end }}
            {{- if include "upgradescope.pushEnabled" . }}
            - --server-url={{ include "upgradescope.serverUrl" . }}
            - --server-token=$(UPGRADESCOPE_SERVER_TOKEN)
            {{- end }}
          {{- if include "upgradescope.pushEnabled" . }}
          env:
            - name: UPGRADESCOPE_SERVER_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ include "upgradescope.agentTokenSecretName" . }}
                  key: serverToken
          {{- end }}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          resources: {{- toYaml .Values.agent.resources | nindent 12 }}
```

- [ ] Write `deploy/chart/templates/clusterreadiness.yaml` (only when targets are pinned at install time; otherwise `EnsureObject` in the agent creates the CR with empty spec — no conflict, create-if-absent):

```yaml
{{- if .Values.agent.targets }}
apiVersion: upgradescope.dev/v1alpha1
kind: ClusterReadiness
metadata:
  name: {{ .Values.agent.crName }}
  labels: {{- include "upgradescope.labels" . | nindent 4 }}
spec:
  targets:
    {{- range .Values.agent.targets }}
    - {{ . | quote }}
    {{- end }}
{{- end }}
```

- [ ] GREEN — `make chart-test` passes all H3 assertions.
- [ ] Commit:

```
git add deploy/chart hack/test-chart.sh
git commit -m "chart: agent templates — SA, read-only ClusterRole, token Secret, Deployment, CR" \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task H4: server templates — Deployment, Service, PVC, Secret

**Files:**
- `hack/test-chart.sh` (extend — assertions first)
- `deploy/chart/templates/server-secret.yaml` (new)
- `deploy/chart/templates/server-pvc.yaml` (new)
- `deploy/chart/templates/server-service.yaml` (new)
- `deploy/chart/templates/server-deployment.yaml` (new)

- [ ] RED — replace the `# --- SERVER ASSERTIONS (H4) ---` marker in `hack/test-chart.sh` with:

```bash
# --- SERVER ASSERTIONS (H4) ---
echo "== server assertions: default render has no server objects"
assert_no_line "$TMP/default.yaml" 'kind: Service'               "no Service when server disabled"
assert_no_line "$TMP/default.yaml" 'kind: PersistentVolumeClaim' "no PVC when server disabled"

echo "== server assertions: server-enabled render"
assert_line "$TMP/server.yaml" 'kind: Service'               "server Service rendered"
assert_line "$TMP/server.yaml" 'kind: PersistentVolumeClaim' "server PVC rendered"
assert_line "$TMP/server.yaml" 'kind: Secret'                "Secrets rendered"
assert_contains "$TMP/server.yaml" 'name: upgradescope-server' "server resources named *-server"
assert_contains "$TMP/server.yaml" 'type: Recreate' "Recreate strategy (single SQLite writer)"
assert_contains "$TMP/server.yaml" '--db=/data/upgradescope.sqlite' "db on the data volume"
assert_contains "$TMP/server.yaml" '--ingest-token=$(UPGRADESCOPE_INGEST_TOKEN)' "ingest token via env expansion"
assert_contains "$TMP/server.yaml" 'ingestToken: "test-token"' "ingest token in Secret stringData"
assert_contains "$TMP/server.yaml" 'serverToken: "test-token"' "agent token defaults to ingestToken"
assert_contains "$TMP/server.yaml" '--server-url=http://upgradescope-server.upgradescope.svc:8080' "agent points at in-chart server"
assert_contains "$TMP/server.yaml" 'path: /healthz' "healthz probes"
assert_not_contains "$TMP/server.yaml" '--read-token' "no read-token flag unless set"

echo "== server assertions: render fails without ingest token"
if helm template upgradescope "$CHART" --set server.enabled=true >/dev/null 2>&1; then
  fail "server.enabled without ingestToken should fail the required check"
else
  pass "server without ingestToken fails render"
fi

echo "== server assertions: emptyDir when persistence disabled"
helm template upgradescope "$CHART" --namespace upgradescope \
  --set server.enabled=true --set server.ingestToken=t \
  --set server.persistence.enabled=false > "$TMP/nopvc.yaml"
assert_no_line "$TMP/nopvc.yaml" 'kind: PersistentVolumeClaim' "no PVC when persistence disabled"
assert_contains "$TMP/nopvc.yaml" 'emptyDir: {}' "emptyDir fallback"
```

Run `make chart-test` — new assertions fail.

- [ ] Write `deploy/chart/templates/server-secret.yaml`:

```yaml
{{- if and .Values.server.enabled (not .Values.server.existingSecret) }}
apiVersion: v1
kind: Secret
metadata:
  name: {{ include "upgradescope.serverSecretName" . }}
  namespace: {{ .Release.Namespace }}
  labels: {{- include "upgradescope.labels" . | nindent 4 }}
type: Opaque
stringData:
  ingestToken: {{ required "server.ingestToken is required when server.enabled=true (or set server.existingSecret)" .Values.server.ingestToken | quote }}
  {{- if .Values.server.readToken }}
  readToken: {{ .Values.server.readToken | quote }}
  {{- end }}
{{- end }}
```

- [ ] Write `deploy/chart/templates/server-pvc.yaml`:

```yaml
{{- if and .Values.server.enabled .Values.server.persistence.enabled }}
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: {{ include "upgradescope.serverFullname" . }}-data
  namespace: {{ .Release.Namespace }}
  labels: {{- include "upgradescope.labels" . | nindent 4 }}
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: {{ .Values.server.persistence.size }}
  {{- if .Values.server.persistence.storageClass }}
  storageClassName: {{ .Values.server.persistence.storageClass }}
  {{- end }}
{{- end }}
```

- [ ] Write `deploy/chart/templates/server-service.yaml`:

```yaml
{{- if .Values.server.enabled }}
apiVersion: v1
kind: Service
metadata:
  name: {{ include "upgradescope.serverFullname" . }}
  namespace: {{ .Release.Namespace }}
  labels: {{- include "upgradescope.labels" . | nindent 4 }}
    app.kubernetes.io/component: server
spec:
  type: {{ .Values.server.service.type }}
  ports:
    - name: http
      port: {{ .Values.server.service.port }}
      targetPort: http
  selector:
    app.kubernetes.io/name: {{ include "upgradescope.name" . }}
    app.kubernetes.io/instance: {{ .Release.Name }}
    app.kubernetes.io/component: server
{{- end }}
```

- [ ] Write `deploy/chart/templates/server-deployment.yaml`. `Recreate` strategy: SQLite must never have two writers during a rollout; root FS read-only, SQLite lives on the `/data` volume:

```yaml
{{- if .Values.server.enabled }}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "upgradescope.serverFullname" . }}
  namespace: {{ .Release.Namespace }}
  labels: {{- include "upgradescope.labels" . | nindent 4 }}
    app.kubernetes.io/component: server
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app.kubernetes.io/name: {{ include "upgradescope.name" . }}
      app.kubernetes.io/instance: {{ .Release.Name }}
      app.kubernetes.io/component: server
  template:
    metadata:
      labels:
        app.kubernetes.io/name: {{ include "upgradescope.name" . }}
        app.kubernetes.io/instance: {{ .Release.Name }}
        app.kubernetes.io/component: server
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: server
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          args:
            - serve
            - --listen=:{{ .Values.server.service.port }}
            - --db=/data/upgradescope.sqlite
            - --ingest-token=$(UPGRADESCOPE_INGEST_TOKEN)
            {{- if .Values.server.readToken }}
            - --read-token=$(UPGRADESCOPE_READ_TOKEN)
            {{- end }}
            {{- if .Values.server.slackWebhook }}
            - --slack-webhook={{ .Values.server.slackWebhook }}
            {{- end }}
            {{- if .Values.server.webhook }}
            - --webhook={{ .Values.server.webhook }}
            {{- end }}
            {{- if .Values.server.targets }}
            - --targets={{ join "," .Values.server.targets }}
            {{- end }}
          env:
            - name: UPGRADESCOPE_INGEST_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ include "upgradescope.serverSecretName" . }}
                  key: ingestToken
            {{- if .Values.server.readToken }}
            - name: UPGRADESCOPE_READ_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ include "upgradescope.serverSecretName" . }}
                  key: readToken
            {{- end }}
          ports:
            - name: http
              containerPort: {{ .Values.server.service.port }}
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 5
            periodSeconds: 15
          readinessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 2
            periodSeconds: 5
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          resources: {{- toYaml .Values.server.resources | nindent 12 }}
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          {{- if .Values.server.persistence.enabled }}
          persistentVolumeClaim:
            claimName: {{ include "upgradescope.serverFullname" . }}-data
          {{- else }}
          emptyDir: {}
          {{- end }}
{{- end }}
```

- [ ] GREEN — `make chart-test` passes everything (H3 + H4 assertion blocks).
- [ ] Commit:

```
git add deploy/chart hack/test-chart.sh
git commit -m "chart: optional server — Deployment (Recreate, SQLite on PVC), Service, Secret" \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task H5: chart README — install, RBAC trade-off, uninstall story

**Files:**
- `deploy/chart/README.md` (new)

- [ ] Write `deploy/chart/README.md`:

```markdown
# upgradescope Helm chart

Installs the upgradescope agent (continuous upgrade-readiness scanning,
results in a `ClusterReadiness` custom resource) and, optionally, the
upgradescope server (snapshot history, what-if API, Slack notifications).

## Install

Agent only (CRD-only mode — no server, results via `kubectl`):

    helm install upgradescope deploy/chart -n upgradescope --create-namespace
    kubectl get clusterreadiness cluster

Agent + server in one cluster:

    helm install upgradescope deploy/chart -n upgradescope --create-namespace \
      --set server.enabled=true \
      --set server.ingestToken=$(openssl rand -hex 16)

Agent pushing to an existing server elsewhere:

    helm install upgradescope deploy/chart -n upgradescope --create-namespace \
      --set agent.serverUrl=https://uscope.example.com \
      --set agent.existingSecret=my-push-token   # key: serverToken

## RBAC: what the agent can read, and why

The chart grants the agent **get/list/watch on all resources in all API
groups** — cluster-wide, strictly read-only. This is deliberate and worth
your scrutiny:

| Need | Why narrower RBAC doesn't work |
|---|---|
| Helm release detection (Secrets of type `helm.sh/release.v1`) | RBAC cannot filter Secrets by type, and release secret names are dynamic. **This means the agent can read every Secret in the cluster.** |
| Deprecated-API usage (metadata-only lists at deprecated group/versions) | The set of flagged group/versions comes from the knowledge base and changes with every KB update; an enumerated RBAC list would silently rot. |

If cluster-wide secret read is unacceptable, set `rbac.create=false` and
bind your own narrower ClusterRole. Affected collectors degrade to
"not assessed (reason)" in reports — nothing crashes (spec §9).

Write access is exactly one resource: the `ClusterReadiness` CR, its
status, and the CRD itself (`apiextensions` create/get/update/patch so the
agent can self-heal the CRD). The agent mutates nothing else, ever.

It also reads two non-resource URLs: `/metrics` (the
`apiserver_requested_deprecated_apis` metric — active deprecated-API
callers) and `/version`.

## Read API exposure

With `server.enabled=true` and `server.readToken` unset, the read API is
**unauthenticated** behind the ClusterIP Service. Set `server.readToken`
before exposing it via Ingress/LoadBalancer.

## Uninstall

    helm -n upgradescope uninstall upgradescope

Helm leaves CRDs in place by design (`crds/` semantics). Full removal:

    kubectl delete crd clusterreadinesses.upgradescope.dev

That deletes the CRD and any `ClusterReadiness` objects. Everything else
(Deployments, RBAC, Secrets, Service, PVC) is removed by `helm uninstall`;
the server PVC is deleted with the release because it is chart-managed.

## Values

See the commented `values.yaml` — every knob is documented there.
```

- [ ] Verify: `make chart-test` still green (README is not rendered, but lint re-runs).
- [ ] Commit:

```
git add deploy/chart/README.md
git commit -m "chart: README — install modes, honest RBAC trade-off, uninstall story" \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task H6: hack/demo/agent-e2e.sh — full chart e2e on the kind demo cluster

**Files:**
- `hack/demo/agent-e2e.sh` (new)
- `Makefile` (extend)

Prereqs: H1–H4 plus working `agent` and `serve` subcommands (CRD-AGENT,
SERVER-API, NOTIFY-CLI sections complete). Reuses the P1 demo cluster — the
script calls `kind-setup.sh` first, which is idempotent and installs the EOL
ingress-nginx that the assertions depend on.

- [ ] Write `hack/demo/agent-e2e.sh`, `chmod +x`:

```bash
#!/usr/bin/env bash
# End-to-end: build image -> kind load -> helm install (agent + server) ->
# assert ClusterReadiness status, the ingress-nginx blocker, and the server API.
#
# Reuses the P1 demo cluster (hack/demo/kind-setup.sh: kind 'upgradescope-demo'
# with EOL ingress-nginx chart 4.7.1 installed).
#
# Usage: ./hack/demo/agent-e2e.sh   (or: make agent-e2e)
# Teardown: helm -n upgradescope uninstall upgradescope
#           kubectl delete crd clusterreadinesses.upgradescope.dev
#           make demo-down
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
NS=upgradescope
RELEASE=upgradescope
TOKEN=dev-token
CR=cluster

for tool in docker kind helm kubectl curl; do
  command -v "$tool" >/dev/null || { echo "ERROR: $tool not found in PATH" >&2; exit 1; }
done

fail() { echo "FAIL: $*" >&2; exit 1; }

"$ROOT/hack/demo/kind-setup.sh"
make -C "$ROOT" kind-load

# interval=1m so the e2e doesn't wait 10 minutes for the second tick; the
# first tick fires immediately either way.
helm upgrade --install "$RELEASE" "$ROOT/deploy/chart" \
  --namespace "$NS" --create-namespace \
  --set server.enabled=true \
  --set server.ingestToken="$TOKEN" \
  --set agent.interval=1m \
  --wait --timeout 3m

kubectl -n "$NS" rollout status "deploy/$RELEASE-server" --timeout=120s
kubectl -n "$NS" rollout status "deploy/$RELEASE-agent" --timeout=120s

echo "== waiting for ClusterReadiness status (first agent tick)"
SCORE=""
for _ in $(seq 1 36); do
  SCORE=$(kubectl get clusterreadiness "$CR" -o jsonpath='{.status.targets[0].score}' 2>/dev/null || true)
  [ -n "$SCORE" ] && break
  sleep 5
done
if [ -z "$SCORE" ]; then
  kubectl -n "$NS" logs "deploy/$RELEASE-agent" --tail=50 >&2 || true
  fail "no status.targets[0].score on clusterreadiness/$CR after 180s"
fi
echo "PASS: clusterreadiness/$CR has score=$SCORE"

READY=$(kubectl get clusterreadiness "$CR" -o jsonpath='{.status.targets[0].ready}')
[ "$READY" = "false" ] || fail "targets[0].ready=$READY, want false (EOL ingress-nginx blocker expected)"
echo "PASS: ready=false (blocker present)"

kubectl get clusterreadiness "$CR" -o json | grep -q '"eol-addon"' \
  || fail "no eol-addon finding in clusterreadiness status"
echo "PASS: eol-addon (ingress-nginx) finding in CRD status"

echo "== checking server API via port-forward"
kubectl -n "$NS" port-forward "svc/$RELEASE-server" 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" 2>/dev/null || true' EXIT
sleep 3

BODY=""
for _ in $(seq 1 24); do
  BODY=$(curl -fsS http://127.0.0.1:18080/api/v1/clusters 2>/dev/null || true)
  echo "$BODY" | grep -q '"name"' && break
  sleep 5
done
echo "$BODY" | grep -q '"name"'  || fail "server /api/v1/clusters lists no cluster after 120s (push failing?): $BODY"
echo "$BODY" | grep -q '"score"' || fail "cluster summary has no score: $BODY"
echo "PASS: server lists the cluster: $BODY"

curl -fsS http://127.0.0.1:18080/healthz | grep -q '"ok"' || fail "/healthz not ok"
echo "PASS: /healthz ok"

echo
echo "agent-e2e: ALL PASS"
echo "Teardown: helm -n $NS uninstall $RELEASE && kubectl delete crd clusterreadinesses.upgradescope.dev"
```

- [ ] Append to `Makefile`:

```make
.PHONY: agent-e2e
agent-e2e:
	./hack/demo/agent-e2e.sh
```

- [ ] Verify: `make agent-e2e` ends with `agent-e2e: ALL PASS` (Colima + kind required). If the score wait times out, debug with `kubectl -n upgradescope logs deploy/upgradescope-agent` — the most likely causes are RBAC (check `kubectl auth can-i list secrets --as=system:serviceaccount:upgradescope:upgradescope`) and image pull (confirm `kind load` ran against `upgradescope-demo`).
- [ ] Commit:

```
git add hack/demo/agent-e2e.sh Makefile
git commit -m "e2e: agent-e2e.sh — chart install on kind, CRD status + server API assertions" \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task H7: gated Go integration test — in-process agent tick, CRD status via dynamic client

**Files:**
- `internal/cli/agent_integration_test.go` (new)

Mirrors `scan_integration_test.go`: same gate (`UPGRADESCOPE_IT=1`), same kind
cluster, name contains `Integration` so `make it` picks it up. Runs the agent
**in-process** (counted in coverage) in CRD-only mode with its own CR name
(`it-agent`) so it never collides with a chart-managed `cluster` object from H6.

- [ ] Write `internal/cli/agent_integration_test.go`:

```go
package cli

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abd-ulbasit/upgradescope/internal/agent"
	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/crd"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// TestAgentIntegration_CRDStatusOnKind runs the agent loop in-process against
// the kind demo cluster (hack/demo/kind-setup.sh) in CRD-only mode (no
// server), waits for the first tick, and asserts the ClusterReadiness status
// through the dynamic client: observed server version, KB version, a 0–100
// score, ready=false, and an eol-addon top finding (the EOL ingress-nginx).
//
// Run: ./hack/demo/kind-setup.sh && UPGRADESCOPE_IT=1 go test ./internal/cli/ -run Integration -v
func TestAgentIntegration_CRDStatusOnKind(t *testing.T) {
	if os.Getenv("UPGRADESCOPE_IT") != "1" {
		t.Skip("integration test: set UPGRADESCOPE_IT=1 to run (needs the kind cluster from hack/demo/kind-setup.sh)")
	}

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v\nIs the demo cluster up? Run: ./hack/demo/kind-setup.sh", err)
	}
	clients, err := collect.NewClients(cfg)
	if err != nil {
		t.Fatalf("build collect clients: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	apiext, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("apiextensions client: %v", err)
	}
	k, err := kb.Load()
	if err != nil {
		t.Fatalf("kb.Load: %v", err)
	}

	const crName = "it-agent" // distinct from the chart-managed "cluster"
	gvr := schema.GroupVersionResource{Group: crd.Group, Version: crd.Version, Resource: crd.Plural}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		_ = dyn.Resource(gvr).Delete(cctx, crName, metav1.DeleteOptions{})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- agent.Run(ctx, clients, dyn, apiext, k, agent.Config{
			Interval: time.Minute, // first tick fires immediately; we cancel after it
			CRName:   crName,      // ServerURL empty: CRD-only mode
		})
	}()

	// Poll until the first tick lands status.targets.
	var u *unstructured.Unstructured
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for ClusterReadiness status.targets after first agent tick")
		}
		got, gerr := dyn.Resource(gvr).Get(ctx, crName, metav1.GetOptions{})
		if gerr == nil {
			if targets, found, _ := unstructured.NestedSlice(got.Object, "status", "targets"); found && len(targets) > 0 {
				u = got
				break
			}
		}
		select {
		case e := <-errCh:
			t.Fatalf("agent.Run exited before producing status: %v", e)
		case <-time.After(2 * time.Second):
		}
	}
	cancel() // graceful stop
	if e := <-errCh; e != nil && !errors.Is(e, context.Canceled) {
		t.Errorf("agent.Run on cancel = %v, want nil or context.Canceled", e)
	}

	if v, _, _ := unstructured.NestedString(u.Object, "status", "observedServerVersion"); v == "" {
		t.Error("status.observedServerVersion is empty")
	}
	if v, _, _ := unstructured.NestedString(u.Object, "status", "kbVersion"); v == "" {
		t.Error("status.kbVersion is empty")
	}

	targets, _, _ := unstructured.NestedSlice(u.Object, "status", "targets")
	first, ok := targets[0].(map[string]any)
	if !ok {
		t.Fatalf("status.targets[0] is %T, want object", targets[0])
	}
	score, found, err := unstructured.NestedInt64(first, "score")
	if err != nil || !found {
		t.Fatalf("targets[0].score missing (found=%v err=%v): %v", found, err, first)
	}
	if score < 0 || score > 100 {
		t.Errorf("targets[0].score = %d, want 0..100", score)
	}
	if ready, _, _ := unstructured.NestedBool(first, "ready"); ready {
		t.Error("targets[0].ready = true, want false (EOL ingress-nginx blocker expected)")
	}
	if blockers, _, _ := unstructured.NestedInt64(first, "blockers"); blockers < 1 {
		t.Errorf("targets[0].blockers = %d, want >= 1", blockers)
	}

	foundEOL := false
	tfs, _, _ := unstructured.NestedSlice(first, "topFindings")
	for _, tf := range tfs {
		if m, ok := tf.(map[string]any); ok && m["category"] == "eol-addon" {
			foundEOL = true
			t.Logf("eol-addon top finding: %v — %v", m["title"], m["remediation"])
		}
	}
	if !foundEOL {
		t.Errorf("no eol-addon category in topFindings: %v", tfs)
	}
	t.Logf("ClusterReadiness/%s: target=%v score=%d", crName, first["target"], score)
}
```

- [ ] Verify the gate: `go test ./internal/cli/ -run AgentIntegration -v` (without the env var) reports SKIP; `go vet ./internal/cli/` is clean.
- [ ] Verify for real: `./hack/demo/kind-setup.sh && UPGRADESCOPE_IT=1 go test ./internal/cli/ -run Integration -v` — both the scan and agent integration tests pass. (This is the same invocation as `make it`.)
- [ ] Commit:

```
git add internal/cli/agent_integration_test.go
git commit -m "test: gated agent integration test — in-process tick, CRD status via dynamic client" \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Section exit criteria

- `make docker-build` produces `ghcr.io/abd-ulbasit/upgradescope:dev` (distroless, nonroot, single static binary).
- `make chart-test` green: helm lint, CRD-copy sync with `internal/crd/manifest.yaml`, and every grep assertion across the four renders (default / server-enabled / external-server / targets), including the negative renders (push without token, server without ingest token both fail).
- `make agent-e2e` green on the kind demo cluster: chart installs, CRD status carries a score, `ready=false` with an `eol-addon` finding, and the server lists the pushed cluster.
- `make it` runs both gated integration tests (scan + agent) against the same cluster.
- The RBAC story is documented in three places that say the same true thing: `values.yaml` (rbac.create comment), `deploy/chart/README.md`, and inline rule comments in `rbac.yaml`.

