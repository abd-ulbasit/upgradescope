package server

import (
	"bufio"
	"bytes"
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
	"sync"
	"time"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// DefaultMaxSnapshotBytes caps the snapshot push body when
// Config.MaxSnapshotBytes is unset — enforced on the wire bytes AND on the
// decompressed stream (gzip-bomb guard).
const DefaultMaxSnapshotBytes = 20 << 20 // 20 MiB

// DefaultMaxGateBytes caps the /gate manifest stream when
// Config.MaxGateBytes is unset. Lower than the snapshot cap: the gate body
// is buffered whole and is reachable with only the read token (or none).
const DefaultMaxGateBytes = 10 << 20 // 10 MiB

// Manifest stream shape limits for /gate, checked on the raw bytes before
// any YAML is decoded. Decoding a document builds a generic tree and then
// JSON — measured at 25-50x the document's size in heap — so the
// per-document cap, not the body cap, is what bounds one request's memory.
// 4 MiB fits any single Kubernetes object (etcd stores at most ~1.5 MiB)
// and a multi-MiB `kubectl get -o yaml` List. The document count bounds
// CPU on streams of many tiny documents.
const (
	maxManifestDocBytes = 4 << 20
	maxManifestDocs     = 20000
)

// sizeString renders a byte limit for error messages: "20MiB" when it is
// a whole number of MiB, otherwise "1024 bytes".
func sizeString(n int64) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return fmt.Sprintf("%dMiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

func (s *Server) maxSnapshotBytes() int64 {
	if s.cfg.MaxSnapshotBytes > 0 {
		return s.cfg.MaxSnapshotBytes
	}
	return DefaultMaxSnapshotBytes
}

func (s *Server) maxGateBytes() int64 {
	if s.cfg.MaxGateBytes > 0 {
		return s.cfg.MaxGateBytes
	}
	return DefaultMaxGateBytes
}

// byteBudget caps the /gate body bytes held in memory across all requests.
// It never blocks: a charge either fits now or is refused.
type byteBudget struct {
	mu   sync.Mutex
	used int64
	max  int64
}

func newByteBudget(limit int64) *byteBudget {
	return &byteBudget{max: limit}
}

// charge adds n bytes to a request that already holds held. If they do
// not fit, it gives back held in the same step and reports false, so a
// refused request's bytes never count against another request's charge.
func (b *byteBudget) charge(held, n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.max {
		b.used -= held
		return false
	}
	b.used += n
	return true
}

func (b *byteBudget) give(n int64) {
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
}

func (b *byteBudget) inUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// /gate bodies are read into chunks: the first is minGateChunk bytes and
// each next one is as large as everything received so far, up to
// maxGateChunk. A client that declares a large Content-Length and then
// sends little has little allocated, and a large body costs no copying.
const (
	minGateChunk = 512
	maxGateChunk = 64 << 10
)

// gateBody is a buffered /gate request body, in the chunks it was read in.
type gateBody [][]byte

// reader returns a fresh reader over the whole body.
func (b gateBody) reader() io.Reader {
	rs := make([]io.Reader, len(b))
	for i, c := range b {
		rs[i] = bytes.NewReader(c)
	}
	return io.MultiReader(rs...)
}

// readManifestBody reads a /gate manifest stream under the body cap and the
// shared buffered-body budget, then splits it (the same kubectl-compatible
// splitter collect uses) to check the document count and each document's
// size before anything decodes it. It writes the 413/422/503 itself.
//
// Each read is charged to the budget for exactly the bytes it returned,
// as they arrive. A declared Content-Length reserves nothing (it only
// bounds the reads, and one over the cap is refused before any are made),
// so a client that stalls mid-upload holds only what it has sent, until
// ReadTimeout ends its request. When a charge does not fit, the request
// gives back everything it holds, in the same step, and gets 503 +
// Retry-After immediately. Nothing waits for budget, let alone while
// holding some, so concurrent uploads cannot deadlock or queue behind a
// stalled one. On success the caller must call release once it no longer
// needs the body; release is idempotent. On failure everything has
// already been given back.
func (s *Server) readManifestBody(w http.ResponseWriter, r *http.Request) (body gateBody, release func(), ok bool) {
	limit := s.maxGateBytes()
	tooLarge := "manifest stream exceeds the " + sizeString(limit) + " limit"
	if r.ContentLength > limit {
		errJSON(w, http.StatusRequestEntityTooLarge, tooLarge)
		return nil, nil, false
	}

	var held int64 // bytes received and charged so far
	release = sync.OnceFunc(func() { s.gateBuffered.give(held) })
	src := http.MaxBytesReader(w, r.Body, limit)
	for {
		if len(body) == 0 || len(body[len(body)-1]) == cap(body[len(body)-1]) {
			size := min(max(held, minGateChunk), maxGateChunk)
			if r.ContentLength >= 0 {
				size = min(size, r.ContentLength-held)
			}
			if size == 0 {
				break // the whole declared Content-Length is in
			}
			body = append(body, make([]byte, 0, size))
		}
		chunk := body[len(body)-1]
		n, err := src.Read(chunk[len(chunk):cap(chunk)])
		if n > 0 {
			if !s.gateBuffered.charge(held, int64(n)) { // gives back held too
				w.Header().Set("Retry-After", "10")
				errJSON(w, http.StatusServiceUnavailable, "too many concurrent gate requests; retry shortly")
				return nil, nil, false
			}
			held += int64(n)
			body[len(body)-1] = chunk[:len(chunk)+n]
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			release()
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				errJSON(w, http.StatusRequestEntityTooLarge, tooLarge)
				return nil, nil, false
			}
			errJSON(w, http.StatusUnprocessableEntity, "reading body: "+err.Error())
			return nil, nil, false
		}
	}

	docs := utilyaml.NewYAMLReader(bufio.NewReader(body.reader()))
	for n := 1; ; n++ {
		doc, err := docs.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			release()
			errJSON(w, http.StatusUnprocessableEntity, "invalid manifest stream: "+err.Error())
			return nil, nil, false
		}
		if n > maxManifestDocs {
			release()
			errJSON(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("manifest stream has more than %d documents", maxManifestDocs))
			return nil, nil, false
		}
		if len(doc) > maxManifestDocBytes {
			release()
			errJSON(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("manifest document %d is %d bytes, over the %s per-document limit (split large Lists into separate documents)",
					n, len(doc), sizeString(maxManifestDocBytes)))
			return nil, nil, false
		}
	}
	return body, release, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// internalErr logs the real error server-side and returns a fixed 500 body:
// store/internal error text (paths, DSNs, SQL) must never reach clients.
// 4xx responses keep their detail — that's useful to agents and operators.
func internalErr(w http.ResponseWriter, what string, err error) {
	log.Printf("server: %s: %v", what, err)
	errJSON(w, http.StatusInternalServerError, "internal error")
}

// bearerToken extracts the raw "Authorization: Bearer <token>" value, ""
// when the header is absent or malformed.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.HasPrefix(h, prefix) {
		return ""
	}
	return h[len(prefix):]
}

