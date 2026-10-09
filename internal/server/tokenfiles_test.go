package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/secretfile"
)

// rotate replaces path with a new file, as the kubelet swaps a Secret
// volume's contents.
func rotate(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

type tokenFiles struct {
	dir                 string
	ingest, read, admin string
	logMu               sync.Mutex
	logLines            []string
	opts                []secretfile.Option
}

func newTokenFiles(t *testing.T) *tokenFiles {
	t.Helper()
	f := &tokenFiles{dir: t.TempDir()}
	f.ingest = filepath.Join(f.dir, "ingestToken")
	f.read = filepath.Join(f.dir, "readToken")
	f.admin = filepath.Join(f.dir, "adminToken")
	rotate(t, f.ingest, "ingest-one\n")
	rotate(t, f.read, "read-one\n")
	rotate(t, f.admin, "admin-one\n")
	f.opts = []secretfile.Option{
		secretfile.WithCheckInterval(0),
		secretfile.WithLogf(func(_ bool, format string, args ...any) {
			f.logMu.Lock()
			f.logLines = append(f.logLines, fmt.Sprintf(format, args...))
			f.logMu.Unlock()
		}),
	}
	return f
}

func (f *tokenFiles) logs() string {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	return strings.Join(f.logLines, "\n")
}

func (f *tokenFiles) apply(c *Config) {
	c.IngestToken, c.ReadToken, c.AdminToken = "", "", ""
	c.IngestTokenFile, c.ReadTokenFile, c.AdminTokenFile = f.ingest, f.read, f.admin
	c.secretOpts = f.opts
}

func status(t *testing.T, ts *httptest.Server, method, path, token string) int {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func pushStatus(t *testing.T, ts *httptest.Server, token string) int {
	t.Helper()
	resp, _ := postSnapshot(t, ts, token, pushReqBody(t, testInventory()), true)
	return resp.StatusCode
}

func TestReadTokenRotatesWithoutARestart(t *testing.T) {
	tf := newTokenFiles(t)
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), tf.apply).Handler())
	defer ts.Close()

	if got := status(t, ts, "GET", "/api/v1/clusters", "read-one"); got != 200 {
		t.Fatalf("current read token = %d, want 200", got)
	}
	rotate(t, tf.read, "read-two\n")
	if got := status(t, ts, "GET", "/api/v1/clusters", "read-two"); got != 200 {
		t.Fatalf("rotated read token = %d, want 200", got)
	}
	if got := status(t, ts, "GET", "/api/v1/clusters", "read-one"); got != 401 {
		t.Fatalf("old read token = %d, want 401", got)
	}
	if strings.Contains(tf.logs(), "read-one") || strings.Contains(tf.logs(), "read-two") {
		t.Fatalf("a log line carries a token: %q", tf.logs())
	}
}

func TestIngestTokenRotatesWithoutARestart(t *testing.T) {
	tf := newTokenFiles(t)
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), tf.apply).Handler())
	defer ts.Close()

	if got := pushStatus(t, ts, "ingest-one"); got != http.StatusAccepted {
		t.Fatalf("current ingest token = %d, want 202", got)
	}
	rotate(t, tf.ingest, "ingest-two\n")
	if got := pushStatus(t, ts, "ingest-two"); got != http.StatusAccepted && got != http.StatusOK {
		t.Fatalf("rotated ingest token = %d, want 202/200", got)
	}
	if got := pushStatus(t, ts, "ingest-one"); got != 401 {
		t.Fatalf("old ingest token = %d, want 401", got)
	}
}

func TestAdminTokenRotatesWithoutARestart(t *testing.T) {
	tf := newTokenFiles(t)
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), tf.apply).Handler())
	defer ts.Close()

	if got := status(t, ts, "DELETE", "/api/v1/clusters/999", "admin-one"); got != 404 {
		t.Fatalf("current admin token = %d, want 404 (authorized, no such cluster)", got)
	}
	rotate(t, tf.admin, "admin-two\n")
	if got := status(t, ts, "DELETE", "/api/v1/clusters/999", "admin-two"); got != 404 {
		t.Fatalf("rotated admin token = %d, want 404", got)
	}
	if got := status(t, ts, "DELETE", "/api/v1/clusters/999", "admin-one"); got != 403 {
		t.Fatalf("old admin token = %d, want 403", got)
	}
}

