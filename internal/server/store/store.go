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

// Store is the persistence contract. SQLite implements it in P2; P3 adds
// Postgres. Behavioral semantics are pinned by storetest.RunStoreConformance.
type Store interface {
	UpsertCluster(ctx context.Context, c Cluster) (int64, error)         // by name; returns id
	InsertSnapshot(ctx context.Context, s Snapshot) (int64, bool, error) // (id, duplicate, err) — duplicate iff same cluster+hash as latest
	LatestSnapshot(ctx context.Context, clusterID int64) (Snapshot, error)
	ListClusters(ctx context.Context) ([]Cluster, error)
	GetCluster(ctx context.Context, id int64) (Cluster, error)
	InsertEvaluation(ctx context.Context, e Evaluation) (int64, error)
	LatestEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error)
	ScoreHistory(ctx context.Context, clusterID int64, target string, limit int) ([]ScorePoint, error)

	// Per-cluster ingest tokens (P3, spec §8). Tokens are keyed by cluster
	// NAME (not id): a token may be minted before the cluster's first push
	// registers it. Only the sha256 of the plaintext is ever stored.
	CreateToken(ctx context.Context, clusterName, token string) error    // errors if the token is already issued (any cluster)
	ValidToken(ctx context.Context, token string) (string, bool, error)  // (clusterName, true) for active; ("", false, nil) for unknown/revoked
	RevokeToken(ctx context.Context, clusterName string) error           // revokes ALL active tokens; ErrNotFound when none are active

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