// bearerOK does a constant-time check of "Authorization: Bearer <token>".
func bearerOK(r *http.Request, token string) bool {
	presented := bearerToken(r)
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

// authIngest authorizes a snapshot push. Two token kinds are accepted:
// the optional shared Config.IngestToken (fleet-wide; single-cluster/dev
// setups — when it is "" only per-cluster tokens work) and per-cluster
// tokens minted via `upgradescope tokens create` (P3, spec §8) — those
// return the cluster name the token is bound to.
// boundCluster == "" means the push may target any cluster. The HTTP error
// (401 / 500) is written here; the cluster-match check happens in
// handleIngest once the body names its cluster (403 there).
func (s *Server) authIngest(w http.ResponseWriter, r *http.Request) (boundCluster string, ok bool) {
	token := bearerToken(r)
	if token != "" && s.cfg.IngestToken != "" &&
		subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.IngestToken)) == 1 {
		return "", true
	}
	if token == "" {
		errJSON(w, http.StatusUnauthorized, "invalid or missing bearer token")
		return "", false
	}
	name, valid, err := s.cfg.Store.ValidToken(r.Context(), token)
	if err != nil {
		internalErr(w, "validating ingest token", err)
		return "", false
	}
	if !valid {
		errJSON(w, http.StatusUnauthorized, "invalid or missing bearer token")
		return "", false
	}
	return name, true
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
// identity body, schema validation, cluster upsert (409 on a cluster UID
// conflict), then ingestSnapshot: every target evaluated and committed
// with the snapshot in one transaction (202), or — for a duplicate of the
// latest snapshot — stale evaluations refreshed (200 duplicate).
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	boundCluster, ok := s.authIngest(w, r)
	if !ok {
		return
	}
	limit := s.maxSnapshotBytes()
	body := http.MaxBytesReader(w, r.Body, limit)
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
		reader = io.LimitReader(gz, limit+1)
	default:
		errJSON(w, http.StatusUnsupportedMediaType, fmt.Sprintf("unsupported Content-Encoding %q (use gzip or identity)", enc))
		return
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			errJSON(w, http.StatusRequestEntityTooLarge, "snapshot exceeds the "+sizeString(limit)+" limit")
			return
		}
		errJSON(w, http.StatusUnprocessableEntity, "reading body: "+err.Error())
		return
	}
	if int64(len(raw)) > limit {
		errJSON(w, http.StatusRequestEntityTooLarge, "snapshot exceeds the "+sizeString(limit)+" limit after decompression")
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
	// Per-cluster tokens authenticate exactly one cluster. 403 (not 401):
	// the token is genuine, the target cluster is what's wrong. Checked
	// before any store write so a mismatched push registers nothing.
	if boundCluster != "" && boundCluster != req.ClusterName {
		errJSON(w, http.StatusForbidden,
			fmt.Sprintf("token is not valid for cluster %q", req.ClusterName))
		return
	}
	inv, msg := decodePushedInventory(req.Inventory)
	if msg != "" {
		errJSON(w, http.StatusUnprocessableEntity, msg)
		return
	}
	// The dedup hash is over a canonical form: the parsed inventory
	// re-marshaled, so wire key order and whitespace never change it.
	// Struct fields marshal in declared order; map keys marshal sorted.
	// CollectedAt is zeroed to match the agent's snapshotHash canonical
	// form (it changes every tick; hashing it would make force-sync pushes
	// never dedup to 200 duplicate). The snapshot itself stores the
	// inventory as pushed, so collectedAt and fields this server does not
	// know (a newer agent's) are kept for a server that does; a push that
	// differs only in those is a duplicate, since nothing judged changed.
	hashed := inv
	hashed.CollectedAt = time.Time{}
	canonical, err := json.Marshal(hashed)
	if err != nil {
		internalErr(w, "canonicalizing inventory", err)
		return
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(canonical))

	ctx := r.Context()
	now := s.now()
	// The cluster is registered (or touched) in the same transaction as the
	// snapshot, so a push that fails to commit leaves no cluster row and no
	// last-seen bump. The lookup here only decides early: a UID conflict
	// needs no evaluation, and a known cluster's latest snapshot decides
	// whether this push is a duplicate.
	cluster := store.Cluster{Name: req.ClusterName, ClusterUID: inv.ClusterID, LastSeen: now}
	existing, err := s.cfg.Store.ClusterByName(ctx, req.ClusterName)
	switch {
	case err == nil:
		if existing.ClusterUID != "" && existing.ClusterUID != inv.ClusterID {
			writeUIDConflict(w, &store.ClusterUIDConflictError{Name: req.ClusterName, StoredUID: existing.ClusterUID, PushedUID: inv.ClusterID})
			return
		}
		cluster.ID = existing.ID
	case !errors.Is(err, store.ErrNotFound):
		internalErr(w, "loading cluster", err)
		return
	}
	// Detached from the request context: once the agent has sent the body,
	// its disconnecting must not abort the commit (it would retry and get a
	// duplicate); the transaction keeps the write all-or-nothing either way.
	snapID, duplicate, err := s.ingestSnapshot(context.WithoutCancel(ctx), cluster, store.Snapshot{
		ClusterID:     cluster.ID,
		Hash:          hash,
		KBVersion:     req.KBVersion,
		AgentVersion:  req.AgentVersion,
		ReceivedAt:    now,
		ServerVersion: inv.ServerVersion, // "" (degraded): ingestSnapshot inherits the last one
		Inventory:     req.Inventory,
	}, legacyView(inv, req.AgentVersion))
	var conflict *store.ClusterUIDConflictError
	if errors.As(err, &conflict) { // another push bound the name meanwhile
		writeUIDConflict(w, conflict)
		return
	}
	if err != nil {
		internalErr(w, "storing snapshot and evaluations", err)
		return
	}
	if duplicate {
		writeJSON(w, http.StatusOK, map[string]any{"snapshotId": snapID, "duplicate": true})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"snapshotId": snapID})
}

