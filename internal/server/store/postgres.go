package store

import (
	"cmp"
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver, registered as "pgx"
)

//go:embed pgmigrations/*.sql
var pgMigrationsFS embed.FS

// Postgres is the Store implementation for fleets: same contract as SQLite
// (pinned by storetest.RunStoreConformance), backed by jackc/pgx via
// database/sql. Times are TIMESTAMPTZ columns; every time.Time returned is
// normalized to UTC.
type Postgres struct {
	db *sql.DB
}

var _ Store = (*Postgres)(nil)

// OpenPostgres connects to dsn (any pgx-accepted form, e.g.
// postgres://user:pass@host:5432/db), verifies the connection with a ping,
// and applies the embedded pgmigrations. Fails fast on unreachable or
// unauthorized servers instead of deferring the error to the first query.
func OpenPostgres(dsn string) (*Postgres, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	sub, err := fs.Sub(pgMigrationsFS, "pgmigrations")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("embedded pgmigrations: %w", err)
	}
	if _, err := migratePostgres(ctx, db, sub); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate postgres: %w", err)
	}
	return &Postgres{db: db}, nil
}

// Close closes the underlying connection pool.
func (p *Postgres) Close() error { return p.db.Close() }

// UpsertCluster inserts the cluster or, if a row with the same name exists,
// bumps last_seen (first_seen never moves) and adopts c.ClusterUID when the
// stored one is empty. A different non-empty UID is refused: the guarded
// DO UPDATE matches no row, so RETURNING yields nothing and the stored UID
// is read back for the *ClusterUIDConflictError. Zero FirstSeen/LastSeen
// default to time.Now().UTC().
func (p *Postgres) UpsertCluster(ctx context.Context, c Cluster) (int64, error) {
	now := time.Now().UTC()
	first, last := c.FirstSeen, c.LastSeen
	if first.IsZero() {
		first = now
	}
	if last.IsZero() {
		last = now
	}
	var id int64
	err := p.db.QueryRowContext(ctx, `
		INSERT INTO clusters (name, cluster_uid, first_seen, last_seen)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE SET
			cluster_uid = CASE WHEN excluded.cluster_uid = '' THEN clusters.cluster_uid ELSE excluded.cluster_uid END,
			last_seen   = excluded.last_seen
		WHERE clusters.cluster_uid = '' OR excluded.cluster_uid = '' OR clusters.cluster_uid = excluded.cluster_uid
		RETURNING id`,
		c.Name, c.ClusterUID, first, last).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		var stored string
		if err := p.db.QueryRowContext(ctx, `SELECT cluster_uid FROM clusters WHERE name = $1`, c.Name).Scan(&stored); err != nil {
			return 0, fmt.Errorf("upsert cluster %q: read stored uid: %w", c.Name, err)
		}
		return 0, &ClusterUIDConflictError{Name: c.Name, StoredUID: stored, PushedUID: c.ClusterUID}
	}
	if err != nil {
		return 0, fmt.Errorf("upsert cluster %q: %w", c.Name, err)
	}
	return id, nil
}

// DeleteCluster removes the named cluster, its evaluations and snapshots in
// one transaction, or returns ErrNotFound. Tokens are keyed by name and
// are left alone. The clusters row is locked first so a concurrent ingest
// of the same cluster cannot add a snapshot between the deletes.
func (p *Postgres) DeleteCluster(ctx context.Context, name string) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete cluster %q: begin: %w", name, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM clusters WHERE name = $1 FOR UPDATE`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("cluster %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("delete cluster %q: %w", name, err)
	}
	for _, stmt := range []string{
		`DELETE FROM evaluations WHERE cluster_id = $1`,
		`DELETE FROM snapshots WHERE cluster_id = $1`,
		`DELETE FROM clusters WHERE id = $1`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return fmt.Errorf("delete cluster %q: %w", name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete cluster %q: commit: %w", name, err)
	}
	return nil
}

// scanClusterPg mirrors scanCluster for TIMESTAMPTZ columns: the driver
// hands back time.Time directly; normalize to UTC.
func scanClusterPg(rs rowScanner) (Cluster, error) {
	var c Cluster
	if err := rs.Scan(&c.ID, &c.Name, &c.ClusterUID, &c.FirstSeen, &c.LastSeen); err != nil {
		return Cluster{}, err
	}
	c.FirstSeen = c.FirstSeen.UTC()
	c.LastSeen = c.LastSeen.UTC()
	return c, nil
}

