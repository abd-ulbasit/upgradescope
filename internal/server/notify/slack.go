package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// webhookTimeout bounds each delivery attempt. Notifications run inside the
// ingest flow, so a hung webhook must not hold a snapshot hostage.
const webhookTimeout = 2 * time.Second

// FormatLine renders the canonical one-line notification text:
//
//	[upgradescope] <cluster> → <target>: <kind>: <title>
func FormatLine(ev Event) string {
	return fmt.Sprintf("[upgradescope] %s → %s: %s: %s", ev.Cluster, ev.Target, ev.Kind, ev.Title)
}

// SlackNotifier posts events to a Slack incoming webhook as plain text.
type SlackNotifier struct {
	URL    string
	Client *http.Client // exported so tests shorten the timeout; never nil from NewSlack
}

// NewSlack returns a SlackNotifier with the 2s delivery timeout.
func NewSlack(url string) *SlackNotifier {
	return &SlackNotifier{URL: url, Client: &http.Client{Timeout: webhookTimeout}}
}

func (s *SlackNotifier) Notify(ctx context.Context, ev Event) error {
	payload, err := json.Marshal(map[string]string{"text": FormatLine(ev)})
	if err != nil {
		return fmt.Errorf("slack: encode payload: %w", err)
	}
	return postJSON(ctx, s.Client, "slack webhook", s.URL, payload)
}

// GenericWebhook POSTs the raw Event as JSON to any HTTP endpoint.
type GenericWebhook struct {
	URL    string
	Client *http.Client
}

// NewGenericWebhook returns a GenericWebhook with the 2s delivery timeout.
func NewGenericWebhook(url string) *GenericWebhook {
	return &GenericWebhook{URL: url, Client: &http.Client{Timeout: webhookTimeout}}
}

func (g *GenericWebhook) Notify(ctx context.Context, ev Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("webhook: encode event: %w", err)
	}
	return postJSON(ctx, g.Client, "webhook", g.URL, payload)
}

// postJSON sends one JSON POST and treats any non-2xx status as an error.
func postJSON(ctx context.Context, client *http.Client, label, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", label, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // drain for connection reuse
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s: unexpected status %d", label, resp.StatusCode)
	}
	return nil
}
