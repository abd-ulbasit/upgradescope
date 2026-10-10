package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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
		{"36500d", 36500 * 24 * time.Hour}, // the cap: 100 years
	} {
		args := []string{}
		if tc.arg != "" {
			args = []string{"--retention", tc.arg}
		}
		if err := execServe(t, args, capture); err != nil || got.parsedRetention != tc.want {
			t.Errorf("--retention %q = %v (err %v), want %v", tc.arg, got.parsedRetention, err, tc.want)
		}
	}
	// 106752d overflows time.Duration (it wrapped to a negative window).
	for _, bad := range []string{"-1d", "90", "ninety", "1.5d", "-5h", "30m", "106752d", "99999999999d", "36501d"} {
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

// One bearer that both pushes and reads would let every agent's shared
// push token read the whole fleet, and every reader push as any cluster
// (#250).
func TestServeRefusesReadTokenEqualToIngestToken(t *testing.T) {
	err := execServe(t, []string{"--read-token", "same", "--ingest-token", "same"}, serveOK())
	if err == nil || !strings.Contains(err.Error(), "--read-token") || !strings.Contains(err.Error(), "--ingest-token") {
		t.Errorf("serve with equal read and ingest tokens: err = %v, want a refusal naming both", err)
	}
	if err := execServe(t, []string{"--read-token", "r", "--ingest-token", "i"}, serveOK()); err != nil {
		t.Errorf("distinct tokens: %v", err)
	}
}

// A notification URL that cannot be delivered to is a startup error, not
// eight silent retries per message (#240), and the error names the flag,
// never the URL, whose path is the Slack credential.
func TestServeValidatesNotificationURLs(t *testing.T) {
	for _, flag := range []string{"slack-webhook", "webhook"} {
		for _, bad := range []string{
			"hooks.slack.com/services/T0/B0/SECRETSECRET",
			"ftp://hooks.slack.com/services/T0/B0/SECRETSECRET",
			"https:///services/T0/B0/SECRETSECRET",
			"https://hooks.slack.com/services/T0/B0/SECRETSECRET\n",
			"/services/T0/B0/SECRETSECRET",
		} {
			err := execServe(t, []string{"--" + flag, bad}, serveOK())
			if err == nil || !strings.Contains(err.Error(), "invalid --"+flag+": want an absolute http(s) URL") {
				t.Errorf("--%s %q: err = %v, want the invalid-URL refusal", flag, bad, err)
				continue
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("--%s %q: the refusal echoes the URL: %v", flag, bad, err)
			}
		}
	}
	// From the environment, surrounding whitespace is trimmed first, as
	// from a file.
	t.Setenv("UPGRADESCOPE_SLACK_WEBHOOK", "https://hooks.slack.com/services/T0/B0/X\n")
	t.Setenv("UPGRADESCOPE_WEBHOOK_URL", "  http://hook.internal:8080/upgradescope?token=q\n")
	t.Setenv("UPGRADESCOPE_READ_TOKEN", "read-tok\n")
	var got serveOptions
	capture := func(_ context.Context, opts serveOptions) error { got = opts; return nil }
	if err := execServe(t, nil, capture); err != nil {
		t.Fatal(err)
	}
	if got.slackWebhook != "https://hooks.slack.com/services/T0/B0/X" || got.webhook != "http://hook.internal:8080/upgradescope?token=q" || got.readToken != "read-tok" {
		t.Errorf("env values not trimmed: slack %q webhook %q read token %q", got.slackWebhook, got.webhook, got.readToken)
	}
}

// --allowed-host (repeatable or comma separated, or
// $UPGRADESCOPE_ALLOWED_HOSTS when the flag is not given) names the hosts
// the Host guard answers for besides loopback; a bad entry fails the start.
func TestServeAllowedHosts(t *testing.T) {
	var got serveOptions
	capture := func(_ context.Context, opts serveOptions) error { got = opts; return nil }
	if err := execServe(t, []string{"--allowed-host", "Upgradescope.example.com", "--allowed-host", "upgradescope-server.upgradescope.svc,10.96.0.10"}, capture); err != nil {
		t.Fatal(err)
	}
	want := []string{"upgradescope.example.com", "upgradescope-server.upgradescope.svc", "10.96.0.10"}
	if !slices.Equal(got.allowedHosts, want) {
		t.Errorf("allowed hosts = %v, want %v", got.allowedHosts, want)
	}
	t.Setenv("UPGRADESCOPE_ALLOWED_HOSTS", "a.example, b.example\n")
	if err := execServe(t, nil, capture); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.allowedHosts, []string{"a.example", "b.example"}) {
		t.Errorf("allowed hosts from the environment = %v, want [a.example b.example]", got.allowedHosts)
	}
	if err := execServe(t, []string{"--allowed-host", "c.example"}, capture); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.allowedHosts, []string{"c.example"}) {
		t.Errorf("--allowed-host with the env var set = %v, want the flag's [c.example]", got.allowedHosts)
	}
	for _, bad := range []string{"https://a.example", "a.example:443", "a/b", "*.example.com"} {
		if err := execServe(t, []string{"--allowed-host", bad}, serveOK()); err == nil || !strings.Contains(err.Error(), "--allowed-host") {
			t.Errorf("--allowed-host %q: err = %v, want a refusal naming the flag", bad, err)
		}
	}
}

