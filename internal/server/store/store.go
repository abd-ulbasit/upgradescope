// Package store persists clusters, snapshots and evaluations for the
// upgradescope server. Store is the seam P3's Postgres implementation
// fills in; P2 ships the SQLite implementation in this package.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// HashToken is the storage form of an ingest token: lowercase hex sha256.
// Exported so callers (CLI, tests) can locate rows without the plaintext.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ErrNotFound is returned (possibly wrapped) by lookups that match no row.
// Test with errors.Is.
var ErrNotFound = errors.New("store: not found")

// ErrClusterUIDConflict is returned (as a *ClusterUIDConflictError) by
// UpsertCluster when the name is already bound to a different cluster UID.
// Test with errors.Is.
var ErrClusterUIDConflict = errors.New("store: cluster name is bound to another cluster UID")

// ClusterUIDConflictError carries both UIDs so callers can explain the
// conflict. Is(ErrClusterUIDConflict) holds.
type ClusterUIDConflictError struct {
	Name      string
	StoredUID string
	PushedUID string
}

func (e *ClusterUIDConflictError) Error() string {
	return fmt.Sprintf("cluster %q is registered with cluster UID %q, not %q", e.Name, e.StoredUID, e.PushedUID)
}

func (e *ClusterUIDConflictError) Is(target error) bool { return target == ErrClusterUIDConflict }

// Store is the persistence contract. SQLite implements it in P2; P3 adds
// Postgres. Behavioral semantics are pinned by storetest.RunStoreConformance.
type Store interface {
	// UpsertCluster registers or touches a cluster by name and returns its
	// id. A name is bound to the first non-empty ClusterUID pushed under
	// it: a different non-empty UID fails with *ClusterUIDConflictError and
	// writes nothing, and so does an empty UID once one is bound (a push that
	// cannot say which cluster it is must not join a bound history).
	UpsertCluster(ctx context.Context, c Cluster) (int64, error)
	// ClusterByName returns the cluster registered under name, or
	// ErrNotFound. It never creates one.
	ClusterByName(ctx context.Context, name string) (Cluster, error)
	// DeleteCluster removes the named cluster with everything that is its
	// own: snapshots, evaluations, queued notifications and the ingest
	// tokens minted for its name (ErrNotFound when unknown). A rebuilt
	// cluster that re-registers the name needs a new token.
	DeleteCluster(ctx context.Context, name string) error
	// RenameCluster renames a cluster; its history and its ingest tokens
	// move with it. ErrNotFound when name is unknown, ErrClusterNameTaken
	// when newName is registered to another cluster. Renaming a cluster to
	// its own name is a no-op.
	RenameCluster(ctx context.Context, name, newName string) error
	// Prune is retention: it deletes evaluations created before cutoff and
	// then snapshots received before it that no evaluation refers to any
	// more, except each cluster's latest snapshot and its evaluations (the
	// cluster's current state, however old). Tokens, clusters and the
	// outbox are not touched.
	Prune(ctx context.Context, cutoff time.Time) (PruneResult, error)

	InsertSnapshot(ctx context.Context, s Snapshot) (int64, bool, error) // (id, duplicate, err) — duplicate iff same cluster+hash as latest
	LatestSnapshot(ctx context.Context, clusterID int64) (Snapshot, error)
	// LatestSnapshotHeads returns every cluster's latest snapshot, keyed by
	// cluster id, without Inventory (nil): fleet views need the id and
	// server version of each, not hundreds of inventory blobs. Clusters
	// without a snapshot are absent.
	LatestSnapshotHeads(ctx context.Context) (map[int64]Snapshot, error)
	ListClusters(ctx context.Context) ([]Cluster, error)
	GetCluster(ctx context.Context, id int64) (Cluster, error)
	InsertEvaluation(ctx context.Context, e Evaluation) (int64, error)
	// LatestEvaluation is the newest evaluation for (cluster, target) from
	// any snapshot — history, not the cluster's current state.
	LatestEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error)
	// CurrentEvaluation is the newest evaluation for target of the
	// cluster's LATEST snapshot, or ErrNotFound — an evaluation of an
	// older snapshot describes an inventory the cluster no longer has.
	CurrentEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error)
	// LatestKnownEvaluation is the newest evaluation for (cluster, target)
	// whose verdict was decided — ready, or at least one blocker — skipping
	// "unknown" ones (no blockers but required checks not assessed).
	LatestKnownEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error)
	ScoreHistory(ctx context.Context, clusterID int64, target string, limit int) ([]ScorePoint, error)

	// CommitEvaluations writes one evaluation pass atomically: the snapshot
	// (when b.Snapshot is set), every insert and refresh, and the outbox
	// messages, or nothing. It returns the evaluated snapshot's id.
	// duplicate is true when b.Snapshot has the same hash as the cluster's
	// latest snapshot: then no snapshot, evaluation or outbox row is
	// written, the push's envelope (KBVersion, AgentVersion) is recorded
	// on the latest snapshot, and the latest id is returned. With
	// b.Cluster set, the cluster is registered or touched in the same
	// transaction, so a failed commit leaves no cluster row and no
	// last-seen bump behind. ErrConflict means another writer moved the
	// cluster on (b.SnapshotID is no longer latest, or b.Current no longer
	// matches); *ClusterUIDConflictError that b.Cluster's UID is refused.
	CommitEvaluations(ctx context.Context, b EvaluationBatch) (snapshotID int64, duplicate bool, err error)

	// Notification outbox: messages committed with their evaluations,
	// delivered by a worker after commit. ClaimOutbox returns up to limit
	// messages due at now, oldest first, and leases them (attempts+1,
	// next attempt at now+lease) so a crashed delivery is retried and
	// concurrent workers do not deliver one message twice.
	ClaimOutbox(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]OutboxMessage, error)
	DeleteOutbox(ctx context.Context, id int64) error                                     // delivered or given up; ErrNotFound when gone
	RescheduleOutbox(ctx context.Context, id int64, next time.Time, lastErr string) error // failed attempt; ErrNotFound when gone

	// Per-cluster ingest tokens (P3, spec §8). Tokens are keyed by cluster
	// NAME (not id): a token may be minted before the cluster's first push
	// registers it. Only the sha256 of the plaintext is ever stored.
	CreateToken(ctx context.Context, clusterName, token string) (int64, error) // returns the token id; errors if the token is already issued (any cluster)
	ValidToken(ctx context.Context, token string) (string, bool, error)        // (clusterName, true) for active; ("", false, nil) for unknown/revoked
	ListTokens(ctx context.Context, clusterName string) ([]Token, error)       // active and revoked, ascending id; "" = every cluster
	RevokeTokenID(ctx context.Context, clusterName string, id int64) error     // revokes one active token of clusterName; ErrNotFound otherwise
	RevokeToken(ctx context.Context, clusterName string) error                 // revokes ALL active tokens; ErrNotFound when none are active

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
	// ServerVersion is the Kubernetes version the snapshot is judged at:
	// its inventory's own serverVersion, or — for a degraded push that
	// reported none — the version of the snapshot before it, so the
	// cluster keeps its default target. "" on rows stored before the
	// column existed (read the inventory's) and when no version was ever
	// known.
	ServerVersion string `json:"serverVersion,omitempty"`
	Inventory     []byte `json:"-"` // the inventory JSON exactly as pushed
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
	// EvaluatedAt is when this result was last confirmed: a re-evaluation
	// with the same verdict, score and finding keys refreshes the row
	// instead of adding a history point. Zero on insert defaults to
	// CreatedAt.
	EvaluatedAt time.Time `json:"evaluatedAt"`
	// TeamMapHash identifies the server --team-map the report was computed
	// with ("" = none), so a changed map triggers a re-evaluation.
	TeamMapHash string `json:"teamMapHash,omitempty"`
}

