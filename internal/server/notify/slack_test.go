package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testEvent() Event {
	return Event{
		Cluster: "prod-eu-1",
		Target:  "1.37",
		Kind:    KindNewBlocker,
		Title:   "policy/v1beta1 PodSecurityPolicy removed",
		Detail:  "3 objects in 2 namespaces",
	}
}

func TestSlackPostsFormattedText(t *testing.T) {
	var gotBody []byte
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := NewSlack(srv.URL).Notify(context.Background(), testEvent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	var payload map[string]string
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, gotBody)
	}
	want := "[upgradescope] prod-eu-1 → 1.37: new-blocker: policy/v1beta1 PodSecurityPolicy removed"
	if payload["text"] != want {
		t.Errorf("text = %q\nwant   %q", payload["text"], want)
	}
	if len(payload) != 1 {
		t.Errorf("payload has extra keys: %v", payload)
	}
}

// TestSlackEscapesControlCharacters: Slack treats &, < and > as control
// characters in message text (https://docs.slack.dev/messaging/formatting-message-text),
// so a title like "deployments <scale>" would render mangled (or be
// interpreted as markup) unless escaped.
func TestSlackEscapesControlCharacters(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ev := Event{
		Cluster: "prod & staging",
		Target:  "1.37",
		Kind:    KindNewBlocker,
		Title:   "clients still requesting apps/v1beta2 deployments <scale>",
	}
	if err := NewSlack(srv.URL).Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, gotBody)
	}
	want := "[upgradescope] prod &amp; staging → 1.37: new-blocker: clients still requesting apps/v1beta2 deployments &lt;scale&gt;"
	if payload["text"] != want {
		t.Errorf("text = %q\nwant   %q", payload["text"], want)
	}
}

func TestSlackDefaultTimeoutIsTwoSeconds(t *testing.T) {
	if got := NewSlack("http://example.invalid").Client.Timeout; got != 2*time.Second {
		t.Fatalf("default timeout = %v, want 2s", got)
	}
}

func TestSlackTimesOutOnSlowServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Same code path as the 2s default; shortened so the test stays fast.
	n := &SlackNotifier{URL: srv.URL, Client: &http.Client{Timeout: 50 * time.Millisecond}}
	if err := n.Notify(context.Background(), testEvent()); err == nil {
		t.Fatal("want timeout error from slow webhook, got nil")
	}
}

func TestSlackNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no_service", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := NewSlack(srv.URL).Notify(context.Background(), testEvent())
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("want status-500 error, got %v", err)
	}
}

func TestGenericWebhookPostsEventJSON(t *testing.T) {
	var gotBody []byte
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted) // any 2xx is success
	}))
	defer srv.Close()

	ev := testEvent()
	if err := NewGenericWebhook(srv.URL).Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	var got Event
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("payload not Event JSON: %v (%s)", err, gotBody)
	}
	if got != ev {
		t.Errorf("round-tripped event = %+v, want %+v", got, ev)
	}
}

func TestGenericWebhookNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	err := NewGenericWebhook(srv.URL).Notify(context.Background(), testEvent())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("want status-403 error, got %v", err)
	}
}
