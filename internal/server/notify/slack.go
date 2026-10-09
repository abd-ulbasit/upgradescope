package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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
// included, as an error: a RetryAfterError for a 429 or 503 with Retry-After.
// No error it returns carries the URL past its host (RedactURL): the
// outbox logs them and stores them in last_error.
func postJSON(ctx context.Context, client *http.Client, label, url string, body []byte, header http.Header) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return redactedError(label+": build request", url, err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	c := *client
	c.CheckRedirect = noRedirect
	resp, err := c.Do(req)
	if err != nil {
		return redactedError(label, url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // drain for connection reuse
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err := fmt.Errorf("%s: unexpected status %d", label, resp.StatusCode)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			if after, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
				return &RetryAfterError{Err: err, After: after}
			}
		}
		return err
	}
	return nil
}

// CheckURL reports whether raw is a URL a notifier can deliver to: an
// absolute http or https URL with a host, and nothing a request could not
// carry. Its error never repeats the URL, whose path or query may be the
// credential.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("does not parse: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("the scheme is not http or https")
	}
	if u.Hostname() == "" {
		return errors.New("no host")
	}
	if p := u.Port(); p != "" {
		if _, err := strconv.ParseUint(p, 10, 16); err != nil {
			return errors.New("the port is not a number")
		}
	}
	return nil
}

// RedactURL is raw with nothing that could be a credential: its scheme and
// host (with the port), and "/…" when anything followed them. The path of a
// Slack incoming webhook is its secret, and a generic webhook's query often
// carries a token. A URL with an "@" anywhere after the scheme, userinfo or
// not, has its host withheld too: text cannot tell where a password holding
// "/" or "?" ends, so the host part could be the credential. So does one
// whose host is not plain host-name characters (it does not parse).
func RedactURL(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok || scheme == "" || strings.ContainsFunc(scheme, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.')
	}) {
		return "(URL redacted)"
	}
	host, tail := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		host, tail = rest[:i], rest[i:]
	}
	if strings.Contains(rest, "@") || !plainHost(host) {
		host = "(host redacted)"
	}
	out := strings.ToLower(scheme) + "://" + host
	if tail != "" {
		out += "/…"
	}
	return out
}

// plainHost reports whether h is a host, with an optional port, made of
// the characters a host name, an IPv4 or a bracketed IPv6 address uses.
func plainHost(h string) bool {
	if h == "" {
		return false
	}
	for _, r := range h {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(".-_:[]", r)) {
			return false
		}
	}
	return true
}

// redactedError is err with every URL it carries redacted: net/http's
// *url.Error (a failed request or a URL that does not parse) prints the
// whole URL, so its operation and cause are kept around RedactURL's form.
func redactedError(label, rawURL string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return &deliveryError{msg: fmt.Sprintf("%s: %s %s: %s", label, ue.Op, RedactURL(rawURL), Scrub(ue.Err.Error(), rawURL)), err: ue.Err}
	}
	return &deliveryError{msg: label + ": " + Scrub(err.Error(), rawURL), err: err}
}

// Scrub is msg with every copy of rawURL in it, as written or as Go quotes
// it (%q, which *url.Error uses), replaced by RedactURL's form, and its
// path and query, when they are long enough to be a credential, by "…".
func Scrub(msg, rawURL string) string {
	if rawURL == "" {
		return msg
	}
	red := RedactURL(rawURL)
	msg = strings.ReplaceAll(msg, rawURL, red)
	if q := strconv.Quote(rawURL); len(q) > 2 {
		msg = strings.ReplaceAll(msg, q[1:len(q)-1], red)
	}
	if u, err := url.Parse(rawURL); err == nil {
		for _, part := range []string{u.EscapedPath(), u.Path, u.RawQuery, u.Fragment, u.User.String()} {
			if len(part) >= 4 {
				msg = strings.ReplaceAll(msg, part, "…")
			}
		}
	}
	return msg
}

// deliveryError is a redacted transport error that still unwraps to its
// cause (a timeout stays a net.Error).
type deliveryError struct {
	msg string
	err error
}

func (e *deliveryError) Error() string { return e.msg }
func (e *deliveryError) Unwrap() error { return e.err }