// ErrConflict is returned by CommitEvaluations when the batch was computed
// against state another writer has since changed. Nothing was written; the
// caller may drop the pass (the other writer covered it) or recompute.
var ErrConflict = errors.New("store: evaluation state changed concurrently")

// ErrClusterNameTaken is returned by RenameCluster when the new name is
// registered to another cluster. Test with errors.Is.
var ErrClusterNameTaken = errors.New("store: cluster name is taken")

// PruneResult counts the rows one Prune deleted.
type PruneResult struct {
	Snapshots   int64
	Evaluations int64
}

// EvaluationBatch is one evaluation pass over one cluster snapshot.
type EvaluationBatch struct {
	// Cluster, when non-nil, is upserted first in the commit's transaction
	// (UpsertCluster rules) and its id replaces ClusterID: ingest registers
	// a new cluster only together with its first snapshot.
	Cluster   *Cluster
	ClusterID int64
	// Snapshot, when non-nil, is a newly pushed snapshot stored first
	// (InsertSnapshot dedup rules); every Insert gets its id.
	Snapshot *Snapshot
	// SnapshotID is the already-stored snapshot the pass evaluated when
	// Snapshot is nil. It must still be the cluster's latest snapshot.
	SnapshotID int64
	// Current records, per target, the id of the snapshot's current
	// evaluation the pass compared against (0 = it had none). The commit
	// fails with ErrConflict when any of them changed meanwhile.
	Current map[string]int64
	// Insert adds history rows; their SnapshotID is overwritten with the
	// evaluated snapshot's id.
	Insert []Evaluation
	// Refresh updates existing rows by ID with an unchanged result:
	// Report, KBVersion, TeamMapHash, Blockers, Warnings and EvaluatedAt.
	// Score, Ready and CreatedAt (the history point) stay.
	Refresh []Evaluation
	Outbox  []OutboxMessage
}

// OutboxMessage is one notification awaiting delivery to one sink.
type OutboxMessage struct {
	ID            int64
	ClusterID     int64  // set by CommitEvaluations; DeleteCluster drops the cluster's messages
	Sink          string // which configured notifier delivers it
	Payload       []byte // the event, JSON
	Attempts      int    // delivery attempts started so far (claims)
	CreatedAt     time.Time
	NextAttemptAt time.Time
}

// Token is a per-cluster ingest token's metadata. The plaintext is never
// stored; Prefix lets an operator match a listed token to the secret they
// deployed (see TokenPrefix).
type Token struct {
	ID          int64      `json:"id"`
	ClusterName string     `json:"clusterName"`
	Prefix      string     `json:"prefix"`
	CreatedAt   time.Time  `json:"createdAt"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"` // nil = active
}

// TokenPrefix is the identifying prefix stored with a token: its first 8
// characters, or "" when the token is shorter than 32 characters — a
// prefix must stay a small fraction of the secret. `tokens create` mints
// 64 hex characters, so the stored prefix carries 32 of its 256 bits.
func TokenPrefix(token string) string {
	if len(token) < 32 {
		return ""
	}
	return token[:8]
}

type ScorePoint struct {
	At    time.Time `json:"at"`
	Score int       `json:"score"`
	Ready bool      `json:"ready"`
}

// timeFormat is RFC 3339 with a fixed nine-digit fractional second so that
// stored UTC strings sort lexicographically in instant order.
// time.RFC3339Nano trims trailing zeros, which would make "…05Z" sort after
// "…05.5Z"; retention compares stored times as strings, so string order must be
// instant order. "Latest" and history order use ids, never these strings.
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