// GetCluster returns the cluster by id, or ErrNotFound.
func (p *Postgres) GetCluster(ctx context.Context, id int64) (Cluster, error) {
	c, err := scanClusterPg(p.db.QueryRowContext(ctx,
		`SELECT id, name, cluster_uid, first_seen, last_seen FROM clusters WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Cluster{}, fmt.Errorf("cluster %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Cluster{}, fmt.Errorf("get cluster %d: %w", id, err)
	}
	return c, nil
}

// ListClusters returns all clusters, ascending by name.
func (p *Postgres) ListClusters(ctx context.Context) ([]Cluster, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, name, cluster_uid, first_seen, last_seen FROM clusters ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	defer rows.Close()
	var out []Cluster
	for rows.Next() {
		c, err := scanClusterPg(rows)
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
// It is CommitEvaluations with nothing but the snapshot.
func (p *Postgres) InsertSnapshot(ctx context.Context, snap Snapshot) (int64, bool, error) {
	id, dup, err := p.CommitEvaluations(ctx, EvaluationBatch{ClusterID: snap.ClusterID, Snapshot: &snap})
	if err != nil {
		return 0, false, fmt.Errorf("insert snapshot: %w", err)
	}
	return id, dup, nil
}

// LatestSnapshot returns the most recently inserted snapshot for the
// cluster (highest id), or ErrNotFound.
func (p *Postgres) LatestSnapshot(ctx context.Context, clusterID int64) (Snapshot, error) {
	var snap Snapshot
	err := p.db.QueryRowContext(ctx, `
		SELECT id, cluster_id, hash, kb_version, agent_version, received_at, inventory
		FROM snapshots WHERE cluster_id = $1 ORDER BY id DESC LIMIT 1`, clusterID).
		Scan(&snap.ID, &snap.ClusterID, &snap.Hash, &snap.KBVersion, &snap.AgentVersion, &snap.ReceivedAt, &snap.Inventory)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("latest snapshot for cluster %d: %w", clusterID, ErrNotFound)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("latest snapshot for cluster %d: %w", clusterID, err)
	}
	snap.ReceivedAt = snap.ReceivedAt.UTC()
	return snap, nil
}

// scanEvaluationPg mirrors scanEvaluation for TIMESTAMPTZ columns.
func scanEvaluationPg(rs rowScanner) (Evaluation, error) {
	var e Evaluation
	if err := rs.Scan(&e.ID, &e.ClusterID, &e.SnapshotID, &e.Target, &e.KBVersion,
		&e.Score, &e.Ready, &e.Blockers, &e.Warnings, &e.Report, &e.CreatedAt, &e.EvaluatedAt, &e.TeamMapHash); err != nil {
		return Evaluation{}, err
	}
	e.CreatedAt = e.CreatedAt.UTC()
	e.EvaluatedAt = e.EvaluatedAt.UTC()
	return e, nil
}

// InsertEvaluation stores e. Zero CreatedAt defaults to now (UTC); zero
// EvaluatedAt to CreatedAt.
func (p *Postgres) InsertEvaluation(ctx context.Context, e Evaluation) (int64, error) {
	return insertEvaluationPg(ctx, p.db, e)
}

func insertEvaluationPg(ctx context.Context, x sqlExecer, e Evaluation) (int64, error) {
	created := e.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	evaluated := e.EvaluatedAt
	if evaluated.IsZero() {
		evaluated = created
	}
	var id int64
	err := x.QueryRowContext(ctx, `
		INSERT INTO evaluations (cluster_id, snapshot_id, target, kb_version, score, ready, blockers, warnings, report, created_at, evaluated_at, team_map_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING id`,
		e.ClusterID, e.SnapshotID, e.Target, e.KBVersion, e.Score, e.Ready, e.Blockers, e.Warnings, e.Report,
		created, evaluated, e.TeamMapHash).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert evaluation: %w", err)
	}
	return id, nil
}

// queryEvaluation runs a single-row evaluation query, mapping no row to
// ErrNotFound (wrapped with what).
func (p *Postgres) queryEvaluation(ctx context.Context, what, query string, args ...any) (Evaluation, error) {
	e, err := scanEvaluationPg(p.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Evaluation{}, fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	if err != nil {
		return Evaluation{}, fmt.Errorf("%s: %w", what, err)
	}
	return e, nil
}

// LatestEvaluation returns the newest evaluation for (cluster, target) by
// created_at (ties broken by id), or ErrNotFound.
func (p *Postgres) LatestEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error) {
	return p.queryEvaluation(ctx, fmt.Sprintf("latest evaluation for cluster %d target %s", clusterID, target), `
		SELECT `+evaluationColumns+` FROM evaluations WHERE cluster_id = $1 AND target = $2
		ORDER BY created_at DESC, id DESC LIMIT 1`, clusterID, target)
}

// CurrentEvaluation returns the newest evaluation for target of the
// cluster's latest snapshot (highest id), or ErrNotFound.
func (p *Postgres) CurrentEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error) {
	return p.queryEvaluation(ctx, fmt.Sprintf("current evaluation for cluster %d target %s", clusterID, target), `
		SELECT `+evaluationColumns+` FROM evaluations
		WHERE snapshot_id = (SELECT MAX(id) FROM snapshots WHERE cluster_id = $1) AND target = $2
		ORDER BY created_at DESC, id DESC LIMIT 1`, clusterID, target)
}

// LatestKnownEvaluation returns the newest evaluation for (cluster, target)
// that is ready or has a blocker, or ErrNotFound.
func (p *Postgres) LatestKnownEvaluation(ctx context.Context, clusterID int64, target string) (Evaluation, error) {
	return p.queryEvaluation(ctx, fmt.Sprintf("latest known evaluation for cluster %d target %s", clusterID, target), `
		SELECT `+evaluationColumns+` FROM evaluations
		WHERE cluster_id = $1 AND target = $2 AND (ready OR blockers > 0)
		ORDER BY created_at DESC, id DESC LIMIT 1`, clusterID, target)
}

// CommitEvaluations writes b in one transaction.
//
// Concurrency: the duplicate check and the Current/SnapshotID checks read
// state that must not change before the commit, and READ COMMITTED gives
// no such protection — two racing pushes of one hash would both miss the
// duplicate, two racing passes would both insert. Locking the parent
// clusters row FOR UPDATE first serializes writers of one cluster (across
// replicas too) without blocking other clusters.
func (p *Postgres) CommitEvaluations(ctx context.Context, b EvaluationBatch) (int64, bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("commit evaluations: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var lockID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM clusters WHERE id = $1 FOR UPDATE`, b.ClusterID).Scan(&lockID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// Unknown cluster: fall through — the INSERT's FK reports it properly.
		return 0, false, fmt.Errorf("commit evaluations: lock cluster: %w", err)
	}

	var latestID int64
	var latestHash string
	err = tx.QueryRowContext(ctx,
		`SELECT id, hash FROM snapshots WHERE cluster_id = $1 ORDER BY id DESC LIMIT 1`,
		b.ClusterID).Scan(&latestID, &latestHash)
	noSnapshot := errors.Is(err, sql.ErrNoRows)
	if err != nil && !noSnapshot {
		return 0, false, fmt.Errorf("commit evaluations: query latest snapshot: %w", err)
	}

	snapID := b.SnapshotID
	if b.Snapshot != nil {
		if !noSnapshot && latestHash == b.Snapshot.Hash {
			return latestID, true, nil
		}
		received := b.Snapshot.ReceivedAt
		if received.IsZero() {
			received = time.Now().UTC()
		}
		inv := b.Snapshot.Inventory
		if inv == nil {
			inv = []byte{}
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO snapshots (cluster_id, hash, kb_version, agent_version, received_at, inventory)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			b.ClusterID, b.Snapshot.Hash, b.Snapshot.KBVersion, b.Snapshot.AgentVersion, received, inv).Scan(&snapID); err != nil {
			return 0, false, fmt.Errorf("commit evaluations: insert snapshot: %w", err)
		}
	} else {
		if noSnapshot || latestID != snapID {
			return 0, false, fmt.Errorf("commit evaluations: snapshot %d is no longer cluster %d's latest: %w", snapID, b.ClusterID, ErrConflict)
		}
		for target, want := range b.Current {
			var got int64
			err := tx.QueryRowContext(ctx, `
				SELECT id FROM evaluations WHERE snapshot_id = $1 AND target = $2
				ORDER BY created_at DESC, id DESC LIMIT 1`, snapID, target).Scan(&got)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, false, fmt.Errorf("commit evaluations: current %s: %w", target, err)
			}
			if got != want {
				return 0, false, fmt.Errorf("commit evaluations: target %s changed (evaluation %d, expected %d): %w", target, got, want, ErrConflict)
			}
		}
	}

	for _, e := range b.Insert {
		e.SnapshotID = snapID
		if _, err := insertEvaluationPg(ctx, tx, e); err != nil {
			return 0, false, fmt.Errorf("commit evaluations: %w", err)
		}
	}
	for _, e := range b.Refresh {
		evaluated := e.EvaluatedAt
		if evaluated.IsZero() {
			evaluated = time.Now().UTC()
		}
		if err := execOne(ctx, tx, fmt.Sprintf("commit evaluations: refresh evaluation %d", e.ID), `
			UPDATE evaluations SET report = $1, kb_version = $2, team_map_hash = $3, blockers = $4, warnings = $5, evaluated_at = $6
			WHERE id = $7 AND cluster_id = $8`,
			e.Report, e.KBVersion, e.TeamMapHash, e.Blockers, e.Warnings, evaluated, e.ID, b.ClusterID); err != nil {
			return 0, false, err
		}
	}
	for _, m := range b.Outbox {
		created := m.CreatedAt
		if created.IsZero() {
			created = time.Now().UTC()
		}
		next := m.NextAttemptAt
		if next.IsZero() {
			next = created
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO outbox (sink, payload, attempts, created_at, next_attempt_at) VALUES ($1, $2, 0, $3, $4)`,
			m.Sink, m.Payload, created, next); err != nil {
			return 0, false, fmt.Errorf("commit evaluations: outbox: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit evaluations: commit: %w", err)
	}
	return snapID, false, nil
}

// ClaimOutbox leases up to limit due messages (next_attempt_at <= now),
// oldest first. FOR UPDATE SKIP LOCKED lets replicas' workers claim
// disjoint batches instead of queueing on each other's rows.
func (p *Postgres) ClaimOutbox(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]OutboxMessage, error) {
	rows, err := p.db.QueryContext(ctx, `
		UPDATE outbox SET attempts = attempts + 1, next_attempt_at = $1
		WHERE id IN (
			SELECT id FROM outbox WHERE next_attempt_at <= $2 ORDER BY id LIMIT $3 FOR UPDATE SKIP LOCKED
		)
		RETURNING id, sink, payload, attempts, created_at, next_attempt_at`,
		now.Add(lease).UTC(), now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	defer rows.Close()
	var out []OutboxMessage
	for rows.Next() {
		var m OutboxMessage
		if err := rows.Scan(&m.ID, &m.Sink, &m.Payload, &m.Attempts, &m.CreatedAt, &m.NextAttemptAt); err != nil {
			return nil, fmt.Errorf("claim outbox: %w", err)
		}
		m.CreatedAt = m.CreatedAt.UTC()
		m.NextAttemptAt = m.NextAttemptAt.UTC()
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	slices.SortFunc(out, func(a, b OutboxMessage) int { return cmp.Compare(a.ID, b.ID) }) // RETURNING order is unspecified
	return out, nil
}

// DeleteOutbox removes message id, or returns ErrNotFound.
func (p *Postgres) DeleteOutbox(ctx context.Context, id int64) error {
	return execOne(ctx, p.db, fmt.Sprintf("delete outbox message %d", id), `DELETE FROM outbox WHERE id = $1`, id)
}

// RescheduleOutbox records a failed attempt and when to try again, or
// returns ErrNotFound.
func (p *Postgres) RescheduleOutbox(ctx context.Context, id int64, next time.Time, lastErr string) error {
	return execOne(ctx, p.db, fmt.Sprintf("reschedule outbox message %d", id),
		`UPDATE outbox SET next_attempt_at = $1, last_error = $2 WHERE id = $3`, next.UTC(), lastErr, id)
}

// CreateToken stores a new active ingest token for clusterName and returns
// its id; only the sha256 of token (plus TokenPrefix) is persisted. Fails
// if the token is already issued.
func (p *Postgres) CreateToken(ctx context.Context, clusterName, token string) (int64, error) {
	if clusterName == "" || token == "" {
		return 0, errors.New("create token: cluster name and token must be non-empty")
	}
	var id int64
	if err := p.db.QueryRowContext(ctx,
		`INSERT INTO tokens (cluster_name, token_hash, token_prefix, created_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		clusterName, HashToken(token), TokenPrefix(token), time.Now().UTC()).Scan(&id); err != nil {
		return 0, fmt.Errorf("create token for %q: %w", clusterName, err)
	}
	return id, nil
}

// ListTokens returns token metadata, ascending by id, for clusterName or
// for every cluster when clusterName is "".
func (p *Postgres) ListTokens(ctx context.Context, clusterName string) ([]Token, error) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT id, cluster_name, token_prefix, created_at, revoked_at FROM tokens
		WHERE $1::text = '' OR cluster_name = $1 ORDER BY id`, clusterName)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var tk Token
		var revoked sql.NullTime
		if err := rows.Scan(&tk.ID, &tk.ClusterName, &tk.Prefix, &tk.CreatedAt, &revoked); err != nil {
			return nil, fmt.Errorf("list tokens: %w", err)
		}
		tk.CreatedAt = tk.CreatedAt.UTC()
		if revoked.Valid {
			at := revoked.Time.UTC()
			tk.RevokedAt = &at
		}
		out = append(out, tk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	return out, nil
}

// RevokeTokenID revokes the active token id of clusterName, or ErrNotFound
// when there is none (unknown id, another cluster's token, or already
// revoked). The cluster check guards against a mistyped id.
func (p *Postgres) RevokeTokenID(ctx context.Context, clusterName string, id int64) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE tokens SET revoked_at = $1 WHERE id = $2 AND cluster_name = $3 AND revoked_at IS NULL`,
		time.Now().UTC(), id, clusterName)
	if err != nil {
		return fmt.Errorf("revoke token %d of %q: %w", id, clusterName, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke token %d of %q: %w", id, clusterName, err)
	}
	if n == 0 {
		return fmt.Errorf("no active token %d for cluster %q: %w", id, clusterName, ErrNotFound)
	}
	return nil
}

// ValidToken resolves a presented plaintext token to its cluster name.
// Unknown or revoked tokens are ("", false, nil) — not an error.
func (p *Postgres) ValidToken(ctx context.Context, token string) (string, bool, error) {
	var name string
	err := p.db.QueryRowContext(ctx,
		`SELECT cluster_name FROM tokens WHERE token_hash = $1 AND revoked_at IS NULL`,
		HashToken(token)).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("validate token: %w", err)
	}
	return name, true, nil
}

// RevokeToken revokes every active token of clusterName, or ErrNotFound
// when the cluster has none.
func (p *Postgres) RevokeToken(ctx context.Context, clusterName string) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE tokens SET revoked_at = $1 WHERE cluster_name = $2 AND revoked_at IS NULL`,
		time.Now().UTC(), clusterName)
	if err != nil {
		return fmt.Errorf("revoke tokens for %q: %w", clusterName, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke tokens for %q: %w", clusterName, err)
	}
	if n == 0 {
		return fmt.Errorf("no active tokens for cluster %q: %w", clusterName, ErrNotFound)
	}
	return nil
}

// ScoreHistory returns score points for (cluster, target), oldest-first
// ascending by created_at. limit > 0 selects the most recent N rows (still
// returned oldest-first); limit <= 0 returns all. An unknown cluster or
// target yields an empty slice and nil error.
func (p *Postgres) ScoreHistory(ctx context.Context, clusterID int64, target string, limit int) ([]ScorePoint, error) {
	var lim any // LIMIT NULL == no limit in Postgres
	if limit > 0 {
		lim = limit
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT created_at, score, ready FROM (
			SELECT id, created_at, score, ready FROM evaluations
			WHERE cluster_id = $1 AND target = $2
			ORDER BY created_at DESC, id DESC LIMIT $3
		) recent ORDER BY created_at ASC, id ASC`, clusterID, target, lim)
	if err != nil {
		return nil, fmt.Errorf("score history cluster %d target %s: %w", clusterID, target, err)
	}
	defer rows.Close()
	var out []ScorePoint
	for rows.Next() {
		var pt ScorePoint
		if err := rows.Scan(&pt.At, &pt.Score, &pt.Ready); err != nil {
			return nil, fmt.Errorf("score history cluster %d target %s: %w", clusterID, target, err)
		}
		pt.At = pt.At.UTC()
		out = append(out, pt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("score history cluster %d target %s: %w", clusterID, target, err)
	}
	return out, nil
}
