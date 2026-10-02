package notify

import (
	"context"
	"errors"
	"testing"
)

// fakeNotifier records every notification it receives and optionally fails.
type fakeNotifier struct {
	got []Notification
	err error
}

func (f *fakeNotifier) Notify(_ context.Context, n Notification) error {
	f.got = append(f.got, n)
	return f.err
}

func TestMultiFansOutToAll(t *testing.T) {
	a, b := &fakeNotifier{}, &fakeNotifier{}
	n := testNotification()

	if err := Multi(a, b).Notify(context.Background(), n); err != nil {
		t.Fatalf("Multi.Notify: %v", err)
	}
	if len(a.got) != 1 || len(b.got) != 1 {
		t.Fatalf("want 1 notification each, got a=%d b=%d", len(a.got), len(b.got))
	}
	if a.got[0].DeliveryID != n.DeliveryID || b.got[0].DeliveryID != n.DeliveryID {
		t.Fatalf("notification mutated in fan-out: a=%+v b=%+v", a.got[0], b.got[0])
	}
}

func TestMultiContinuesPastFailures(t *testing.T) {
	failing := &fakeNotifier{err: errors.New("boom")}
	ok := &fakeNotifier{}

	// A failing notifier must be logged-and-skipped: the healthy one still
	// fires and Multi never propagates the error.
	if err := Multi(failing, ok).Notify(context.Background(), testNotification()); err != nil {
		t.Fatalf("Multi must swallow individual failures, got %v", err)
	}
	if len(ok.got) != 1 {
		t.Fatalf("healthy notifier skipped after earlier failure: got %d notifications", len(ok.got))
	}
}

func TestMultiEmptyIsHarmless(t *testing.T) {
	if err := Multi().Notify(context.Background(), Notification{}); err != nil {
		t.Fatalf("empty Multi: %v", err)
	}
}

func TestMembersFlattensMulti(t *testing.T) {
	a, b, c := &fakeNotifier{}, &fakeNotifier{}, &fakeNotifier{}
	got := Members(Multi(a, Multi(b, c)))
	if len(got) != 3 || got[0] != a || got[1] != b || got[2] != c {
		t.Fatalf("Members(Multi(a, Multi(b, c))) = %v, want [a b c] in order", got)
	}
	if got := Members(a); len(got) != 1 || got[0] != a {
		t.Fatalf("Members(a) = %v, want [a]", got)
	}
	if got := Members(nil); len(got) != 0 {
		t.Fatalf("Members(nil) = %v, want none", got)
	}
	if got := Members(Multi()); len(got) != 0 {
		t.Fatalf("Members(Multi()) = %v, want none", got)
	}
}

func TestNopNotifier(t *testing.T) {
	if err := (NopNotifier{}).Notify(context.Background(), testNotification()); err != nil {
		t.Fatalf("NopNotifier: %v", err)
	}
}
