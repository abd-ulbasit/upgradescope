// Package store persists clusters, snapshots and evaluations for the
// upgradescope server. Store is the seam P3's Postgres implementation
// fills in; P2 ships the SQLite implementation in this package.
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
	"unicode/utf8"
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
	// LatestSnapshotHead is LatestSnapshot without Inventory (nil): a
	// push compares its hash and reads its server version, and the
	// inventory is up to --max-snapshot-bytes.
	LatestSnapshotHead(ctx context.Context, clusterID int64) (Snapshot, error)
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
	// CurrentEvaluationSummary is CurrentEvaluation without the report
	// (Report nil): the columns and NotAssessed. The read API's summaries
	// of every cluster (cluster list, fleet matrix, metrics) use it, so
	// their cost never depends on how large a report is.
	CurrentEvaluationSummary(ctx context.Context, clusterID int64, target string) (Evaluation, error)
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
	// DeferOutbox puts a claimed message back, due at next, for a delivery
	// that was never tried: the attempt the claim counted is given back
	// (attempts-1, never below 0) and last_error is left alone. ErrNotFound
	// when gone.
	DeferOutbox(ctx context.Context, id int64, next time.Time) error

	// Per-cluster ingest tokens (P3, spec §8). Tokens are keyed by cluster
	// NAME (not id): a token may be minted before the cluster's first push
	// registers it. Only the sha256 of the plaintext is ever stored.
	CreateToken(ctx context.Context, clusterName, token string) (int64, error) // returns the token id; errors if the token is already issued (any cluster)
	ValidToken(ctx context.Context, token string) (string, bool, error)        // (clusterName, true) for active; ("", false, nil) for unknown/revoked
	ListTokens(ctx context.Context, clusterName string) ([]Token, error)       // active and revoked, ascending id; "" = every cluster
	RevokeTokenID(ctx context.Context, clusterName string, id int64) error     // revokes one active token of clusterName; ErrNotFound otherwise
	RevokeToken(ctx context.Context, clusterName string) error                 // revokes ALL active tokens; ErrNotFound when none are active

	// Read tokens: bearer tokens for the read API, each scoped to a set of
	// team names, or to ReadScopeFleet ("*") for the whole fleet. Like
	// ingest tokens, only the sha256 of the plaintext and its prefix are
	// stored, in a table of their own: an ingest token never reads, and a
	// read token never pushes. Never deleted, only revoked, so the server
	// can tell a database where read tokens were ever minted (it then
	// requires one) from one where none were.
	CreateReadToken(ctx context.Context, teams []string, token string) (int64, error) // returns the token id; errors on no teams, or if the token is already issued
	ValidReadToken(ctx context.Context, token string) ([]string, bool, error)         // (teams, sorted, true) for active; (nil, false, nil) for unknown/revoked
	ListReadTokens(ctx context.Context) ([]ReadToken, error)                          // active and revoked, ascending id
	RevokeReadToken(ctx context.Context, id int64) error                              // revokes one active read token; ErrNotFound otherwise

	// ClustersOfTeams returns, ascending, the ids of the clusters whose
	// current evaluations (each target's newest of the cluster's latest
	// snapshot) were written with teamMapHash (Evaluation.TeamMapHash, the
	// server's current --team-map) and name at least one of teams in their
	// Teams: the clusters a read token scoped to those teams reads. An
	// evaluation written with another team map says which teams owned the
	// cluster's namespaces then, not now: the row of a target no longer
	// evaluated (a removed --targets entry) is never rewritten, and a
	// current target's is until the next pass rewrites it, so neither
	// counts. One query, whatever the fleet's size; nothing is returned
	// for no teams.
	ClustersOfTeams(ctx context.Context, teams []string, teamMapHash string) ([]int64, error)

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
	ID         int64  `json:"id"`
	ClusterID  int64  `json:"clusterId"`
	SnapshotID int64  `json:"snapshotId"`
	Target     string `json:"target"`
	KBVersion  string `json:"kbVersion"`
	Score      int    `json:"score"`
	Ready      bool   `json:"ready"`
	Blockers   int    `json:"blockers"`
	Warnings   int    `json:"warnings"`
	Report     []byte `json:"-"` // full engine.Report JSON
	// NotAssessed is the report's notAssessed array as JSON, nil when it
	// is absent or empty. Written by the store, never by the caller: every
	// write that stores a Report stores this beside it (notAssessedOf), so
	// a summary that says what a verdict could not cover reads no report.
	NotAssessed []byte    `json:"-"`
	CreatedAt   time.Time `json:"createdAt"`
	// EvaluatedAt is when this result was last confirmed: a re-evaluation
	// with the same verdict, score and finding keys refreshes the row
	// instead of adding a history point. Zero on insert defaults to
	// CreatedAt.
	EvaluatedAt time.Time `json:"evaluatedAt"`
	// TeamMapHash identifies the server --team-map the report was computed
	// with ("" = none), so a changed map triggers a re-evaluation.
	TeamMapHash string `json:"teamMapHash,omitempty"`
	// Teams are the teams the evaluated inventory attributes a namespace to
	// (sorted): the clusters a team-scoped read token reads
	// (ClustersOfTeams). Written with every Report, on insert and refresh
	// (nil is none); reads leave it nil.
	Teams []string `json:"-"`
	// TeamsUnknown is set on read for a row written by a binary that
	// predates the teams column (NULL): no scoped token reads its cluster
	// until the next pass rewrites it, so the server treats it as stale.
	TeamsUnknown bool `json:"-"`
}

