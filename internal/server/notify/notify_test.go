package notify

import (
	"context"
	"errors"
	"testing"
)

// fakeNotifier records every event it receives and optionally fails.
type fakeNotifier struct {
	events []Event
	err    error
}

func (f *fakeNotifier) Notify(_ context.Context, ev Event) error {
	f.events = append(f.events, ev)
	return f.err
}

func TestMultiFansOutToAll(t *testing.T) {
	a, b := &fakeNotifier{}, &fakeNotifier{}
	ev := Event{Cluster: "prod-eu-1", Target: "1.37", Kind: KindNewBlocker, Title: "x removed"}

	if err := Multi(a, b).Notify(context.Background(), ev); err != nil {
		t.Fatalf("Multi.Notify: %v", err)
	}
	if len(a.events) != 1 || len(b.events) != 1 {
		t.Fatalf("want 1 event each, got a=%d b=%d", len(a.events), len(b.events))
	}
	if a.events[0] != ev || b.events[0] != ev {
		t.Fatalf("event mutated in fan-out: a=%+v b=%+v", a.events[0], b.events[0])
	}
}

func TestMultiContinuesPastFailures(t *testing.T) {
	failing := &fakeNotifier{err: errors.New("boom")}
	ok := &fakeNotifier{}
	ev := Event{Cluster: "c", Target: "1.36", Kind: KindBecameReady, Title: "ready"}

	// A failing notifier must be logged-and-skipped: the healthy one still
	// fires and Multi never propagates the error (ingestion must not block).
	if err := Multi(failing, ok).Notify(context.Background(), ev); err != nil {
		t.Fatalf("Multi must swallow individual failures, got %v", err)
	}
	if len(ok.events) != 1 {
		t.Fatalf("healthy notifier skipped after earlier failure: got %d events", len(ok.events))
	}
}

func TestMultiEmptyIsHarmless(t *testing.T) {
	if err := Multi().Notify(context.Background(), Event{}); err != nil {
		t.Fatalf("empty Multi: %v", err)
	}
}

func TestNopNotifier(t *testing.T) {
	if err := (NopNotifier{}).Notify(context.Background(), Event{Kind: KindNewBlocker}); err != nil {
		t.Fatalf("NopNotifier: %v", err)
	}
}
