package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// pruneBatchRows is how many snapshots or evaluations one retention
// transaction deletes. A backlog (a long outage, or --retention lowered
// from 0) drains over successive transactions within one Prune, so what a
// single transaction has to journal and sort is bounded by this and not by
// the backlog; a Prune cut short by a failure or a shutdown leaves what it
// committed deleted and the next run resumes there.
const pruneBatchRows = 5000

// PruneBatch is one committed retention transaction: the table it deleted
// from ("evaluations" or "snapshots") and how many rows. See PruneTuner.
type PruneBatch struct {
	Table string
	Rows  int64
}

// PruneTuner is implemented by SQLite and Postgres for tests: TunePrune
// sets how many rows one retention transaction deletes (0 restores the
// default) and a hook called after each transaction that deleted rows has
// committed. Call it before Prune, not concurrently with it.
type PruneTuner interface {
	TunePrune(batchRows int, onBatch func(PruneBatch))
}

// pruneTuning is the batch size and test hook a store carries.
type pruneTuning struct {
	batchRows int
	onBatch   func(PruneBatch)
}

func (t *pruneTuning) tune(batchRows int, onBatch func(PruneBatch)) {
	t.batchRows = batchRows
	t.onBatch = onBatch
}

func (t pruneTuning) rows() int {
	if t.batchRows > 0 {
		return t.batchRows
	}
	return pruneBatchRows
}

// pruneDialect is what differs between the stores' SQL.
type pruneDialect struct {
	db *sql.DB
	// ph is the nth (1-based) bind parameter.
	ph func(n int) string
	// decided is the condition for a decided (ready or blocked) evaluation.
	decided string
}

var (
	sqliteDialect = pruneDialect{ph: func(int) string { return "?" }, decided: "ready = 1 OR blockers > 0"}
	pgDialect     = pruneDialect{ph: func(n int) string { return fmt.Sprintf("$%d", n) }, decided: "ready OR blockers > 0"}
)

// prune is Prune for both stores. Each step is its own statement and so
// its own transaction, of at most t.rows() rows (pruneChunk ids for the
// baselines): evaluations first, then the baselines baselines does not
// spare, then the snapshots no evaluation refers to. Every statement
// re-checks its conditions, so a snapshot that stops being a cluster's
// latest between two of them only keeps its evaluations until the next
// run. On an error the result counts what was already deleted.
//
// at is the cutoff in the store's time representation.
func prune(ctx context.Context, d pruneDialect, t pruneTuning, at any, baselines PruneBaselines) (PruneResult, error) {
	var res PruneResult
	batch := t.rows()
	// drain runs one batched DELETE until a transaction deletes less than
	// a full batch.
	drain := func(table, stmt string) (int64, error) {
		var total int64
		for {
			r, err := d.db.ExecContext(ctx, stmt, at, batch)
			if err != nil {
				return total, fmt.Errorf("prune %s: %w", table, err)
			}
			n, err := r.RowsAffected()
			if err != nil {
				return total, fmt.Errorf("prune %s: %w", table, err)
			}
			total += n
			if n > 0 && t.onBatch != nil {
				t.onBatch(PruneBatch{Table: table, Rows: n})
			}
			if n < int64(batch) {
				return total, nil
			}
		}
	}
	var err error
	res.Evaluations, err = drain("evaluations", `
		DELETE FROM evaluations WHERE id IN (
			SELECT id FROM evaluations WHERE created_at < `+d.ph(1)+`
			AND snapshot_id NOT IN (SELECT MAX(id) FROM snapshots GROUP BY cluster_id)
			AND id NOT IN (SELECT MAX(id) FROM evaluations WHERE `+d.decided+` GROUP BY cluster_id, target)
			ORDER BY id LIMIT `+d.ph(2)+`)`)
	if err != nil {
		return res, err
	}
	if len(baselines) > 0 {
		n, err := pruneBaselines(ctx, d, t, at, baselines)
		res.Evaluations += n
		if err != nil {
			return res, err
		}
	}
	res.Snapshots, err = drain("snapshots", `
		DELETE FROM snapshots WHERE id IN (
			SELECT id FROM snapshots WHERE received_at < `+d.ph(1)+`
			AND id NOT IN (SELECT MAX(id) FROM snapshots GROUP BY cluster_id)
			AND NOT EXISTS (SELECT 1 FROM evaluations e WHERE e.snapshot_id = snapshots.id)
			ORDER BY id LIMIT `+d.ph(2)+`)`)
	return res, err
}

// pruneBaselines deletes the baselines of targets no longer in use, which
// the evaluation delete spared: found by (cluster, target) pair, deleted by
// id, pruneChunk ids a statement, under the same conditions.
func pruneBaselines(ctx context.Context, d pruneDialect, t pruneTuning, at any, baselines PruneBaselines) (int64, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT cluster_id, target, MAX(id) FROM evaluations WHERE `+d.decided+` GROUP BY cluster_id, target`)
	if err != nil {
		return 0, fmt.Errorf("prune baselines: %w", err)
	}
	var pairs []baselinePair
	for rows.Next() {
		var bp baselinePair
		if err := rows.Scan(&bp.clusterID, &bp.target, &bp.id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("prune baselines: %w", err)
		}
		pairs = append(pairs, bp)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("prune baselines: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("prune baselines: %w", err)
	}
	var total int64
	for ids := baselines.unspared(pairs); len(ids) > 0; {
		n := min(len(ids), pruneChunk)
		args := []any{at}
		holders := make([]string, 0, n)
		for i, id := range ids[:n] {
			args = append(args, id)
			holders = append(holders, d.ph(i+2))
		}
		ids = ids[n:]
		gone, err := d.db.ExecContext(ctx, `
			DELETE FROM evaluations WHERE created_at < `+d.ph(1)+`
			AND snapshot_id NOT IN (SELECT MAX(id) FROM snapshots GROUP BY cluster_id)
			AND id IN (`+strings.Join(holders, ",")+`)`, args...)
		if err != nil {
			return total, fmt.Errorf("prune baselines: %w", err)
		}
		k, err := gone.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("prune baselines: %w", err)
		}
		total += k
		if k > 0 && t.onBatch != nil {
			t.onBatch(PruneBatch{Table: "evaluations", Rows: k})
		}
	}
	return total, nil
}
