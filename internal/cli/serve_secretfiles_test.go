package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSecret(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A secret given as --*-file is handed to the server as a path it follows;
// the same secret from a flag or the environment is a fixed value (it cannot
// rotate).
func TestServePassesSecretFilesForRotation(t *testing.T) {
	dir := t.TempDir()
	var got serveOptions
	capture := func(_ context.Context, o serveOptions) error { got = o; return nil }
	ingest := writeSecret(t, dir, "ingestToken", "i-tok\n")
	read := writeSecret(t, dir, "readToken", "r-tok\n")
	admin := writeSecret(t, dir, "adminToken", "a-tok\n")
	slack := writeSecret(t, dir, "slackWebhook", "https://hooks.example/s\n")
	hook := writeSecret(t, dir, "webhook", "https://hooks.example/w\n")
	key := writeSecret(t, dir, "webhookSecret", "k\n")
	err := execServe(t, []string{
		"--ingest-token-file", ingest, "--read-token-file", read, "--admin-token-file", admin,
		"--slack-webhook-file", slack, "--webhook-file", hook, "--webhook-secret-file", key,
	}, capture)
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]string{
		"ingest": {got.ingestTokenFile, ingest}, "read": {got.readTokenFile, read}, "admin": {got.adminTokenFile, admin},
		"slack": {got.slackWebhookFile, slack}, "webhook": {got.webhookFile, hook}, "webhook key": {got.webhookKeyFile, key},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s file = %q, want %q", name, pair[0], pair[1])
		}
	}
	if got.readToken != "r-tok" || got.webhook != "https://hooks.example/w" {
		t.Errorf("initial values not read: read %q webhook %q", got.readToken, got.webhook)
	}

	t.Setenv("UPGRADESCOPE_READ_TOKEN", "env-read")
	if err := execServe(t, []string{"--ingest-token", "flag-ingest"}, capture); err != nil {
		t.Fatal(err)
	}
	if got.readTokenFile != "" || got.ingestTokenFile != "" || got.readToken != "env-read" || got.ingestToken != "flag-ingest" {
		t.Errorf("env and flag sources must not claim a file: %+v", got)
	}
}

func TestServeOptionalSecretFile(t *testing.T) {
	dir := t.TempDir()
	var got serveOptions
	capture := func(_ context.Context, o serveOptions) error { got = o; return nil }
	missing := filepath.Join(dir, "absent")

	// Without --optional-secret-file a missing file is the start-up error it was.
	if err := execServe(t, []string{"--ingest-token-file", missing}, capture); err == nil {
		t.Fatal("a missing --ingest-token-file without --optional-secret-file must fail")
	}
	if err := execServe(t, []string{"--ingest-token-file", missing, "--optional-secret-file", "ingest-token"}, capture); err != nil {
		t.Fatalf("optional missing ingest token file: %v", err)
	}
	if got.ingestToken != "" || got.ingestTokenFile != missing || len(got.optionalSecretFile) != 1 {
		t.Errorf("got %+v", got)
	}
	// An empty optional file is an absent key; an empty required one is an error.
	empty := writeSecret(t, dir, "empty", " \n")
	if err := execServe(t, []string{"--ingest-token-file", empty, "--optional-secret-file", "ingest-token"}, capture); err != nil || got.ingestToken != "" {
		t.Fatalf("an empty optional file reads as absent: %v", err)
	}
	if err := execServe(t, []string{"--ingest-token-file", empty}, capture); err == nil {
		t.Fatal("an empty required file must fail")
	}
	// A typo or a flag without its file would leave a secret unset silently.
	for _, args := range [][]string{
		{"--optional-secret-file", "ingest"},
		{"--optional-secret-file", "read-token", "--read-token-file", missing},
		{"--optional-secret-file", "ingest-token"},
	} {
		err := execServe(t, args, capture)
		if err == nil || !strings.Contains(err.Error(), "--optional-secret-file") {
			t.Errorf("%v: err = %v, want a refusal naming --optional-secret-file", args, err)
		}
	}
}
