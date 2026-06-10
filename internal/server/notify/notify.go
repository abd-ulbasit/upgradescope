// Package notify defines the notification seam fired on finding deltas
// (new blocker, became-ready, add-on entering the EOL window) — never on
// every snapshot. Implementations (Slack, generic webhook) live here too.
package notify

import "context"

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
