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