func TestEmptyOrUnreadableTokenFileKeepsTheOldToken(t *testing.T) {
	tf := newTokenFiles(t)
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), tf.apply).Handler())
	defer ts.Close()

	rotate(t, tf.read, "\n")
	rotate(t, tf.ingest, "")
	if err := os.Remove(tf.admin); err != nil {
		t.Fatal(err)
	}
	if got := status(t, ts, "GET", "/api/v1/clusters", "read-one"); got != 200 {
		t.Fatalf("read token after its file was emptied = %d, want 200 (old value kept)", got)
	}
	if got := status(t, ts, "GET", "/api/v1/clusters", ""); got != 401 {
		t.Fatalf("no token after the file was emptied = %d, want 401: an empty file must not open the read API", got)
	}
	if got := pushStatus(t, ts, "ingest-one"); got != http.StatusAccepted {
		t.Fatalf("ingest token after its file was emptied = %d, want 202", got)
	}
	if got := status(t, ts, "DELETE", "/api/v1/clusters/999", "admin-one"); got != 404 {
		t.Fatalf("admin token after its file went away = %d, want 404", got)
	}
	for _, p := range []string{tf.read, tf.ingest, tf.admin} {
		if !strings.Contains(tf.logs(), p) {
			t.Errorf("no log line names %s:\n%s", p, tf.logs())
		}
	}
	for _, secret := range []string{"read-one", "ingest-one", "admin-one"} {
		if strings.Contains(tf.logs(), secret) {
			t.Errorf("a log line carries %q", secret)
		}
	}
}

// A rotation that would make two credentials the same is refused for the
// one that moves, as New refuses it at start.
func TestRotationToACollidingTokenIsRefused(t *testing.T) {
	tf := newTokenFiles(t)
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), tf.apply).Handler())
	defer ts.Close()

	rotate(t, tf.read, "ingest-one")
	if got := status(t, ts, "GET", "/api/v1/clusters", "read-one"); got != 200 {
		t.Fatalf("old read token = %d, want 200: a read token equal to the ingest token is refused", got)
	}
	if got := status(t, ts, "GET", "/api/v1/clusters", "ingest-one"); got != 401 {
		t.Fatalf("the ingest token must not read = %d, want 401", got)
	}
	rotate(t, tf.admin, "read-one")
	if got := status(t, ts, "DELETE", "/api/v1/clusters/999", "read-one"); got != 403 {
		t.Fatalf("the read token must not administer = %d, want 403", got)
	}
}

func TestOptionalIngestTokenFileAppearsAndRevokes(t *testing.T) {
	tf := newTokenFiles(t)
	if err := os.Remove(tf.ingest); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), tf.apply, func(c *Config) { c.IngestTokenFileOptional = true }).Handler())
	defer ts.Close()

	if got := pushStatus(t, ts, "ingest-late"); got != 401 {
		t.Fatalf("no ingest token yet = %d, want 401", got)
	}
	rotate(t, tf.ingest, "ingest-late")
	if got := pushStatus(t, ts, "ingest-late"); got != http.StatusAccepted {
		t.Fatalf("ingest token added to the Secret later = %d, want 202", got)
	}
	if err := os.Remove(tf.ingest); err != nil {
		t.Fatal(err)
	}
	if got := pushStatus(t, ts, "ingest-late"); got != 401 {
		t.Fatalf("ingest token removed from the Secret = %d, want 401: deleting the key revokes it", got)
	}
}

func TestNewFailsOnABadTokenFile(t *testing.T) {
	tf := newTokenFiles(t)
	rotate(t, tf.read, "")
	_, err := New(Config{Store: newFakeStore(), ReadTokenFile: tf.read})
	if err == nil || !strings.Contains(err.Error(), tf.read) {
		t.Fatalf("New with an empty read-token file: err = %v, want one naming the file", err)
	}
	_, err = New(Config{Store: newFakeStore(), ReadTokenFile: filepath.Join(tf.dir, "absent")})
	if err == nil {
		t.Fatal("New with a missing read-token file: want an error")
	}
	rotate(t, tf.read, "same")
	rotate(t, tf.ingest, "same")
	if _, err := New(Config{Store: newFakeStore(), ReadTokenFile: tf.read, IngestTokenFile: tf.ingest}); err == nil {
		t.Fatal("New with equal read and ingest token files: want a refusal")
	}
}

// Concurrent requests and rotations: run with -race.
func TestTokenRotationUnderConcurrentRequests(t *testing.T) {
	tf := newTokenFiles(t)
	ts := httptest.NewServer(newTestServer(t, newFakeStore(), tf.apply).Handler())
	defer ts.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				req, _ := http.NewRequest("GET", ts.URL+"/api/v1/clusters", nil)
				req.Header.Set("Authorization", "Bearer read-one")
				if resp, err := ts.Client().Do(req); err == nil {
					resp.Body.Close()
				}
				req, _ = http.NewRequest("DELETE", ts.URL+"/api/v1/clusters/999", nil)
				req.Header.Set("Authorization", "Bearer admin-one")
				if resp, err := ts.Client().Do(req); err == nil {
					resp.Body.Close()
				}
				pushStatus(t, ts, "ingest-one")
			}
		}()
	}
	for i := range 30 {
		rotate(t, tf.read, fmt.Sprintf("read-%d", i))
		rotate(t, tf.admin, fmt.Sprintf("admin-%d", i))
		rotate(t, tf.ingest, fmt.Sprintf("ingest-%d", i))
	}
	close(stop)
	wg.Wait()
	if got := status(t, ts, "GET", "/api/v1/clusters", "read-29"); got != 200 {
		t.Fatalf("final read token = %d, want 200", got)
	}
}