// supportedInventorySchema is the inventory.schemaVersion this server
// judges. Another version means fields this server would misread.
const supportedInventorySchema = 1

// decodePushedInventory parses and checks a pushed inventory, returning a
// 422 message for one the server cannot judge: absent or null, another
// schemaVersion (which includes {} and a missing one), or a serverVersion
// that is not a Kubernetes 1.x version. A degraded inventory with no
// serverVersion at all (the versions collector failed) is accepted and
// judged at the cluster's last reported version (ingestSnapshot).
func decodePushedInventory(raw json.RawMessage) (inventory.Inventory, string) {
	var inv inventory.Inventory
	if t := bytes.TrimSpace(raw); len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return inv, "inventory is required"
	}
	if err := json.Unmarshal(raw, &inv); err != nil {
		return inv, "invalid inventory: " + err.Error()
	}
	if inv.SchemaVersion != supportedInventorySchema {
		return inv, fmt.Sprintf("unsupported inventory schemaVersion %d (want %d)", inv.SchemaVersion, supportedInventorySchema)
	}
	if inv.ServerVersion != "" {
		if _, err := inventory.ParseTarget(inv.ServerVersion); err != nil {
			return inv, "invalid inventory serverVersion: " + err.Error()
		}
	}
	return inv, ""
}

