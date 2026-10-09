package server

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
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
