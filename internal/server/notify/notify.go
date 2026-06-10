// Package notify defines the notification seam fired on finding deltas
// (new blocker, became-ready, add-on entering the EOL window) — never on
// every snapshot. Implementations (Slack, generic webhook) live here too.
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
	Cluster string
	Target  string
	Kind    string // "new-blocker" | "eol-approaching" | "became-ready"
	Title   string // human line, e.g. finding title
	Detail  string
}

type Notifier interface {
	Notify(ctx context.Context, ev Event) error
}

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