// writeUIDConflict answers a push whose clusterId does not match the one
// its cluster name is bound to.
func writeUIDConflict(w http.ResponseWriter, conflict *store.ClusterUIDConflictError) {
	// Two clusters reporting one name would interleave their snapshots in
	// one history and flap every score and alert, so the second one is
	// refused until an operator decides which cluster the name means. A
	// push without a clusterId is refused the same way: it cannot show
	// that it is the cluster the name is bound to.
	if conflict.PushedUID == "" {
		errJSON(w, http.StatusConflict, fmt.Sprintf(
			"cluster name %q is registered to clusterId %s, but this push carries no clusterId "+
				"(the agent could not read the kube-system namespace, which needs get on namespaces). "+
				"Fix the agent's access, or give it a distinct --cluster-name if it is another cluster",
			conflict.Name, conflict.StoredUID))
		return
	}
	errJSON(w, http.StatusConflict, fmt.Sprintf(
		"cluster name %q is registered to clusterId %s, but this push comes from clusterId %s. "+
			"If this is a different cluster, give its agent a distinct --cluster-name (chart value clusterName). "+
			"If the cluster was rebuilt, remove the old record (and its history) with "+
			"'upgradescope clusters delete %s', then push again; the delete also removes the name's "+
			"per-cluster ingest tokens, so mint a new one with 'upgradescope tokens create %s' if the agent used one",
		conflict.Name, conflict.StoredUID, conflict.PushedUID, conflict.Name, conflict.Name))
}

// ----- read API -----

// readAuth gates a read handler behind Config.ReadToken when configured;
// an empty ReadToken leaves the read API open (the CLI documents this loudly).
// The admin token reads too, so one credential can list and then delete.
func (s *Server) readAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.ReadToken != "" && !bearerOK(r, s.cfg.ReadToken) &&
			(s.cfg.AdminToken == "" || !bearerOK(r, s.cfg.AdminToken)) {
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
		internalErr(w, "loading cluster", err)
		return store.Cluster{}, false
	}
	return c, true
}

