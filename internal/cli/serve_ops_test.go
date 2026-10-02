package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server"
)

// The admin token is a secret like the others: flag, env or file.
func TestServeAdminToken(t *testing.T) {
	var got serveOptions
	capture := func(_ context.Context, opts serveOptions) error {
		got = opts
		return nil
	}
	if err := execServe(t, nil, capture); err != nil || got.adminToken != "" {
		t.Fatalf("default admin token = %q (err %v), want empty: administration off", got.adminToken, err)
	}
	t.Setenv("UPGRADESCOPE_ADMIN_TOKEN", "env-admin")
	if err := execServe(t, nil, capture); err != nil || got.adminToken != "env-admin" {
		t.Fatalf("admin token from env = %q (err %v)", got.adminToken, err)
	}
	p := filepath.Join(t.TempDir(), "admin")
	if err := os.WriteFile(p, []byte("file-admin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := execServe(t, []string{"--admin-token-file", p}, capture); err != nil || got.adminToken != "file-admin" {
		t.Fatalf("admin token from file = %q (err %v)", got.adminToken, err)
	}
}

func TestServeStaleAfter(t *testing.T) {
	var got serveOptions
	capture := func(_ context.Context, opts serveOptions) error {
		got = opts
		return nil
	}
	if err := execServe(t, nil, capture); err != nil || got.staleAfter != server.DefaultStaleAfter {
		t.Fatalf("default --stale-after = %v (err %v), want %v", got.staleAfter, err, server.DefaultStaleAfter)
	}
	if err := execServe(t, []string{"--stale-after", "3h"}, capture); err != nil || got.staleAfter != 3*time.Hour {
		t.Fatalf("--stale-after 3h = %v (err %v)", got.staleAfter, err)
	}
	if err := execServe(t, []string{"--stale-after", "0s"}, serveOK()); err == nil || !strings.Contains(err.Error(), "--stale-after") {
		t.Errorf("--stale-after 0s: err = %v, want a refusal", err)
	}
}

// --retention takes days ("90d") or a Go duration; 0 keeps everything.
func TestServeRetention(t *testing.T) {
	var got serveOptions
	capture := func(_ context.Context, opts serveOptions) error {
		got = opts
		return nil
	}
	for _, tc := range []struct {
		arg  string
		want time.Duration
	}{
		{"", 90 * 24 * time.Hour}, // default
		{"30d", 30 * 24 * time.Hour},
		{"720h", 720 * time.Hour},
		{"0", 0},
		{"0d", 0},
	} {
		args := []string{}
		if tc.arg != "" {
			args = []string{"--retention", tc.arg}
		}
		if err := execServe(t, args, capture); err != nil || got.parsedRetention != tc.want {
			t.Errorf("--retention %q = %v (err %v), want %v", tc.arg, got.parsedRetention, err, tc.want)
		}
	}
	for _, bad := range []string{"-1d", "90", "ninety", "1.5d", "-5h", "30m"} {
		if err := execServe(t, []string{"--retention", bad}, serveOK()); err == nil || !strings.Contains(err.Error(), "--retention") {
			t.Errorf("--retention %q: err = %v, want a refusal", bad, err)
		}
	}
}

// The webhook signing key is a secret (env/file), and is meaningless
// without the webhook it signs.
func TestServeWebhookSecret(t *testing.T) {
	var got serveOptions
	capture := func(_ context.Context, opts serveOptions) error {
		got = opts
		return nil
	}
	t.Setenv("UPGRADESCOPE_WEBHOOK_SECRET", "env-key")
	if err := execServe(t, []string{"--webhook", "https://hook.test/x"}, capture); err != nil || got.webhookKey != "env-key" {
		t.Fatalf("webhook secret from env = %q (err %v)", got.webhookKey, err)
	}
	if err := execServe(t, nil, serveOK()); err == nil || !strings.Contains(err.Error(), "--webhook") {
		t.Errorf("secret without --webhook: err = %v, want a refusal", err)
	}
}

// Reusing the read or ingest token as the admin token would hand cluster
// deletion to every dashboard user or agent.
func TestServeRefusesSharedAdminToken(t *testing.T) {
	for _, args := range [][]string{
		{"--read-token", "same", "--admin-token", "same"},
		{"--ingest-token", "same", "--admin-token", "same"},
	} {
		err := execServe(t, args, serveOK())
		if err == nil || !strings.Contains(err.Error(), "--admin-token") {
			t.Errorf("serve %v: err = %v, want a refusal naming --admin-token", args, err)
		}
	}
}
