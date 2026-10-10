package server

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// lastErrorStore records what each failed delivery stores as last_error.
type lastErrorStore struct {
	store.Store
	mu   sync.Mutex
	errs []string
}

func (s *lastErrorStore) RescheduleOutbox(ctx context.Context, id int64, next time.Time, lastErr string) error {
	s.mu.Lock()
	s.errs = append(s.errs, lastErr)
	s.mu.Unlock()
	return s.Store.RescheduleOutbox(ctx, id, next, lastErr)
}

// A Slack or generic webhook that cannot be reached used to put its whole
// URL, the Slack credential and a webhook's ?token= included, in every
// retry's log line and in outbox.last_error (#240). Neither carries
// anything past the scheme and host now, which still appear.
func TestOutboxNeverLogsOrStoresSinkURLs(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := ln.Addr().String()
	ln.Close()
	slack := notify.NewSlack("http://" + refused + "/services/T000/B000/SECRETSECRET")
	hook := notify.NewGenericWebhook("http://" + refused + "/hooks/SECRETPATH?token=SECRETQ")
	hook.Secret = "SECRETHMAC"
	logged := captureLog(t)
	st := &lastErrorStore{Store: openSQLite(t)}
	clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
	s := newTestServer(t, nil, func(c *Config) { c.Store = st; c.Notifier = notify.Multi(slack, hook) })
	s.now = clock.now
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	blockedThenClean(t, ts)

	for range outboxMaxAttempts + 1 { // every retry, then the give-up
		s.deliverOutbox(context.Background())
		clock.set(clock.now().Add(outboxMaxBackoff + time.Minute))
	}
	st.mu.Lock()
	stored := strings.Join(st.errs, "\n")
	st.mu.Unlock()
	if len(st.errs) < 2 {
		t.Fatalf("only %d failed deliveries were stored", len(st.errs))
	}
	for what, text := range map[string]string{"the log": logged.String(), "last_error": stored} {
		if strings.Contains(text, "SECRET") {
			t.Errorf("%s carries a sink's secret:\n%s", what, text)
		}
		if !strings.Contains(text, "http://"+refused+"/…") {
			t.Errorf("%s does not name the sink's scheme and host:\n%s", what, text)
		}
	}
	if !strings.Contains(logged.String(), "giving up") {
		t.Errorf("the log has no give-up line:\n%s", logged)
	}
}

// rawURLNotifier is a sink whose errors repeat its URL as written and as
// Go quotes it, as a notifier that does not redact its own errors would.
type rawURLNotifier struct {
	url   string
	urlFn func() string // when set, the URL now, as a sink that follows a mounted file gives it
}

func (r *rawURLNotifier) SinkURL() string {
	if r.urlFn != nil {
		return r.urlFn()
	}
	return r.url
}

func (r *rawURLNotifier) Pin() (notify.Notifier, string) {
	u := r.SinkURL()
	return &rawURLNotifier{url: u}, u
}

func (r *rawURLNotifier) Notify(context.Context, notify.Notification) error {
	u := r.SinkURL()
	return fmt.Errorf("post %s: refused (Post %q)", u, u)
}

// The outbox scrubs a sink's URL from every error it logs or stores, not
// only from the ones the Slack and webhook notifiers already redact: a
// sink whose error carries its URL raw leaves neither its path nor its
// query in the log or in last_error.
func TestOutboxScrubsTheSinkURLFromAnyError(t *testing.T) {
	raw := &rawURLNotifier{url: "https://hooks.example.com/services/T000/B000/SECRETSECRET?token=SECRETQ"}
	logged := captureLog(t)
	st := &lastErrorStore{Store: openSQLite(t)}
	clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
	s := newTestServer(t, nil, func(c *Config) { c.Store = st; c.Notifier = raw })
	s.now = clock.now
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	blockedThenClean(t, ts)

	for range outboxMaxAttempts + 1 { // every retry, then the give-up
		s.deliverOutbox(context.Background())
		clock.set(clock.now().Add(outboxMaxBackoff + time.Minute))
	}
	st.mu.Lock()
	stored := strings.Join(st.errs, "\n")
	st.mu.Unlock()
	if len(st.errs) < 2 {
		t.Fatalf("only %d failed deliveries were stored", len(st.errs))
	}
	for what, text := range map[string]string{"the log": logged.String(), "last_error": stored} {
		if strings.Contains(text, "SECRET") {
			t.Errorf("%s carries the sink's secret:\n%s", what, text)
		}
		if !strings.Contains(text, "https://hooks.example.com/…") {
			t.Errorf("%s does not name the sink's scheme and host:\n%s", what, text)
		}
	}
	if !strings.Contains(logged.String(), "giving up") {
		t.Errorf("the log has no give-up line:\n%s", logged)
	}
}

// A sink whose URL follows a rotating file can change it between the post
// and the scrub. The delivery reads the URL once and scrubs with the one it
// posted to, so an error that names the old URL raw leaves nothing of it in
// the log or in last_error even when the file has moved on.
func TestOutboxScrubsWithTheURLItPostedTo(t *testing.T) {
	var reads atomic.Int32
	rotating := &rawURLNotifier{urlFn: func() string {
		if reads.Add(1) == 1 {
			return "https://hooks.example.com/services/OLDSECRETOLD"
		}
		return "https://hooks.example.com/services/NEWSECRETNEW"
	}}
	logged := captureLog(t)
	st := &lastErrorStore{Store: openSQLite(t)}
	clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
	s := newTestServer(t, nil, func(c *Config) { c.Store = st; c.Notifier = rotating })
	s.now = clock.now
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	blockedThenClean(t, ts)

	s.deliverOutbox(context.Background())
	st.mu.Lock()
	stored := strings.Join(st.errs, "\n")
	st.mu.Unlock()
	if stored == "" {
		t.Fatal("no failed delivery was stored")
	}
	for what, text := range map[string]string{"the log": logged.String(), "last_error": stored} {
		if strings.Contains(text, "SECRET") {
			t.Errorf("%s carries a sink's secret:\n%s", what, text)
		}
	}
}
