package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/secretfile"
)

func rotateFile(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// pusherWithTokenFile builds the pusher the way Run does for
// --server-token-file, checking the file on every push.
func pusherWithTokenFile(t *testing.T, rec *pushRecorder, path string, logs *strings.Builder) *pusher {
	t.Helper()
	p, _ := newTestPusher(t, rec, http.StatusAccepted, http.StatusAccepted, http.StatusAccepted, http.StatusAccepted)
	var mu sync.Mutex
	cfg := Config{
		ServerURL: p.url, ServerTokenFile: path,
		secretOpts: []secretfile.Option{
			secretfile.WithCheckInterval(0),
			secretfile.WithLogf(func(_ bool, format string, args ...any) {
				mu.Lock()
				defer mu.Unlock()
				fmt.Fprintf(logs, format+"\n", args...)
			}),
		},
	}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	p.token, p.tokenFn = cfg.ServerToken, cfg.serverTokenFn
	return p
}

func pushOnce(t *testing.T, p *pusher, rec *pushRecorder) string {
	t.Helper()
	n := rec.requests()
	p.offer(testPayload("prod"))
	if err := p.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if rec.requests() != n+1 {
		t.Fatalf("requests = %d, want %d", rec.requests(), n+1)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.headers[len(rec.headers)-1].Get("Authorization")
}

func TestPushTokenRotatesWithoutARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	rotateFile(t, path, "tok-one\n")
	rec := &pushRecorder{}
	var logs strings.Builder
	p := pusherWithTokenFile(t, rec, path, &logs)

	if got := pushOnce(t, p, rec); got != "Bearer tok-one" {
		t.Fatalf("first push Authorization = %q", got)
	}
	rotateFile(t, path, "tok-two\n")
	if got := pushOnce(t, p, rec); got != "Bearer tok-two" {
		t.Fatalf("push after rotation Authorization = %q, want the new token", got)
	}
	if strings.Contains(logs.String(), "tok-") {
		t.Fatalf("log carries a token: %q", logs.String())
	}
}

func TestPushTokenFileEmptyOrBadKeepsTheOldToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	rotateFile(t, path, "tok-one")
	rec := &pushRecorder{}
	var logs strings.Builder
	p := pusherWithTokenFile(t, rec, path, &logs)

	rotateFile(t, path, "\n")
	if got := pushOnce(t, p, rec); got != "Bearer tok-one" {
		t.Fatalf("after the file was emptied: %q", got)
	}
	rotateFile(t, path, "tok two") // whitespace inside: no request could carry it
	if got := pushOnce(t, p, rec); got != "Bearer tok-one" {
		t.Fatalf("after a token with whitespace: %q", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := pushOnce(t, p, rec); got != "Bearer tok-one" {
		t.Fatalf("after the file went away: %q", got)
	}
	if !strings.Contains(logs.String(), path) || strings.Contains(logs.String(), "tok-one") || strings.Contains(logs.String(), "tok two") {
		t.Fatalf("log must name the file and no token: %q", logs.String())
	}
}

func TestConfigServerTokenFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	rotateFile(t, good, "tok\n")
	cfg := Config{ServerURL: "https://fleet.example.com", ServerTokenFile: good}
	if err := cfg.applyDefaults(); err != nil || cfg.ServerToken != "tok" || cfg.serverTokenFn == nil {
		t.Fatalf("a token file stands in for ServerToken: err %v, token %q", err, cfg.ServerToken)
	}
	for name, content := range map[string]string{"empty": " \n", "spaced": "to k"} {
		path := filepath.Join(dir, name)
		rotateFile(t, path, content)
		cfg := Config{ServerURL: "https://fleet.example.com", ServerTokenFile: path}
		if err := cfg.applyDefaults(); err == nil || !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), content) && strings.TrimSpace(content) != "" {
			t.Errorf("%s token file: err = %v, want one naming the file", name, err)
		}
	}
	cfg = Config{ServerURL: "https://fleet.example.com", ServerTokenFile: filepath.Join(dir, "absent")}
	if err := cfg.applyDefaults(); err == nil {
		t.Error("a missing token file must fail the start")
	}
}

// Pushes and rotations at once: run with -race.
func TestPushTokenRotationIsRaceFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	rotateFile(t, path, "tok-0")
	rec := &pushRecorder{}
	var logs strings.Builder
	p := pusherWithTokenFile(t, rec, path, &logs)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if tok := p.tokenFn(); !strings.HasPrefix(tok, "tok-") {
					t.Errorf("torn token %q", tok)
					return
				}
			}
		}()
	}
	for i := 1; i <= 40; i++ {
		rotateFile(t, path, fmt.Sprintf("tok-%d", i))
	}
	close(stop)
	wg.Wait()
	if got := p.tokenFn(); got != "tok-40" {
		t.Fatalf("final token %q", got)
	}
}

// The agent that ships: a runner built the way Run builds it (applyDefaults,
// then newRunner) pushes with the token file's current content, so a rotated
// Secret takes effect at the next push with no restart. The tests above build
// the pusher by hand; this one covers the wiring in newRunner that connects
// Config to it (#255).
func TestRunnerPushesWithTheRotatedTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	rotateFile(t, path, "tok-one\n")

	var mu sync.Mutex
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"snapshotId": 1}`)
	}))
	t.Cleanup(srv.Close)

	cfg := Config{
		ServerURL: srv.URL, ServerTokenFile: path,
		secretOpts: []secretfile.Option{secretfile.WithCheckInterval(0)},
	}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults: %v", err)
	}
	r := newRunner(fakeClients(t, "v1.35.2"), fakeDyn(), mustKB(t), cfg)
	r.pusher.wait = func(context.Context, time.Duration) error { return nil }

	tickPush := func() string {
		t.Helper()
		mu.Lock()
		n := len(auth)
		mu.Unlock()
		r.lastHash = "" // an unchanged inventory would not be pushed again
		if err := r.tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(auth) != n+1 {
			t.Fatalf("pushes = %d, want %d", len(auth), n+1)
		}
		return auth[len(auth)-1]
	}

	if got := tickPush(); got != "Bearer tok-one" {
		t.Fatalf("first push Authorization = %q", got)
	}
	rotateFile(t, path, "tok-two\n")
	if got := tickPush(); got != "Bearer tok-two" {
		t.Fatalf("push after rotation Authorization = %q, want the rotated token", got)
	}
	// A bad new file keeps the token in service.
	rotateFile(t, path, "\n")
	if got := tickPush(); got != "Bearer tok-two" {
		t.Fatalf("push after the file was emptied Authorization = %q, want the old token", got)
	}
}