// defaultTarget computes a cluster's default evaluation target (next minor
// above the version its latest snapshot is judged at — judgedVersion) and
// returns the parsed latest inventory alongside so callers don't
// unmarshal twice. Errors: store.ErrNotFound (no snapshots) or a
// corrupt/unparseable-version error.
func (s *Server) defaultTarget(ctx context.Context, clusterID int64) (inventory.Version, inventory.Inventory, error) {
	snap, inv, err := s.latestInventory(ctx, clusterID)
	if err != nil {
		return inventory.Version{}, inventory.Inventory{}, err
	}
	server, err := inventory.ParseVersion(judgedAt(snap, inv))
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
		v, err := inventory.ParseTarget(q)
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

// errCorruptInventory marks a stored snapshot whose inventory JSON does not
// decode.
var errCorruptInventory = errors.New("stored inventory is corrupt")

// latestInventory loads and decodes the cluster's latest snapshot, as this
// server judges it (legacyView of the pushing agent's version).
// store.ErrNotFound means the cluster has no snapshots.
func (s *Server) latestInventory(ctx context.Context, clusterID int64) (store.Snapshot, inventory.Inventory, error) {
	snap, err := s.cfg.Store.LatestSnapshot(ctx, clusterID)
	if err != nil {
		return store.Snapshot{}, inventory.Inventory{}, err
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(snap.Inventory, &inv); err != nil {
		return store.Snapshot{}, inventory.Inventory{}, fmt.Errorf("cluster %d (snapshot %d): %w: %v", clusterID, snap.ID, errCorruptInventory, err)
	}
	return snap, legacyView(inv, snap.AgentVersion), nil
}

// evalSummary is the read API's compact evaluation view. Evaluations are
// always the cluster's current ones (of its latest snapshot).
type evalSummary struct {
	Target      string         `json:"target"`
	Score       int            `json:"score"`
	Ready       bool           `json:"ready"`
	Verdict     engine.Verdict `json:"verdict"`
	Blockers    int            `json:"blockers"`
	Warnings    int            `json:"warnings"`
	KBVersion   string         `json:"kbVersion"`
	EvaluatedAt time.Time      `json:"evaluatedAt"` // last confirmed; a re-evaluation with an unchanged result moves it
	SnapshotID  int64          `json:"snapshotId"`
	Outdated    bool           `json:"outdated,omitempty"` // evaluated before today UTC or under another KB or team map; the next pass replaces it
}

func (s *Server) summarize(e store.Evaluation, now time.Time) evalSummary {
	return evalSummary{
		Target:      e.Target,
		Score:       e.Score,
		Ready:       e.Ready,
		Verdict:     verdictOf(e),
		Blockers:    e.Blockers,
		Warnings:    e.Warnings,
		KBVersion:   e.KBVersion,
		EvaluatedAt: e.EvaluatedAt,
		SnapshotID:  e.SnapshotID,
		Outdated:    s.outdated(e, now),
	}
}

type clusterSummary struct {
	store.Cluster
	Stale  bool         `json:"stale"`            // no push within the server's --stale-after
	Latest *evalSummary `json:"latest,omitempty"` // default-target evaluation, if any
}

// handleListClusters: GET /api/v1/clusters — every cluster plus its current
// default-target score summary (omitted when no snapshot/evaluation exists).
// Snapshot heads come from one store call (clusterStates), so no inventory
// is decoded; the summaries are still one CurrentEvaluation per cluster.
func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	states, err := s.clusterStates(ctx)
	if err != nil {
		internalErr(w, "listing clusters", err)
		return
	}
	out := make([]clusterSummary, 0, len(states))
	now := s.now()
	for _, c := range states {
		cs := clusterSummary{Cluster: c.Cluster, Stale: s.clusterStale(c.Cluster, now)}
		if server, err := inventory.ParseVersion(c.version); c.hasSnapshot && err == nil {
			target := server.Next()
			if e, err := s.cfg.Store.CurrentEvaluation(ctx, c.ID, target.String()); err == nil {
				sum := s.summarize(e, now)
				cs.Latest = &sum
			}
		}
		out = append(out, cs)
	}
	writeJSON(w, http.StatusOK, out)
}

type clusterDetail struct {
	store.Cluster
	Stale         bool                                                `json:"stale"` // no push within the server's --stale-after
	ServerVersion string                                              `json:"serverVersion,omitempty"`
	Capabilities  map[inventory.Capability]inventory.CapabilityStatus `json:"capabilities,omitempty"`
	Evaluations   []evalSummary                                       `json:"evaluations"`
}

// handleGetCluster: GET /api/v1/clusters/{id} — cluster row, the latest
// snapshot's server version and capability map, and the current evaluation
// summaries for the default target plus every applicable extra target.
func (s *Server) handleGetCluster(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireCluster(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	now := s.now()
	detail := clusterDetail{Cluster: c, Stale: s.clusterStale(c, now), Evaluations: []evalSummary{}}
	var targets []inventory.Version
	if snap, inv, err := s.latestInventory(ctx, c.ID); err == nil {
		detail.ServerVersion = judgedAt(snap, inv)
		detail.Capabilities = inv.Capabilities
		targets = s.evalTargets(detail.ServerVersion)
	} else {
		targets = s.extraTargets
	}
	for _, t := range targets {
		if e, err := s.cfg.Store.CurrentEvaluation(ctx, c.ID, t.String()); err == nil {
			detail.Evaluations = append(detail.Evaluations, s.summarize(e, now))
		}
	}
	writeJSON(w, http.StatusOK, detail)
}

// Report sources: a stored evaluation of the latest snapshot, or a what-if
// computed on request from the latest snapshot.
const (
	sourceStored = "stored"
	sourceWhatIf = "what-if"
)

// reportMeta says what a served report is: which snapshot, when it was
// evaluated, whether it was stored or computed, and whether the target is
// one the cluster already runs.
type reportMeta struct {
	EvaluatedAt   time.Time `json:"evaluatedAt"`
	SnapshotID    int64     `json:"snapshotId"`
	Source        string    `json:"source"`                  // sourceStored | sourceWhatIf
	ServerVersion string    `json:"serverVersion,omitempty"` // the version the latest snapshot is judged at (judgedVersion)
	NotApplicable bool      `json:"notApplicable,omitempty"` // target at or below ServerVersion
	Outdated      bool      `json:"outdated,omitempty"`      // a stored evaluation the next pass replaces (evalSummary.Outdated)
}

// loadOrComputeReport returns the current stored evaluation's report for
// (cluster, target) when one exists, else a what-if computed from the
// latest snapshot. Only a missing evaluation (store.ErrNotFound) falls
// through to the what-if path — any other store failure is returned, never
// masked by a recompute that would hide a broken store behind a 200.
// A store.ErrNotFound result means the cluster has no snapshots at all.
func (s *Server) loadOrComputeReport(ctx context.Context, clusterID int64, target inventory.Version) (engine.Report, reportMeta, error) {
	snap, inv, err := s.latestInventory(ctx, clusterID)
	if err != nil {
		return engine.Report{}, reportMeta{}, err
	}
	version := judgedAt(snap, inv)
	meta := reportMeta{ServerVersion: version, NotApplicable: notApplicable(version, target)}
	e, err := s.cfg.Store.CurrentEvaluation(ctx, clusterID, target.String())
	switch {
	case err == nil:
		var rep engine.Report
		if err := json.Unmarshal(e.Report, &rep); err != nil {
			return engine.Report{}, reportMeta{}, fmt.Errorf("stored report for evaluation %d is corrupt: %w", e.ID, err)
		}
		meta.EvaluatedAt, meta.SnapshotID, meta.Source = e.EvaluatedAt, e.SnapshotID, sourceStored
		meta.Outdated = s.outdated(e, s.now())
		return rep, meta, nil
	case errors.Is(err, store.ErrNotFound):
		now := s.now()
		meta.EvaluatedAt, meta.SnapshotID, meta.Source = now, snap.ID, sourceWhatIf
		return evaluateWhatIf(inv, s.cfg.KB, s.cfg.TeamMap, target, now), meta, nil
	default:
		return engine.Report{}, reportMeta{}, fmt.Errorf("loading current evaluation: %w", err)
	}
}

// reportForRequest is the shared resolve-cluster → resolve-target → load/
// compute pipeline behind the report, findings and teams endpoints.
func (s *Server) reportForRequest(w http.ResponseWriter, r *http.Request) (engine.Report, reportMeta, bool) {
	c, ok := s.requireCluster(w, r)
	if !ok {
		return engine.Report{}, reportMeta{}, false
	}
	target, ok := s.resolveTarget(w, r, c.ID)
	if !ok {
		return engine.Report{}, reportMeta{}, false
	}
	rep, meta, err := s.loadOrComputeReport(r.Context(), c.ID, target)
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "no snapshots for cluster")
		return engine.Report{}, reportMeta{}, false
	}
	if err != nil {
		internalErr(w, "loading or computing report", err)
		return engine.Report{}, reportMeta{}, false
	}
	return rep, meta, true
}

// reportResponse is the report endpoint's body: the engine report, per-team
// scores, and what the report is (reportMeta).
type reportResponse struct {
	reportWithTeams
	reportMeta
}

// handleReport: GET /api/v1/clusters/{id}/report?target= — full engine.Report
// plus presentation-time per-team scores (`teams`, omitted when empty) and
// evaluatedAt/snapshotId/source.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	rep, meta, ok := s.reportForRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, reportResponse{withTeams(rep), meta})
}

// handleFindings: GET /api/v1/clusters/{id}/findings?target=&severity=&category=
// — the report's findings, exact-match filtered. Unknown filter values simply
// match nothing.
func (s *Server) handleFindings(w http.ResponseWriter, r *http.Request) {
	rep, meta, ok := s.reportForRequest(w, r)
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
	writeJSON(w, http.StatusOK, struct {
		Target   string           `json:"target"`
		Findings []engine.Finding `json:"findings"`
		reportMeta
	}{rep.Target.String(), findings, meta})
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
		internalErr(w, "loading history", err)
		return
	}
	if points == nil {
		points = []store.ScorePoint{}
	}
	writeJSON(w, http.StatusOK, points)
}
