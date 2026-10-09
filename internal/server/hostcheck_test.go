package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

// hostRequest sends method path to addr (host:port) over a real TCP
// connection with the Host header set to host, the way a browser sends a
// rebound request: connected to the loopback socket, naming its own origin.
func hostRequest(t *testing.T, addr, method, path, host string, header ...string) (int, string) {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewReader([]byte("{}"))
	}
	req, err := http.NewRequest(method, "http://"+addr+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Add(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s (Host %s): %v", method, path, host, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// A serve on loopback with an open read API answered a DNS-rebinding page
// on attacker.example with the whole fleet (#240). Every request on a
// loopback listener now names a host the server answers for, or gets 421
// before any route: the API, /metrics, the dashboard, the gate and ingest
// alike. Loopback names on any port still work.
func TestLoopbackServerRefusesForeignHosts(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.Listen = "127.0.0.1:0" })
	startServer(t, s)
	addr := s.Addr()
	_, port, _ := net.SplitHostPort(addr)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/clusters"},
		{http.MethodGet, "/api/v1/fleet"},
		{http.MethodGet, "/metrics"},
		{http.MethodGet, "/"},
		{http.MethodGet, "/healthz"},
		{http.MethodPost, "/api/v1/gate"},
		{http.MethodPost, "/api/v1/snapshots"},
	} {
		for _, host := range []string{"attacker.example:" + port, "attacker.example", "localhost.attacker.example:" + port, "127.0.0.1.nip.io:" + port} {
			if code, body := hostRequest(t, addr, tc.method, tc.path, host); code != http.StatusMisdirectedRequest {
				t.Errorf("%s %s with Host %q = %d %s, want 421", tc.method, tc.path, host, code, body)
			}
		}
	}
	for _, host := range []string{
		"127.0.0.1:" + port, "127.0.0.1", "127.9.9.9:" + port, "localhost:" + port, "localhost", "LOCALHOST:" + port,
		"localhost.:" + port, "[::1]:" + port, "[::1]", "localhost:9000", "127.0.0.1:80",
	} {
		if code, body := hostRequest(t, addr, http.MethodGet, "/api/v1/clusters", host); code != http.StatusOK {
			t.Errorf("GET /api/v1/clusters with Host %q = %d %s, want 200", host, code, body)
		}
	}
}

// refusalLines is the log's Host-refusal lines.
func refusalLines(logged *bytes.Buffer) []string {
	var out []string
	for _, l := range strings.Split(logged.String(), "\n") {
		if strings.Contains(l, "refused a request for Host") {
			out = append(out, l)
		}
	}
	return out
}

// A 421 from the Host check came before the metrics middleware and wrote no
// log, so neither /metrics nor the log showed a rebinding attempt or a
// client sending a Host serve does not answer for (#240). Every refusal is
// counted under route "host-refused", code 421, and logged at most once a
// minute, the Host escaped and the refusals since the last line counted.
func TestHostRefusalsAreCountedAndLogged(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = "127.0.0.1:0" })
	clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
	s.now = clock.now
	logged := captureLog(t)
	startServer(t, s)
	addr := s.Addr()
	refuse := func(host string) {
		t.Helper()
		if code, body := hostRequest(t, addr, http.MethodGet, "/api/v1/clusters", host); code != http.StatusMisdirectedRequest {
			t.Fatalf("Host %q = %d %s, want 421", host, code, body)
		}
	}
	refuse("attacker.example:8080")
	for range 4 {
		refuse("other.example")
	}
	clock.set(clock.now().Add(hostRefusalLogEvery - time.Second))
	refuse("other.example")
	lines := refusalLines(logged)
	if len(lines) != 1 || !strings.Contains(lines[0], `Host "attacker.example:8080"`) || !strings.Contains(lines[0], "127.0.0.1:") {
		t.Fatalf("refusal lines within a minute:\n%s\nwant one, naming the first Host and the client's address", strings.Join(lines, "\n"))
	}

	// A minute on, the next refusal is logged with the count it stood for,
	// its Host escaped to ASCII: no control character or look-alike letter
	// reaches the log.
	clock.set(clock.now().Add(time.Second))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "evil\n.ex\u0430mple\x1b[2J\u202e"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("Host with control characters = %d, want 421", rec.Code)
	}
	lines = refusalLines(logged)
	if len(lines) != 2 {
		t.Fatalf("refusal lines after a minute:\n%s\nwant two", strings.Join(lines, "\n"))
	}
	if want := `Host "evil\n.ex\u0430mple\x1b[2J\u202e"`; !strings.Contains(lines[1], want) || !strings.Contains(lines[1], "5 more") {
		t.Errorf("second refusal line %q, want it to name %s and the 5 refusals not logged", lines[1], want)
	}
	if strings.ContainsAny(logged.String(), "\x1b\u202e\u0430") {
		t.Errorf("the log carries a raw control or non-ASCII character:\n%q", logged)
	}

	// A clock stepped back does not silence the log for the step.
	clock.set(clock.now().Add(-time.Hour))
	refuse("stepped.example")
	if lines = refusalLines(logged); len(lines) != 3 {
		t.Errorf("refusal after the clock stepped back: %d lines, want 3", len(lines))
	}

	// A Host of any length is quoted cut to maxLoggedHost bytes.
	clock.set(clock.now().Add(hostRefusalLogEvery))
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = strings.Repeat("a", 4096) + ".example"
	s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	lines = refusalLines(logged)
	if last := lines[len(lines)-1]; len(lines) != 4 || !strings.Contains(last, `"`+strings.Repeat("a", maxLoggedHost)+`"…`) || len(last) > maxLoggedHost+300 {
		t.Errorf("refusal of a 4 KiB Host: %d lines, the last %d bytes, want 4 and the Host cut to %d bytes", len(lines), len(last), maxLoggedHost)
	}

	code, body := hostRequest(t, addr, http.MethodGet, "/metrics", "localhost")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics = %d %s", code, body)
	}
	for _, want := range []string{
		`upgradescope_http_requests_total{code="421",route="host-refused"} 9`,
		`upgradescope_http_request_duration_seconds_count{route="host-refused"} 9`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, "attacker.example") || strings.Contains(l, `code="421"`) && !strings.Contains(l, `route="host-refused"`) {
			t.Errorf("/metrics labels a refusal by its Host or path: %s", l)
		}
	}
}

