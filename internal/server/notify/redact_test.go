package notify

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A Slack webhook's path is its credential, and a generic webhook's query
// often carries one. A failed delivery used to return net/http's
// *url.Error, whose text is the whole URL, and the outbox logged it and
// stored it in last_error (#240). The error now keeps the scheme and host,
// for diagnosis, and nothing after them.
func TestDeliveryErrorsNeverCarryTheURLsSecrets(t *testing.T) {
	// A port nothing listens on: connection refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := ln.Addr().String()
	ln.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond) // past the clients' 50ms timeout
	}))
	defer slow.Close()
	slowHost := strings.TrimPrefix(slow.URL, "http://")

	for _, tc := range []struct {
		name     string
		notifier Notifier
		host     string
		hidden   []string // text that must not appear: the host the URL withholds
		reason   string   // text that must appear: the cause, without the address
	}{
		{name: "slack, refused", notifier: NewSlack("http://" + refused + "/services/T000/B000/SECRETSECRET"), host: refused},
		{name: "webhook, refused", notifier: NewGenericWebhook("http://" + refused + "/hook/SECRETPATH?token=SECRETQ#SECRETF"), host: refused},
		{name: "webhook with userinfo", notifier: NewGenericWebhook("http://user:SECRETPW@" + refused + "/x"),
			hidden: []string{refused, "127.0.0.1"}, reason: "connection refused"},
		{name: "@ only in the query", notifier: NewGenericWebhook("http://" + refused + "/x?email=a@b.example&t=SECRETQ"),
			hidden: []string{refused, "127.0.0.1", "b.example"}, reason: "connection refused"},
		{name: "@, unresolvable name", notifier: NewGenericWebhook("http://user:SECRETPW@secrethostzz.invalid/p/SECRETX"),
			hidden: []string{"secrethostzz", ".invalid"}},
		{name: "@, name that resolves (localhost, [::1] or 127.0.0.1)", notifier: NewGenericWebhook("http://user:SECRETPW@localhost:1/x"),
			hidden: []string{"localhost", "[::1]", "127.0.0.1", "tcp"}, reason: "connection refused"},
		{name: "@, slack, refused", notifier: NewSlack("http://user:SECRETPW@" + refused + "/services/T000/B000/SECRETSECRET"),
			hidden: []string{refused, "127.0.0.1"}, reason: "connection refused"},
		{name: "slack, timeout", notifier: &SlackNotifier{URL: slow.URL + "/services/T000/B000/SECRETSECRET", Client: &http.Client{Timeout: 50 * time.Millisecond}}, host: slowHost},
		{name: "slack, unparseable", notifier: NewSlack("https://hooks.slack.com/services/T000/B000/SECRETSECRET\n"), host: "hooks.slack.com"},
		{name: "webhook, no scheme", notifier: NewGenericWebhook("hooks.example.com/SECRETPATH"), host: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.notifier.Notify(context.Background(), testNotification())
			if err == nil {
				t.Fatal("delivery succeeded")
			}
			msg := err.Error()
			if strings.Contains(msg, "SECRET") {
				t.Errorf("error carries a secret: %s", msg)
			}
			if tc.host != "" && !strings.Contains(msg, tc.host) {
				t.Errorf("error %q does not name the host %s", msg, tc.host)
			}
			for _, h := range tc.hidden {
				if strings.Contains(msg, h) {
					t.Errorf("error %q names %q, which the redacted URL withholds", msg, h)
				}
			}
			if tc.reason != "" && !strings.Contains(msg, tc.reason) {
				t.Errorf("error %q does not give the reason %q", msg, tc.reason)
			}
		})
	}
	// The cause survives the redaction.
	err = (&SlackNotifier{URL: slow.URL + "/x", Client: &http.Client{Timeout: 50 * time.Millisecond}}).Notify(context.Background(), testNotification())
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("a timeout is no longer a net.Error timeout: %v", err)
	}
}

func TestRedactURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://hooks.slack.com/services/T/B/SECRET":   "https://hooks.slack.com/…",
		"https://hooks.slack.com":                       "https://hooks.slack.com",
		"http://hook.internal:8080/x?token=SECRET":      "http://hook.internal:8080/…",
		"http://hook.internal:8080?token=SECRET":        "http://hook.internal:8080/…",
		"https://user:SECRET@hook.example/x":            "https://(host redacted)/…",
		"https://user:SE/CRET@hook.example/x":           "https://(host redacted)/…",
		"https://hook.example/x?email=a@b.example&t=S":  "https://(host redacted)/…",
		"https://hooks.slack.com/services/T/B/SECRET\n": "https://hooks.slack.com/…",
		"https://hooks.slack.com\n":                     "https://(host redacted)",
		"hooks.slack.com/services/T/B/SECRET":           "(URL redacted)",
		"":                                              "(URL redacted)",
		"https://[fd00::1]:8443/x":                      "https://[fd00::1]:8443/…",
	} {
		if got := RedactURL(raw); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

// CheckURL accepts what can be delivered to and says why not otherwise,
// without the URL.
func TestCheckURL(t *testing.T) {
	for _, ok := range []string{"https://hooks.slack.com/services/T/B/X", "http://hook.internal:8080/x?token=q", "https://[fd00::1]/x"} {
		if err := CheckURL(ok); err != nil {
			t.Errorf("CheckURL(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"hooks.slack.com/SECRET", "ftp://h/SECRET", "https:///SECRET", "https://h/SECRET\n", "/SECRET", "https://h:port/SECRET", "mailto:SECRET@h",
		"https://user:pa/ss@host/x", "https://h:SECRETPORT/x", "https://host/%zzSECRET", "https://h/\x7fSECRET", "http://[SECRET/x", "https://user:SECRET@h:port/x"} {
		err := CheckURL(bad)
		if err == nil {
			t.Errorf("CheckURL(%q) accepted it", bad)
			continue
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("CheckURL(%q) echoes it: %v", bad, err)
		}
		// A fixed text per failure class, never net/url's own error, which
		// quotes part of the value (#371).
		for _, frag := range []string{":pa", "pa/ss", "%zz", "x7f", `"`, "invalid port", "invalid URL escape", "net/url"} {
			if strings.Contains(err.Error(), frag) {
				t.Errorf("CheckURL(%q) = %q, which holds %q, a fragment of the value or of net/url's error", bad, err, frag)
			}
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			t.Errorf("CheckURL(%q) wraps net/url's error: %v", bad, err)
		}
	}
}

// leakyTransport fails every request with an error that quotes its URL, as
// a proxy or a custom transport might.
type leakyTransport struct{}

func (leakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, errors.New("upstream refused " + r.URL.String() + " and path " + r.URL.Path)
}

// The cause inside a *url.Error, and an error from anywhere else, is
// scrubbed of the URL too.
func TestDeliveryErrorCauseIsScrubbed(t *testing.T) {
	n := &GenericWebhook{URL: "https://hook.example/SECRETPATH?token=SECRETQ", Client: &http.Client{Transport: leakyTransport{}}}
	err := n.Notify(context.Background(), testNotification())
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "hook.example") {
		t.Errorf("err = %v, want the host and no secret", err)
	}
}

// A TLS failure against a URL whose host is withheld gives the TLS reason
// and not the address, and a timeout stays a timeout (#371).
func TestWithheldHostKeepsTheTLSReason(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	err := NewGenericWebhook("https://user:SECRETPW@"+addr+"/x").Notify(context.Background(), testNotification())
	if err == nil {
		t.Fatal("delivery to an untrusted certificate succeeded")
	}
	msg := err.Error()
	if !strings.Contains(msg, "x509: certificate signed by unknown authority") || strings.Contains(msg, addr) || strings.Contains(msg, "127.0.0.1") || strings.Contains(msg, "SECRET") {
		t.Errorf("err = %q, want the TLS reason and neither the address nor a secret", msg)
	}
}