// --require-read-credential keeps the read API closed whatever the
// database holds (#295): the flag reaches the server, it is off by default,
// and it contradicts --allow-anonymous-read, which opens the API.
func TestServeRequireReadCredential(t *testing.T) {
	var got serveOptions
	capture := func(_ context.Context, opts serveOptions) error {
		got = opts
		return nil
	}
	if err := execServe(t, []string{"--ingest-token", "t"}, capture); err != nil {
		t.Fatal(err)
	}
	if got.requireReadCredential {
		t.Error("requireReadCredential must default to false")
	}
	if err := execServe(t, []string{"--ingest-token", "t", "--listen", ":8080", "--require-read-credential"}, capture); err != nil {
		t.Fatalf("--require-read-credential on an exposed address: %v, want it accepted (the API is not open)", err)
	}
	if !got.requireReadCredential {
		t.Error("--require-read-credential did not reach the options")
	}
	err := execServe(t, []string{"--ingest-token", "t", "--require-read-credential", "--allow-anonymous-read"}, serveOK())
	if err == nil || !strings.Contains(err.Error(), "--require-read-credential") || !strings.Contains(err.Error(), "--allow-anonymous-read") {
		t.Errorf("both flags: err = %v, want a refusal naming both", err)
	}
}

// With the real wiring, on every interface and an empty database, the flag
// starts the server where serve would otherwise refuse (see
// TestServeAnonymousReadGuard); it runs until its context ends, then exits
// cleanly. (The server package's tests cover the 401s over HTTP.)
func TestServeRequireReadCredentialStartsOnAnExposedAddress(t *testing.T) {
	cmd := newServeCmd() // runServe is the real wiring: no execServe stub
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"--listen", "0.0.0.0:0", "--db", filepath.Join(t.TempDir(), "closed.db"), "--require-read-credential"})
	if err := cmd.Execute(); err != nil {
		t.Errorf("serve --listen 0.0.0.0:0 --require-read-credential with an empty database: %v, want it to run until cancelled", err)
	}
}

// The first read token is minted by a second process against the database
// the running server already holds open (the chart's install notes tell the
// operator to `kubectl exec` the command into the server pod). With the real
// serve wiring on a SQLite file and --require-read-credential: a read is 401,
// `tokens create --read --teams '*'` on the same --db (its own store handle)
// succeeds while the server runs, and the printed token reads 200 on the next
// request with no restart.
func TestReadTokenMintedWhileServeHoldsTheDatabase(t *testing.T) {
	db := filepath.Join(t.TempDir(), "held.db")

	// A free port to hand to serve (the real wiring does not report the bound
	// address back); the race between closing and re-binding is accepted.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	cmd := newServeCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"--listen", fmt.Sprintf("0.0.0.0:%d", port), "--db", db, "--require-read-credential"})
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve exited with %v, want a clean stop", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not stop after its context was cancelled")
		}
	}
	defer stop()

	status := func(path, bearer string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	deadline := time.Now().Add(10 * time.Second)
	for status("/healthz", "") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatalf("serve never answered /healthz; its output:\n%s", out.String())
		}
		time.Sleep(25 * time.Millisecond)
	}

	if got := status("/api/v1/clusters", ""); got != http.StatusUnauthorized {
		t.Fatalf("anonymous read before any token exists = %d, want 401", got)
	}
	if got := status("/api/v1/clusters", strings.Repeat("a", 64)); got != http.StatusUnauthorized {
		t.Fatalf("unknown bearer before any token exists = %d, want 401", got)
	}

	// The second process: its own store handle on the file serve holds open.
	token, _ := createReadTokenCLI(t, db, "*")

	if got := status("/api/v1/clusters", token); got != http.StatusOK {
		t.Fatalf("the minted token against the running server = %d, want 200 with no restart", got)
	}
	if got := status("/api/v1/clusters", ""); got != http.StatusUnauthorized {
		t.Errorf("anonymous read after minting = %d, want 401 (the API stays closed)", got)
	}
	stop()
}