// teamsColumn is Teams as the teams column stores it: a JSON array of
// strings, "[]" for none.
func teamsColumn(teams []string) string {
	if len(teams) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(teams) // a []string cannot fail
	return string(b)
}

// What the not_assessed column keeps of a gap. The fleet-wide reads load
// it for every cluster and target, so it is bounded: a gap's capability
// name (a push may name one of 16 KiB) is cut to SummaryCapabilityBytes,
// its reason (up to 64 KiB) to SummaryReasonBytes, and its skipped list
// to its first SummarySkipped entries, each cut to SummarySkippedBytes,
// with skippedOmitted counting the rest. A gap is then at most about
// 900 bytes, and with at most inventory.MaxCapabilities gaps and the
// engine's own an evaluation's column at most about 30 KB. (With names
// whole, a 1 KiB reason and 10 entries of 512 bytes, the columns of 50
// clusters made /fleet answer 108 MB and grow the heap 516 MiB; the reads
// also list only part of a column: fleetSummaryBytes in the server.) A
// genuine name, reason or entry is shorter; the report keeps every gap
// whole.
const (
	SummaryCapabilityBytes = 64
	SummaryReasonBytes     = 256
	SummarySkipped         = 3
	SummarySkippedBytes    = 128
)

// notAssessedOf extracts a report's notAssessed array for its own column,
// each gap bounded as above: "" when the report has none, an empty one,
// or does not decode (the report endpoint then says it is corrupt).
// Skipped fields allocate nothing, so this costs a scan of the report, not
// a copy of it.
func notAssessedOf(report []byte) string {
	var r struct {
		NotAssessed []json.RawMessage `json:"notAssessed"`
	}
	if len(report) == 0 || json.Unmarshal(report, &r) != nil || len(r.NotAssessed) == 0 {
		return ""
	}
	var out bytes.Buffer
	out.WriteByte('[')
	for i, raw := range r.NotAssessed {
		if i > 0 {
			out.WriteByte(',')
		}
		if writeGapSummary(&out, raw) != nil {
			return ""
		}
	}
	out.WriteByte(']')
	return out.String()
}

