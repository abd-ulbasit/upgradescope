package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// readDataset returns the entries of a previously written apilifecycle.json,
// or nil if there is none yet (first run).
func readDataset(path string) ([]entry, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc output
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc.Entries, nil
}

// carryForward keeps previously generated entries that a fresh extraction
// no longer produces, so a group/version upstream deletes (k8s.io/api v0.37
// deleted scheduling/v1alpha2) cannot silently vanish from the KB — a
// manifest still using it must keep failing the scan.
//
//   - GVK in gen: the fresh entry wins.
//   - GVK in nonPersisted: dropped, so a dataset written before the
//     exclusion existed loses it on the next run.
//   - GVK still registered upstream but without lifecycle data: the
//     previous entry is kept unchanged (no evidence it was removed).
//   - GVK gone from upstream: tombstoned. If it never carried a removal
//     version, or one after removedAt (the k8s.io/api minor that dropped
//     it — a deleted type is not served), removed is set to removedAt and
//     marked removedInferred; a documented or inferred removal at or
//     before removedAt is kept as-is, so reruns are idempotent.
//
// It returns gen followed by the carried entries, and the subset of those
// that are gone from upstream (for logging).
func carryForward(prev, gen []entry, upstream map[gvkOut]bool, removedAt version) (out, tombstoned []entry) {
	fresh := make(map[gvkOut]bool, len(gen))
	for _, e := range gen {
		fresh[e.gvk()] = true
	}
	out = append([]entry(nil), gen...)
	for _, p := range prev {
		if fresh[p.gvk()] || isNonPersisted(p.gvk()) {
			continue
		}
		if !upstream[p.gvk()] {
			if p.Removed == nil || removedAt.before(*p.Removed) {
				at := removedAt
				p.Removed = &at
				p.RemovedInferred = true
			}
			tombstoned = append(tombstoned, p)
		}
		out = append(out, p)
	}
	return out, tombstoned
}
