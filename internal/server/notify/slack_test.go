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

// testNotification is a one-change notification.
func testNotification() Notification {
	return Notification{
		SchemaVersion: SchemaVersion,
		DeliveryID:    "d1",
		Type:          TypeReadinessChanged,
		Cluster:       Cluster{ID: 1, Name: "prod-eu-1"},
		Targets:       []Target{{Target: "1.37", Verdict: "blocked", Score: 75, Blockers: 1}},
		Changes: []Change{{
			Kind: KindNewBlocker, Key: "removed-api/policy/v1beta1/PodSecurityPolicy", Severity: "blocker",
			Title: "policy/v1beta1 PodSecurityPolicy removed", Detail: "3 objects in 2 namespaces", Targets: []string{"1.37"},
		}},
	}
}

// slackText posts n to a Slack notifier and returns the message text.
func slackText(t *testing.T, n Notification) string {
	t.Helper()
	var gotBody []byte
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := NewSlack(srv.URL).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	var payload map[string]string
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, gotBody)
	}
	if len(payload) != 1 {
		t.Errorf("payload has extra keys: %v", payload)
	}
	return payload["text"]
}

func TestSlackPostsFormattedText(t *testing.T) {
	want := "[upgradescope] prod-eu-1 → 1.37: new-blocker: policy/v1beta1 PodSecurityPolicy removed"
	if got := slackText(t, testNotification()); got != want {
		t.Errorf("text = %q\nwant   %q", got, want)
	}
}

// TestSlackGroupsChanges: one message per notification, one line per
// change, the targets a change applies to on its line, and the capped
// remainder summarized.
func TestSlackGroupsChanges(t *testing.T) {
	n := testNotification()
	n.Changes = append(n.Changes, Change{Kind: KindNewBlocker, Title: "ingress-nginx is end-of-life", Targets: []string{"1.36", "1.37"}})
	n.Omitted = map[string]int{KindNewBlocker: 4}
	want := "[upgradescope] prod-eu-1: 6 changes\n" +
		"• 1.37: new-blocker: policy/v1beta1 PodSecurityPolicy removed\n" +
		"• 1.36, 1.37: new-blocker: ingress-nginx is end-of-life\n" +
		"• and 4 more new-blocker"
	if got := slackText(t, n); got != want {
		t.Errorf("text = %q\nwant   %q", got, want)
	}
}

// TestSlackEscapesControlCharacters: Slack treats &, < and > as control
// characters in message text (https://docs.slack.dev/messaging/formatting-message-text),
// so a title like "deployments <scale>" would render mangled (or be
// interpreted as markup) unless escaped.
func TestSlackEscapesControlCharacters(t *testing.T) {
	n := testNotification()
	n.Cluster.Name = "prod & staging"
	n.Changes[0].Title = "clients still requesting apps/v1beta2 deployments <scale>"
	want := "[upgradescope] prod &amp; staging → 1.37: new-blocker: clients still requesting apps/v1beta2 deployments &lt;scale&gt;"
	if got := slackText(t, n); got != want {
		t.Errorf("text = %q\nwant   %q", got, want)
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
	if err := n.Notify(context.Background(), testNotification()); err == nil {
		t.Fatal("want timeout error from slow webhook, got nil")
	}
}

func TestSlackNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no_service", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := NewSlack(srv.URL).Notify(context.Background(), testNotification())
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("want status-500 error, got %v", err)
	}
}

func TestGenericWebhookPostsNotificationJSON(t *testing.T) {
	var gotBody []byte
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted) // any 2xx is success
	}))
	defer srv.Close()

	n := testNotification()
	if err := NewGenericWebhook(srv.URL).Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	var got Notification
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("payload not Notification JSON: %v (%s)", err, gotBody)
	}
	if got.DeliveryID != n.DeliveryID || got.Cluster != n.Cluster || len(got.Changes) != 1 || got.Changes[0].Title != n.Changes[0].Title {
		t.Errorf("round-tripped notification = %+v, want %+v", got, n)
	}
}

func TestGenericWebhookNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	err := NewGenericWebhook(srv.URL).Notify(context.Background(), testNotification())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("want status-403 error, got %v", err)
	}
}
