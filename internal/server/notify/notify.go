// Package notify defines the notification seam fired on finding deltas
// (new blocker, became-ready, add-on entering the EOL window) — never on
// every snapshot. Implementations (Slack, generic webhook) live here too.
//
// The server computes *what* changed (delta rules live in internal/server)
// and groups one evaluation pass's changes for one cluster into a single
// Notification; this package only knows how to deliver one somewhere.
// Delivery is at-least-once (the server's outbox retries), so receivers
// deduplicate by DeliveryID.
package notify

import (
	"context"
	"log/slog"
	"time"
)

// Change kinds (Change.Kind), the only three the delta rules emit.
const (
	KindNewBlocker     = "new-blocker"
	KindEOLApproaching = "eol-approaching"
	KindBecameReady    = "became-ready"
)

// SchemaVersion is the Notification payload version. A field is only ever
// added within a version; a rename or removal bumps it.
const SchemaVersion = 1

// TypeReadinessChanged is Notification.Type: one evaluation pass changed a
// cluster's readiness (new blockers, add-ons nearing EOL, or ready again).
const TypeReadinessChanged = "readiness.changed"

// Notification is one delivery: every change one evaluation pass found
// for one cluster, across all of its targets. It is the generic webhook's
// JSON body, documented in docs/operations.md.
type Notification struct {
	SchemaVersion int       `json:"schemaVersion"`
	DeliveryID    string    `json:"deliveryId"` // stable across retries: deduplicate on it
	Type          string    `json:"type"`       // TypeReadinessChanged
	Timestamp     time.Time `json:"timestamp"`  // when the pass ran (UTC)
	Cluster       Cluster   `json:"cluster"`
	// Targets is the verdict after the pass for every target with a change.
	Targets []Target `json:"targets"`
	// Changes is the findings delta. A change found for several targets
	// (an EOL add-on is a blocker for every target) is one entry listing
	// them all.
	Changes []Change `json:"changes"`
	// Omitted counts changes left out by the per-kind cap, by kind.
	Omitted map[string]int `json:"omitted,omitempty"`
}

// Cluster identifies the cluster a notification is about.
type Cluster struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Target is one target's verdict after the pass.
type Target struct {
	Target   string `json:"target"`
	Verdict  string `json:"verdict"` // ready | blocked | unknown
	Score    int    `json:"score"`
	Blockers int    `json:"blockers"`
}

// Change is one entry of the findings delta.
type Change struct {
	Kind     string   `json:"kind"`               // KindNewBlocker | KindEOLApproaching | KindBecameReady
	Key      string   `json:"key,omitempty"`      // the finding's stable key (empty for became-ready)
	Severity string   `json:"severity,omitempty"` // the finding's severity: blocker | warning
	Title    string   `json:"title"`
	Detail   string   `json:"detail,omitempty"`
	Targets  []string `json:"targets"` // the targets the change applies to
}

type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// Multi fans one notification out to every notifier. Individual failures
// are logged and skipped — the remaining notifiers still fire and the
// error is never propagated. The server does not use it to deliver (it
// queues one outbox message per member, see Members); Multi() with no
// arguments is a valid no-op.
func Multi(notifiers ...Notifier) Notifier { return multi(notifiers) }

type multi []Notifier

// Members flattens n into the individual notifiers it fans out to: a
// Multi's members (recursively), nothing for nil, otherwise n itself. The
// server queues one delivery per member so each is retried on its own.
func Members(n Notifier) []Notifier {
	switch m := n.(type) {
	case nil:
		return nil
	case multi:
		var out []Notifier
		for _, x := range m {
			out = append(out, Members(x)...)
		}
		return out
	default:
		return []Notifier{n}
	}
}

func (m multi) Notify(ctx context.Context, n Notification) error {
	for _, x := range m {
		if err := x.Notify(ctx, n); err != nil {
			slog.Warn("notifier failed", "cluster", n.Cluster.Name, "delivery", n.DeliveryID, "err", err)
		}
	}
	return nil
}

// NopNotifier discards every notification. Useful default when no webhook is configured.
type NopNotifier struct{}

func (NopNotifier) Notify(context.Context, Notification) error { return nil }