// writeGapSummary writes one gap of a report's notAssessed to out, cut to
// the column's bounds, every field it does not cut as it was, with < > &
// and U+2028/U+2029 as themselves (json.Marshal writes each as six bytes),
// and no field the gap did not have.
func writeGapSummary(out *bytes.Buffer, raw json.RawMessage) error {
	var g struct {
		Capability string   `json:"capability"`
		Reason     string   `json:"reason"`
		Skipped    []string `json:"skipped"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return err
	}
	if len(g.Capability) <= SummaryCapabilityBytes && len(g.Reason) <= SummaryReasonBytes && len(g.Skipped) <= SummarySkipped &&
		!slices.ContainsFunc(g.Skipped, func(s string) bool { return len(s) > SummarySkippedBytes }) {
		return json.Compact(out, raw)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if _, ok := fields["capability"]; ok {
		fields["capability"] = jsonString(CutString(g.Capability, SummaryCapabilityBytes))
	}
	if _, ok := fields["reason"]; ok {
		fields["reason"] = jsonString(CutString(g.Reason, SummaryReasonBytes))
	}
	if n := len(g.Skipped) - SummarySkipped; n > 0 {
		g.Skipped = g.Skipped[:SummarySkipped]
		fields["skippedOmitted"] = json.RawMessage(fmt.Sprint(n))
	}
	if _, ok := fields["skipped"]; ok && g.Skipped != nil {
		var list bytes.Buffer
		list.WriteByte('[')
		for i, s := range g.Skipped {
			if i > 0 {
				list.WriteByte(',')
			}
			list.Write(jsonString(CutString(s, SummarySkippedBytes)))
		}
		list.WriteByte(']')
		fields["skipped"] = list.Bytes()
	}
	out.WriteByte('{')
	for i, k := range slices.Sorted(maps.Keys(fields)) {
		if i > 0 {
			out.WriteByte(',')
		}
		out.Write(jsonString(k))
		out.WriteByte(':')
		if err := json.Compact(out, fields[k]); err != nil {
			return err
		}
	}
	out.WriteByte('}')
	return nil
}

// jsonString is s as a JSON string with only what JSON requires escaped:
// '"', '\\' and control characters, so < > & and U+2028/U+2029 are
// themselves.
func jsonString(s string) json.RawMessage {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			b = append(b, '\\', c)
		case c < 0x20:
			b = fmt.Appendf(b, `\u%04x`, c)
		default:
			b = append(b, c)
		}
	}
	return append(b, '"')
}

// CutString is s cut to at most max bytes, at a UTF-8 boundary, marked
// with "…" when it is cut.
func CutString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// notAssessedBytes is the read side of the not_assessed column: NULL (a
// row written by a binary that predates migration 0007) and "" both read
// as none.
func notAssessedBytes(v sql.NullString) []byte {
	if !v.Valid || v.String == "" {
		return nil
	}
	return []byte(v.String)
}

// ErrConflict is returned by CommitEvaluations when the batch was computed
// against state another writer has since changed. Nothing was written; the
// caller may drop the pass (the other writer covered it) or recompute.
var ErrConflict = errors.New("store: evaluation state changed concurrently")

// ErrTokenRevoked is returned by CommitEvaluations when the batch's
// IngestToken is no longer an active token of its cluster: revoked, or
// moved or deleted with the cluster, while the push was processed.
// Nothing was written.
var ErrTokenRevoked = errors.New("store: the ingest token is no longer valid for this cluster")

// ErrClusterChanged is returned by CommitEvaluations when the batch's
// Cluster.ID is set and the cluster registered under its name is no longer
// that cluster: it was renamed or deleted after the push looked it up.
// Nothing was written.
var ErrClusterChanged = errors.New("store: the cluster was renamed or deleted while the push was processed")

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
	// When Cluster.ID is set (the cluster the push looked up), the commit
	// fails with ErrClusterChanged unless the name is still that cluster.
	Cluster   *Cluster
	ClusterID int64
	// IngestToken, with Cluster, is the per-cluster ingest token the push
	// was authenticated with ("" for the shared token, which the store does
	// not know). The commit fails with ErrTokenRevoked unless it is still
	// an active token of Cluster.Name, checked in the commit's transaction
	// so a push authenticated before a revoke cannot land after it.
	IngestToken string
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

// ReadScopeFleet is the team scope of a fleet-wide read token.
const ReadScopeFleet = "*"

// ReadToken is a read token's metadata. The plaintext is never stored;
// Prefix is TokenPrefix of it, as for an ingest token.
type ReadToken struct {
	ID        int64      `json:"id"`
	Teams     []string   `json:"teams"` // sorted; [ReadScopeFleet] for fleet-wide
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"createdAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"` // nil = active
}

// readTokenTeams is a read token's scope as stored: sorted and
// deduplicated, or an error when it is empty or names an empty team.
func readTokenTeams(teams []string) (string, error) {
	if len(teams) == 0 {
		return "", errors.New("a read token needs at least one team, or " + ReadScopeFleet + " for the whole fleet")
	}
	out := slices.Clone(teams)
	slices.Sort(out)
	out = slices.Compact(out)
	if out[0] == "" {
		return "", errors.New("a read token's team names must be non-empty")
	}
	return teamsColumn(out), nil
}

// decodeTeams reads a teams column back.
func decodeTeams(raw string) ([]string, error) {
	var teams []string
	if err := json.Unmarshal([]byte(raw), &teams); err != nil {
		return nil, fmt.Errorf("stored teams %q: %w", raw, err)
	}
	return teams, nil
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

// recheckPush is CommitEvaluations' check, after b.Cluster's upsert gave
// b.ClusterID and in the same transaction, that the push may still commit:
// the cluster it looked up is still the one under its name
// (ErrClusterChanged), and its per-cluster token is still active for it
// (ErrTokenRevoked). tokenQuery selects a row for (token_hash,
// cluster_name) of an active token.
func recheckPush(ctx context.Context, tx *sql.Tx, b EvaluationBatch, tokenQuery string) error {
	if b.Cluster.ID != 0 && b.Cluster.ID != b.ClusterID {
		return fmt.Errorf("cluster %q (id %d, now %d): %w", b.Cluster.Name, b.Cluster.ID, b.ClusterID, ErrClusterChanged)
	}
	if b.IngestToken == "" {
		return nil
	}
	var one int
	err := tx.QueryRowContext(ctx, tokenQuery, HashToken(b.IngestToken), b.Cluster.Name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("cluster %q: %w", b.Cluster.Name, ErrTokenRevoked)
	}
	if err != nil {
		return fmt.Errorf("check ingest token: %w", err)
	}
	return nil
}
