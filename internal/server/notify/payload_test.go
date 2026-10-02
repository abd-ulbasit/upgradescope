package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

// goldenNotification exercises every field of the payload.
func goldenNotification() Notification {
	return Notification{
		SchemaVersion: SchemaVersion,
		DeliveryID:    "3f2b8c1e9a7d4e6f8b0c2d4e6f8a0b1c",
		Type:          TypeReadinessChanged,
		Timestamp:     time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC),
		Cluster:       Cluster{ID: 7, Name: "prod-eu-1"},
		Targets: []Target{
			{Target: "1.35", Verdict: "blocked", Score: 75, Blockers: 1},
			{Target: "1.36", Verdict: "blocked", Score: 50, Blockers: 2},
		},
		Changes: []Change{
			{Kind: KindNewBlocker, Key: "eol-addon/ingress-nginx", Severity: "blocker",
				Title: "ingress-nginx is end-of-life", Detail: "retired upstream in March 2026", Targets: []string{"1.35", "1.36"}},
			{Kind: KindEOLApproaching, Key: "eol-approaching/istio", Severity: "warning",
				Title: "Istio 1.24 reaches end-of-life on 2026-11-30", Targets: []string{"1.36"}},
		},
		Omitted: map[string]int{KindNewBlocker: 3},
	}
}

// TestNotificationJSONGolden pins the webhook payload contract: versioned,
// camelCase, with a delivery id, a timestamp, the event type, the cluster,
// the targets and the findings delta. Renaming a Go field must not change
// it silently.
func TestNotificationJSONGolden(t *testing.T) {
	got, err := json.MarshalIndent(goldenNotification(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "notification_v1.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("payload differs from %s:\n%s", path, got)
	}
	for _, key := range []string{`"schemaVersion": 1`, `"deliveryId"`, `"timestamp"`, `"type": "readiness.changed"`, `"cluster"`, `"targets"`, `"changes"`} {
		if !strings.Contains(string(got), key) {
			t.Errorf("payload lacks %s", key)
		}
	}
}

// TestGenericWebhookSignsBody: with a secret, every request carries
// X-Upgradescope-Signature: sha256=<hex HMAC-SHA256(secret, body)>, which a
// receiver verifies over the raw body; without one there is no signature.
func TestGenericWebhookSignsBody(t *testing.T) {
	var body []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
	}))
	defer srv.Close()

	n := goldenNotification()
	hook := NewGenericWebhook(srv.URL)
	hook.Secret = "s3cret"
	if err := hook.Notify(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := hdr.Get(SignatureHeader); got != want {
		t.Errorf("%s = %q, want %q", SignatureHeader, got, want)
	}
	if got := Sign("s3cret", body); got != want {
		t.Errorf("Sign = %q, want %q", got, want)
	}
	if hdr.Get("X-Upgradescope-Delivery") != n.DeliveryID || hdr.Get("X-Upgradescope-Event") != TypeReadinessChanged {
		t.Errorf("delivery/event headers = %q/%q", hdr.Get("X-Upgradescope-Delivery"), hdr.Get("X-Upgradescope-Event"))
	}
	var got Notification
	if err := json.Unmarshal(body, &got); err != nil || got.DeliveryID != n.DeliveryID || len(got.Changes) != 2 {
		t.Errorf("body = %s (%v), want the notification", body, err)
	}

	if err := NewGenericWebhook(srv.URL).Notify(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if got := hdr.Get(SignatureHeader); got != "" {
		t.Errorf("unsigned webhook sent %s: %q", SignatureHeader, got)
	}
}

// TestNotifierRedirectIsFailure: a redirect is not a delivery. Following it
// turned the POST into a bodyless GET that "succeeded", and the outbox
// message was deleted with nothing delivered or logged.
func TestNotifierRedirectIsFailure(t *testing.T) {
	var followed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Add(1) }))
	defer target.Close()
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		redirect := httptest.NewServer(http.RedirectHandler(target.URL, code))
		for name, n := range map[string]Notifier{"slack": NewSlack(redirect.URL), "webhook": NewGenericWebhook(redirect.URL)} {
			if err := n.Notify(context.Background(), goldenNotification()); err == nil || !strings.Contains(err.Error(), "status") {
				t.Errorf("%s answered %d: err = %v, want a status error", name, code, err)
			}
		}
		redirect.Close()
	}
	if n := followed.Load(); n != 0 {
		t.Errorf("redirects were followed %d times", n)
	}
}
