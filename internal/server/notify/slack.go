package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// webhookTimeout bounds each delivery attempt; the server's outbox retries
// a failed one with backoff.
const webhookTimeout = 2 * time.Second

// SignatureHeader carries the generic webhook's HMAC-SHA256 signature when
// a secret is configured: "sha256=" + hex(HMAC(secret, raw body)).
const SignatureHeader = "X-Upgradescope-Signature"

// Sign returns the SignatureHeader value for body under secret.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// FormatText renders a notification as Slack message text. One change is
// one line:
//
//	[upgradescope] <cluster> → <targets>: <kind>: <title>
//
// several are a header line and one bullet per change.
func FormatText(n Notification) string {
	if len(n.Changes) == 1 && len(n.Omitted) == 0 {
		c := n.Changes[0]
		return fmt.Sprintf("[upgradescope] %s → %s: %s: %s", n.Cluster.Name, strings.Join(c.Targets, ", "), c.Kind, c.Title)
	}
	total := len(n.Changes)
	for _, k := range n.Omitted {
		total += k
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[upgradescope] %s: %d changes", n.Cluster.Name, total)
	for _, c := range n.Changes {
		fmt.Fprintf(&b, "\n• %s: %s: %s", strings.Join(c.Targets, ", "), c.Kind, c.Title)
	}
	for _, kind := range []string{KindNewBlocker, KindEOLApproaching, KindBecameReady} {
		if k := n.Omitted[kind]; k > 0 {
			fmt.Fprintf(&b, "\n• and %d more %s", k, kind)
		}
	}
	return b.String()
}

// SlackNotifier posts notifications to a Slack incoming webhook as text.
type SlackNotifier struct {
	URL    string
	Client *http.Client // exported so tests shorten the timeout; never nil from NewSlack
}

// NewSlack returns a SlackNotifier with the 2s delivery timeout.
func NewSlack(url string) *SlackNotifier {
	return &SlackNotifier{URL: url, Client: &http.Client{Timeout: webhookTimeout}}
}

// slackEscaper escapes the three characters Slack treats as control
// characters in message text (&, <, >), per
// https://docs.slack.dev/messaging/formatting-message-text. Applied to the
// whole formatted text at delivery time; FormatText itself stays plain.
var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func (s *SlackNotifier) Notify(ctx context.Context, n Notification) error {
	payload, err := json.Marshal(map[string]string{"text": slackEscaper.Replace(FormatText(n))})
	if err != nil {
		return fmt.Errorf("slack: encode payload: %w", err)
	}
	return postJSON(ctx, s.Client, "slack webhook", s.URL, payload, nil)
}

// GenericWebhook POSTs the Notification as JSON to any HTTP endpoint,
// signed when Secret is set.
type GenericWebhook struct {
	URL    string
	Secret string // HMAC-SHA256 key for SignatureHeader; "" = unsigned
	Client *http.Client
}

// NewGenericWebhook returns an unsigned GenericWebhook with the 2s
// delivery timeout; set Secret to sign.
func NewGenericWebhook(url string) *GenericWebhook {
	return &GenericWebhook{URL: url, Client: &http.Client{Timeout: webhookTimeout}}
}

func (g *GenericWebhook) Notify(ctx context.Context, n Notification) error {
	payload, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("webhook: encode notification: %w", err)
	}
	h := http.Header{}
	h.Set("X-Upgradescope-Delivery", n.DeliveryID)
	h.Set("X-Upgradescope-Event", n.Type)
	if g.Secret != "" {
		h.Set(SignatureHeader, Sign(g.Secret, payload))
	}
	return postJSON(ctx, g.Client, "webhook", g.URL, payload, h)
}

// noRedirect makes a redirect the response: following one turns the POST
// into a bodyless GET whose 2xx would count as delivered.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// postJSON sends one JSON POST and treats any non-2xx status, a redirect
// included, as an error.
func postJSON(ctx context.Context, client *http.Client, label, url string, body []byte, header http.Header) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", label, err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	c := *client
	c.CheckRedirect = noRedirect
	resp, err := c.Do(req)
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
