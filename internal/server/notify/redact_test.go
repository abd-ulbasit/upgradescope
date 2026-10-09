package notify

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
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
	}{
		{"slack, refused", NewSlack("http://" + refused + "/services/T000/B000/SECRETSECRET"), refused},
		{"webhook, refused", NewGenericWebhook("http://" + refused + "/hook/SECRETPATH?token=SECRETQ#SECRETF"), refused},
		{"webhook with userinfo", NewGenericWebhook("http://user:SECRETPW@" + refused + "/x"), ""},
		{"slack, timeout", &SlackNotifier{URL: slow.URL + "/services/T000/B000/SECRETSECRET", Client: &http.Client{Timeout: 50 * time.Millisecond}}, slowHost},
		{"slack, unparseable", NewSlack("https://hooks.slack.com/services/T000/B000/SECRETSECRET\n"), "hooks.slack.com"},
		{"webhook, no scheme", NewGenericWebhook("hooks.example.com/SECRETPATH"), ""},
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
	for _, bad := range []string{"hooks.slack.com/SECRET", "ftp://h/SECRET", "https:///SECRET", "https://h/SECRET\n", "/SECRET", "https://h:port/SECRET", "mailto:SECRET@h"} {
		err := CheckURL(bad)
		if err == nil {
			t.Errorf("CheckURL(%q) accepted it", bad)
			continue
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("CheckURL(%q) echoes it: %v", bad, err)
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