// The trusted-header mode trusts every connection from a trusted address,
// and kubectl port-forward delivers a browser's from 127.0.0.1 with its
// Host unchanged: a rebound page read every team it could name (#240). The
// guard is on in that mode wherever the server listens, and the Host is
// checked before the header (or a token) is looked at.
func TestTrustedHeaderModeRefusesForeignHosts(t *testing.T) {
	header := "X-Forwarded-Groups"
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	s, _, ts, _ := scopeServer(t, func(c *Config) { c.ReadToken, c.TrustTeamHeader, c.TrustedProxies = "", header, loopback })
	if !s.hostGuardActive() {
		t.Fatal("trusted-header mode: the Host guard is off")
	}
	addr := strings.TrimPrefix(ts.URL, "http://")
	_, port, _ := net.SplitHostPort(addr)
	teams := "interns,payments,web"
	for _, path := range []string{"/api/v1/clusters", "/api/v1/fleet", "/metrics"} {
		code, body := hostRequest(t, addr, http.MethodGet, path, "attacker.example:"+port, header, teams)
		if code != http.StatusMisdirectedRequest {
			t.Errorf("%s with Host attacker.example and the header = %d %s, want 421", path, code, body)
		}
		if strings.Contains(body, "calm") || strings.Contains(body, "payments") {
			t.Errorf("%s refused with 421 still names a cluster or team: %s", path, body)
		}
	}
	if code, body := hostRequest(t, addr, http.MethodGet, "/api/v1/clusters", "localhost:"+port, header, teams); code != http.StatusOK || !strings.Contains(body, "calm") {
		t.Errorf("GET /api/v1/clusters with Host localhost and the header = %d %s, want 200 with payments' clusters", code, body)
	}
}

// The Host is checked before any credential: a store whose token lookups
// fail would answer 500 had the guard run after them.
func TestHostCheckPrecedesCredentials(t *testing.T) {
	st := newFakeStore()
	st.errs["ValidToken"] = errors.New("store down")
	st.errs["ValidReadToken"] = errors.New("store down")
	st.errs["ListReadTokens"] = errors.New("store down")
	s := newTestServer(t, st, func(c *Config) {
		c.TrustTeamHeader, c.TrustedProxies = "X-Forwarded-Groups", []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	})
	for _, path := range []string{"/api/v1/clusters", "/api/v1/snapshots"} {
		method := http.MethodGet
		if path == "/api/v1/snapshots" {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Host = "attacker.example"
		req.Header.Set("Authorization", "Bearer some-token")
		req.Header.Set("X-Forwarded-Groups", "payments")
		req.RemoteAddr = "127.0.0.1:5555"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("%s %s: %d %s, want 421 before any token lookup", method, path, rec.Code, rec.Body)
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s %s: the 421 carries no security headers", method, path)
		}
	}
}

// The names the guard answers for: the --listen host, every
// --allowed-host (any port, any case, a trailing dot), loopback literals,
// localhost, and an IP literal that is the address the request arrived on
// (the kubelet probes, and Prometheus scrapes, the pod's IP: no health
// endpoint needs an exemption). Nothing else, *.localhost included.
func TestHostAllowed(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) {
		c.Listen = "serve.internal:8080"
		c.AllowedHosts = []string{"upgradescope.example.com", "Upgradescope-Server.upgradescope.svc.", "10.96.0.10"}
	})
	local := &net.TCPAddr{IP: net.ParseIP("10.42.0.7"), Port: 8080}
	for host, want := range map[string]bool{
		"upgradescope.example.com":                  true,
		"UPGRADESCOPE.example.com:443":              true,
		"upgradescope.example.com.":                 true,
		"upgradescope-server.upgradescope.svc":      true,
		"upgradescope-server.upgradescope.svc:8080": true,
		"serve.internal:8080":                       true,
		"10.42.0.7:8080":                            true, // the address it arrived on: a probe
		"10.96.0.10:80":                             true,
		"127.0.0.1:8080":                            true,
		"[::1]:8080":                                true,
		"[::ffff:127.0.0.1]:8080":                   true,
		"localhost":                                 true,
		"10.42.0.8:8080":                            false, // another pod's address
		"evil.localhost":                            false,
		"example.com":                               false,
		"upgradescope.example.com.evil.test":        false,
		"":                                          false,
	} {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Host = host
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, net.Addr(local)))
		if got := s.hostAllowed(req); got != want {
			t.Errorf("Host %q: allowed = %v, want %v", host, got, want)
		}
	}
}

