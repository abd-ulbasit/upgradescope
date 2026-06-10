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
