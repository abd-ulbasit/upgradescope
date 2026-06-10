package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	sleep func(time.Duration) // injectable for deterministic tests

	mu      sync.Mutex
	pending *pushPayload
}

func newPusher(serverURL, token string) *pusher {
	return &pusher{
		url:   strings.TrimRight(serverURL, "/"),
		token: token,
		hc:    &http.Client{Timeout: 30 * time.Second},
		sleep: time.Sleep,
	}
}

// offer replaces any pending snapshot with the newer one.
func (p *pusher) offer(pl pushPayload) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = &pl
}

// flush sends the pending snapshot, if any. Transient failures (network,
// 5xx) retry up to pushRetries times with exponential backoff; the payload
// stays buffered on exhaustion so the next tick retries. Permanent failures
// (401 bad token, 422 invalid body) drop the payload — resending identical
// bytes cannot succeed — and return the error for logging.
func (p *pusher) flush(ctx context.Context) error {
	p.mu.Lock()
	pl := p.pending
	p.mu.Unlock()
	if pl == nil {
		return nil
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		permanent, err := p.send(ctx, *pl)
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
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.sleep(backoff(attempt))
	}
	return fmt.Errorf("push snapshot after %d attempts (kept buffered): %w", pushRetries+1, lastErr)
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

func (p *pusher) send(ctx context.Context, pl pushPayload) (permanent bool, err error) {
	body, err := json.Marshal(pl)
	if err != nil {
		return true, fmt.Errorf("marshal snapshot: %w", err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		return true, fmt.Errorf("gzip snapshot: %w", err)
	}
	if err := zw.Close(); err != nil {
		return true, fmt.Errorf("gzip snapshot: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+"/api/v1/snapshots", &buf)
	if err != nil {
		return true, fmt.Errorf("build push request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := p.hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("push snapshot: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK: // 202 accepted, 200 duplicate
		return false, nil
	case http.StatusUnauthorized:
		return true, fmt.Errorf("server rejected push (401): check --server-token")
	case http.StatusUnprocessableEntity:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return true, fmt.Errorf("server rejected snapshot (422): %s", strings.TrimSpace(string(msg)))
	default:
		return false, fmt.Errorf("server returned %s", resp.Status)
	}
}