// An unspecified --listen host (0.0.0.0, ::) is where serve binds, not a
// name anyone reaches it under, so like a loopback one it adds nothing to
// the allow-list: a Host of 0.0.0.0 is refused while the guard is on.
func TestAllowedHostsSkipAnUnspecifiedListenHost(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:8080", "[::]:8080", "[::ffff:0.0.0.0]:8080", "127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
		got, err := allowedHostsOf(Config{Listen: listen, AllowedHosts: []string{"upgradescope.example.com"}})
		if err != nil {
			t.Fatalf("--listen %s: %v", listen, err)
		}
		if !slices.Equal(got, []string{"upgradescope.example.com"}) {
			t.Errorf("--listen %s: allowed hosts %v, want only the --allowed-host", listen, got)
		}
	}
	if got, _ := allowedHostsOf(Config{Listen: "10.42.0.7:8080"}); !slices.Equal(got, []string{"10.42.0.7"}) {
		t.Errorf("--listen 10.42.0.7:8080: allowed hosts %v, want [10.42.0.7]", got)
	}
	s := newTestServer(t, newFakeStore(), func(c *Config) {
		c.Listen = "0.0.0.0:8080"
		c.TrustTeamHeader, c.TrustedProxies = "X-Forwarded-Groups", []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	})
	local := &net.TCPAddr{IP: net.ParseIP("10.42.0.7"), Port: 8080}
	for host, want := range map[string]bool{"0.0.0.0:8080": false, "0.0.0.0": false, "[::]:8080": false, "10.42.0.7:8080": true} {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Host = host
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, net.Addr(local)))
		if got := s.hostAllowed(req); got != want {
			t.Errorf("--listen 0.0.0.0:8080, Host %q: allowed = %v, want %v", host, got, want)
		}
	}
}

// --allowed-host takes a name or an address, never a port, scheme or path:
// a typo must fail the start, not leave the name refused.
func TestNewRefusesInvalidAllowedHosts(t *testing.T) {
	for _, bad := range []string{"", "https://upgradescope.example.com", "upgradescope.example.com:443", "a/b", "*.example.com", "a..b", "-a.example"} {
		_, err := New(Config{Store: newFakeStore(), AllowedHosts: []string{bad}})
		if err == nil {
			t.Errorf("AllowedHosts %q: New succeeded, want an error", bad)
		}
	}
	for _, good := range []string{"upgradescope.example.com", "10.0.0.1", "[fd00::1]", "::1", "Upgradescope.EXAMPLE.com."} {
		if _, err := New(Config{Store: newFakeStore(), AllowedHosts: []string{good}}); err != nil {
			t.Errorf("AllowedHosts %q: %v", good, err)
		}
	}
}

// With a read token on a routable address, and without the trusted
// header, nothing changes: any Host is answered.
func TestRoutableServerWithTokenAnswersAnyHost(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = "localhost:0"; c.ReadToken = "r" })
	fakeBind(t, s, "172.17.0.2")
	startServer(t, s)
	if s.hostGuardActive() {
		t.Fatal("a routable server with a read token turned the Host guard on")
	}
	_, port, _ := net.SplitHostPort(s.Addr()) // fakeBind's listener is really on 127.0.0.1
	code, body := hostRequest(t, "127.0.0.1:"+port, http.MethodGet, "/api/v1/clusters", "upgradescope.example.com", "Authorization", "Bearer r")
	if code != http.StatusOK {
		t.Errorf("Host upgradescope.example.com = %d %s, want 200", code, body)
	}
	s2 := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = "localhost:0"; c.AllowAnonymousRead = true })
	fakeBind(t, s2, "0.0.0.0")
	startServer(t, s2)
	_, port2, _ := net.SplitHostPort(s2.Addr())
	if code, body := hostRequest(t, "127.0.0.1:"+port2, http.MethodGet, "/api/v1/clusters", "anything.example"); code != http.StatusOK {
		t.Errorf("--allow-anonymous-read on a routable address, Host anything.example = %d %s, want 200", code, body)
	}
}

func ExampleParseAllowedHost() {
	for _, raw := range []string{"Upgradescope.Example.com.", "[fd00::1]", "upgradescope.example.com:443"} {
		h, err := ParseAllowedHost(raw)
		fmt.Println(h, err != nil)
	}
	// Output:
	// upgradescope.example.com false
	// fd00::1 false
	//  true
}
