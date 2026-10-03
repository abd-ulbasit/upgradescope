package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pushPayload is the snapshot envelope (plan: "Snapshot push protocol").
type pushPayload struct {
	SchemaVersion int             `json:"schemaVersion"`
	ClusterName   string          `json:"clusterName"`
	AgentVersion  string          `json:"agentVersion"`
	KBVersion     string          `json:"kbVersion"`
	Inventory     json.RawMessage `json:"inventory"`
}

const (
	pushRetries    = 3 // retries after the initial attempt
	maxPushBackoff = time.Minute
)

// backoff is the delay before retry n (0-based): 1s, 2s, 4s, ... capped at 1m.
func backoff(attempt int) time.Duration {
	d := time.Second << uint(min(attempt, 20))
	if d > maxPushBackoff {
		return maxPushBackoff
	}
	return d
}

// pusher delivers snapshots to the server. It buffers at most one payload:
// offering a newer snapshot replaces the pending one (latest-only — spec §9:
// the agent never queues history, the server reconstructs it from pushes).
type pusher struct {
	url   string // base server URL, trailing slash trimmed
	token string
	hc    *http.Client
	log   *slog.Logger                                     // nil = slog.Default()
	wait  func(ctx context.Context, d time.Duration) error // injectable for deterministic tests

	mu      sync.Mutex
	pending *pushPayload
}

// CleartextPushWarning says why pushing to serverURL with token is unsafe,
// or returns "": a bearer token sent over plain http:// to a host that is
// not loopback crosses the network in the clear, where anyone on the path
// can replay it — and the shared ingest token may push as any cluster.
func CleartextPushWarning(serverURL, token string) string {
	u, err := url.Parse(serverURL)
	if err != nil || token == "" || !strings.EqualFold(u.Scheme, "http") {
		return ""
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback() {
		return ""
	}
	return fmt.Sprintf("pushing snapshots to %s over plain http: the bearer token crosses the network unencrypted, "+
		"so anyone on the path can replay it; serve the server over https (--tls-cert-file, the chart's server.tls, or a TLS Ingress)", u.Host)
}

func newPusher(serverURL, token string) *pusher {
	return &pusher{
		url:   strings.TrimRight(serverURL, "/"),
		token: token,
		hc: &http.Client{
			Timeout: 30 * time.Second,
			// Never follow a redirect: Go turns a 301/302/303 POST into a
			// body-less GET, which a login page or SPA can answer 200 and the
			// push would count as delivered. send reports any 3xx instead.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		wait: waitFor,
	}
}

// waitFor blocks for d or until ctx is canceled, whichever comes first, so a
// SIGTERM during backoff never stalls shutdown for the backoff duration.
func waitFor(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (p *pusher) logger() *slog.Logger {
	if p.log != nil {
		return p.log
	}
	return slog.Default()
}

// offer replaces any pending snapshot with the newer one.
func (p *pusher) offer(pl pushPayload) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = &pl
}

// flush sends the pending snapshot, if any. Transient failures (network,
// 5xx, 408, 429) retry up to pushRetries times with exponential backoff; the
// payload stays buffered on exhaustion so a later flush can retry it (in
// practice the runner re-offers a fresh payload on the next push-worthy
// tick). Permanent failures (any other 4xx: bad token, invalid body, ...)
// drop the payload — resending identical bytes cannot succeed — and return
// the error for logging. A context cancel or deadline during backoff
// returns promptly, wrapping ctx's error and the last attempt's, payload kept.
func (p *pusher) flush(ctx context.Context) error {
	p.mu.Lock()
	pl := p.pending
	p.mu.Unlock()
	if pl == nil {
		return nil
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		permanent, retryAfter, err := p.send(ctx, *pl)
		if err == nil {
			p.clear(pl)
			return nil
		}
		if permanent {
			p.clear(pl)
			return err
		}
		lastErr = err
		if attempt == pushRetries {
			break
		}
		if werr := p.wait(ctx, retryDelay(attempt, retryAfter)); werr != nil {
			// A Retry-After can outlast the tick deadline; keep the server's
			// status so the log says why the push was failing, not just that
			// the tick ran out of time.
			return fmt.Errorf("push snapshot (kept buffered): %w; last attempt: %w", werr, lastErr)
		}
	}
	return fmt.Errorf("push snapshot after %d attempts (kept buffered): %w", pushRetries+1, lastErr)
}

// retryDelay is the wait before retry n: the backoff step, or the server's
// Retry-After when that asks for longer, never beyond maxPushBackoff.
func retryDelay(attempt int, retryAfter time.Duration) time.Duration {
	return min(max(backoff(attempt), retryAfter), maxPushBackoff)
}

// parseRetryAfter reads a Retry-After value, delta-seconds or an HTTP-date
// (RFC 9110 §10.2.3); 0 when absent, malformed or not in the future.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(min(secs, int64(maxPushBackoff/time.Second))) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0)
	}
	return 0
}

// clear drops pl iff it is still the pending payload — a newer offer made
// during a slow send must survive.
func (p *pusher) clear(pl *pushPayload) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == pl {
		p.pending = nil
	}
}

// send makes one attempt. retryAfter is the server's Retry-After on a
// transient failure (0 = none).
func (p *pusher) send(ctx context.Context, pl pushPayload) (permanent bool, retryAfter time.Duration, err error) {
	body, err := json.Marshal(pl)
	if err != nil {
		return true, 0, fmt.Errorf("marshal snapshot: %w", err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		return true, 0, fmt.Errorf("gzip snapshot: %w", err)
	}
	if err := zw.Close(); err != nil {
		return true, 0, fmt.Errorf("gzip snapshot: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+"/api/v1/snapshots", &buf)
	if err != nil {
		return true, 0, fmt.Errorf("build push request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := p.hc.Do(req)
	if err != nil {
		return false, 0, fmt.Errorf("push snapshot: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK: // 202 accepted, 200 duplicate
		// Drain (bounded) so the keep-alive connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return false, 0, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return true, 0, fmt.Errorf("server rejected push (401): check --server-token")
	case resp.StatusCode == http.StatusUnprocessableEntity:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return true, 0, fmt.Errorf("server rejected snapshot (422): %s", strings.TrimSpace(string(msg)))
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// Never followed (see newPusher): resending the body to the Location
		// could leak it, and a body-less GET can "succeed" without delivering
		// anything. The URL is wrong, so identical retries cannot succeed.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		loc := resp.Header.Get("Location")
		err := fmt.Errorf("server redirected the push (%s) to %q: the push is never sent to a redirect target; use the final URL in --server-url", resp.Status, loc)
		p.logger().Error("snapshot push refused: the server answered with a redirect", "status", resp.Status, "location", loc, "server", p.url, "hint", "use the final URL in --server-url")
		return true, 0, err
	case resp.StatusCode == http.StatusRequestTimeout:
		// Retryable by definition despite being 4xx.
		return false, 0, fmt.Errorf("server returned %s", resp.Status)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
		return false, parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()), fmt.Errorf("server returned %s", resp.Status)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// Any other 4xx: the request itself is wrong; identical retries cannot succeed.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return true, 0, fmt.Errorf("server rejected push (%s): %s", resp.Status, strings.TrimSpace(string(msg)))
	default: // 5xx and anything unexpected: transient
		return false, 0, fmt.Errorf("server returned %s", resp.Status)
	}
}
