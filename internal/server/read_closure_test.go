package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// The closed state of the read API used to live only in the read_tokens
// rows: a lost PVC, an emptyDir restart, an older backup or a fresh
// --db-url emptied them, and the whole fleet was served to anyone (#295).
// RequireReadCredential keeps the API closed whatever the database holds.

// getWith sends GET path to addr with the given Authorization header value
// ("" = none) and Host (as hostRequest does).
func getWith(t *testing.T, addr, path, host, authorization string) (int, string) {
	t.Helper()
	if authorization == "" {
		return hostRequest(t, addr, http.MethodGet, path, host)
	}
	return hostRequest(t, addr, http.MethodGet, path, host, "Authorization", authorization)
}

// doGet serves GET path through h in memory, with an Authorization header
// when auth is not "".
func doGet(t *testing.T, h http.Handler, path, auth string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "localhost"
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// doPostGate posts an empty manifest to /api/v1/gate through h.
func doPostGate(t *testing.T, h http.Handler, auth string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gate", strings.NewReader(""))
	req.Host = "localhost"
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// loopbackProxies is a trusted-proxy range of the loopback addresses.
func loopbackProxies() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")} }

// With the flag and an empty store, on a wildcard listener: Start does not
// refuse (the API is not open), and a read with no credential, or with a
// bearer nothing knows, is 401 on every read endpoint, the gate included.
func TestRequireReadCredentialEmptyStoreStartsAndRefusesReads(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = ":0"; c.RequireReadCredential = true })
	startServer(t, s) // Start did not refuse the non-loopback listener
	_, port, _ := net.SplitHostPort(s.Addr())
	addr := net.JoinHostPort("127.0.0.1", port)
	host := "localhost:" + port
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/clusters"},
		{http.MethodGet, "/api/v1/fleet"},
		{http.MethodGet, "/api/v1/registry"},
		{http.MethodGet, "/metrics"},
		{http.MethodPost, "/api/v1/gate"},
	} {
		for _, auth := range []string{"", "Bearer unknown-tok", "Bearer ingest-tok"} {
			var hdr []string
			if auth != "" {
				hdr = []string{"Authorization", auth}
			}
			if code, body := hostRequest(t, addr, tc.method, tc.path, host, hdr...); code != http.StatusUnauthorized {
				t.Errorf("%s %s with Authorization %q = %d %s, want 401", tc.method, tc.path, auth, code, body)
			}
		}
	}
	if code, _ := getWith(t, addr, "/healthz", host, ""); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200: probes need no credential", code)
	}
}

// The flag does not depend on the store's rows: mint a token, read with it,
// swap in a fresh store (the lost PVC) or restart on one (an emptyDir, a
// restored older backup), and the same token and an anonymous read are 401.
// Without the flag a restart on an empty store reopens the API: the
// contrast the flag exists for.
func TestRequireReadCredentialSurvivesAStoreSwap(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cfg        func(*Config)
		afterSwap  int // anonymous read after the swap, same process
		afterStart int // anonymous read on a restart with an empty store
	}{
		{"require-read-credential", func(c *Config) { c.RequireReadCredential = true }, http.StatusUnauthorized, http.StatusUnauthorized},
		// The process remembers that a token was minted; a restart does not.
		{"allow-anonymous-read, the old chart default", func(c *Config) { c.AllowAnonymousRead = true }, http.StatusUnauthorized, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			s := newTestServer(t, st, func(c *Config) { c.Listen = ":0"; tc.cfg(c) })
			clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
			s.openClock = clock.now
			startServer(t, s)
			_, port, _ := net.SplitHostPort(s.Addr())
			addr, host := net.JoinHostPort("127.0.0.1", port), "localhost:"+port
			mintReadToken(t, st, "minted-tok", store.ReadScopeFleet)
			clock.set(clock.now().Add(readOpenWindow)) // an open answer kept from startup lapses
			if code, body := getWith(t, addr, "/api/v1/clusters", host, "Bearer minted-tok"); code != http.StatusOK {
				t.Fatalf("minted token = %d %s, want 200", code, body)
			}
			if code, _ := getWith(t, addr, "/api/v1/clusters", host, ""); code != http.StatusUnauthorized {
				t.Fatalf("anonymous read with a token minted = %d, want 401", code)
			}

			st.wipe() // the database is lost: an empty one, under the store's own lock
			if code, body := getWith(t, addr, "/api/v1/clusters", host, ""); code != tc.afterSwap {
				t.Errorf("anonymous read after the swap = %d %s, want %d", code, body, tc.afterSwap)
			}
			// The token the lost database held is an unknown credential now:
			// never silently upgraded to the whole fleet (#295: a team token
			// read every team's data instead of failing).
			if code, body := getWith(t, addr, "/api/v1/clusters", host, "Bearer minted-tok"); code != http.StatusUnauthorized {
				t.Errorf("a minted token the new store does not hold = %d %s, want 401", code, body)
			}

			s2 := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = ":0"; tc.cfg(c) })
			startServer(t, s2)
			_, port2, _ := net.SplitHostPort(s2.Addr())
			if code, body := getWith(t, net.JoinHostPort("127.0.0.1", port2), "/api/v1/clusters", "localhost:"+port2, ""); code != tc.afterStart {
				t.Errorf("anonymous read after a restart on an empty store = %d %s, want %d", code, body, tc.afterStart)
			}
			// A fresh process has no memory of the token. Even where the
			// restart reopens the API for anonymous reads, the old (possibly
			// team-scoped) token is an unknown credential: 401, not the whole
			// fleet. This is the case the unknown-bearer rule exists for.
			if code, body := getWith(t, net.JoinHostPort("127.0.0.1", port2), "/api/v1/clusters", "localhost:"+port2, "Bearer minted-tok"); code != http.StatusUnauthorized {
				t.Errorf("the old token after a restart on an empty store = %d %s, want 401", code, body)
			}
		})
	}
}

// --require-read-credential and --allow-anonymous-read contradict each
// other.
func TestNewRefusesRequireReadCredentialWithAllowAnonymousRead(t *testing.T) {
	_, err := New(Config{Store: newFakeStore(), RequireReadCredential: true, AllowAnonymousRead: true})
	if err == nil || !strings.Contains(err.Error(), "require-read-credential") || !strings.Contains(err.Error(), "allow-anonymous-read") {
		t.Fatalf("New with both = %v, want an error naming both flags", err)
	}
}

// With the flag, a configured credential still reads, and a configured
// --read-token or a trusted team header does not need the flag's warning.
func TestRequireReadCredentialAcceptsEveryCredential(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) {
		c.Listen = ":0"
		c.RequireReadCredential = true
		c.ReadToken = "fleet-tok"
		c.AdminToken = "admin-tok"
	})
	startServer(t, s)
	_, port, _ := net.SplitHostPort(s.Addr())
	addr, host := net.JoinHostPort("127.0.0.1", port), "localhost:"+port
	mintReadToken(t, st, "team-tok", "payments")
	for _, tok := range []string{"fleet-tok", "admin-tok", "team-tok"} {
		if code, body := getWith(t, addr, "/api/v1/clusters", host, "Bearer "+tok); code != http.StatusOK {
			t.Errorf("token %q = %d %s, want 200", tok, code, body)
		}
	}
	if code, _ := getWith(t, addr, "/api/v1/clusters", host, "Bearer nope"); code != http.StatusUnauthorized {
		t.Errorf("unknown token = %d, want 401", code)
	}
}

// On an open read API (--allow-anonymous-read, or loopback, no credential
// anywhere), a request that presents a bearer nothing knows is 401, not
// the whole fleet; a request with no Authorization header still reads.
// The documented bearers keep working: the admin token reads, and an open
// server stays open to a client that sends no bearer.
func TestOpenReadAPIRefusesAnUnknownBearer(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.AllowAnonymousRead = true; c.AdminToken = "admin-tok" })
	h := s.Handler()
	get := func(path, auth string) int {
		t.Helper()
		return doGet(t, h, path, auth)
	}
	for _, path := range []string{"/api/v1/clusters", "/api/v1/fleet", "/api/v1/registry"} {
		if code := get(path, ""); code != http.StatusOK {
			t.Errorf("GET %s with no Authorization = %d, want 200 (open read API)", path, code)
		}
		for _, auth := range []string{"Bearer unknown-tok", "bearer unknown-tok", "Bearer ingest-tok"} {
			if code := get(path, auth); code != http.StatusUnauthorized {
				t.Errorf("GET %s with Authorization %q = %d, want 401: an unknown credential is never upgraded to fleet scope", path, auth, code)
			}
		}
		// Not a bearer credential at all: the open read, as before.
		for _, auth := range []string{"Basic dTpw", "Bearer", "Bearer "} {
			if code := get(path, auth); code != http.StatusOK {
				t.Errorf("GET %s with Authorization %q = %d, want 200 (no bearer presented)", path, auth, code)
			}
		}
		if code := get(path, "Bearer admin-tok"); code != http.StatusOK {
			t.Errorf("GET %s with the admin token = %d, want 200", path, code)
		}
	}
	if code := doPostGate(t, h, "Bearer unknown-tok"); code != http.StatusUnauthorized {
		t.Errorf("POST /api/v1/gate with an unknown bearer on an open API = %d, want 401", code)
	}
}

// A store failure while validating the bearer is a 500, not an upgrade to
// the open read: the unknown-bearer rule does not turn errors into reads.
func TestOpenReadAPIBearerValidationErrorIsNotAnOpenRead(t *testing.T) {
	st := newFakeStore()
	st.errs["ValidReadToken"] = errors.New("store down")
	s := newTestServer(t, st, func(c *Config) { c.AllowAnonymousRead = true })
	if code := doGet(t, s.Handler(), "/api/v1/clusters", "Bearer some-tok"); code != http.StatusInternalServerError {
		t.Errorf("bearer with the store down = %d, want 500", code)
	}
}

// Startup warns when the flag is set and nothing can read yet, saying how
// to mint a token, and is quiet once a credential exists.
func TestRequireReadCredentialStartupWarning(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      func(*Config)
		mint     bool
		revoke   bool
		wantWarn bool
	}{
		{"no credential at all", nil, false, false, true},
		{"a minted read token", nil, true, false, false},
		{"only a revoked read token", nil, true, true, true},
		{"a --read-token", func(c *Config) { c.ReadToken = "r" }, false, false, false},
		{"a trusted team header", func(c *Config) {
			c.TrustTeamHeader, c.TrustedProxies = "X-Forwarded-Groups", loopbackProxies()
		}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			if tc.mint {
				id := mintReadToken(t, st, "minted-tok", store.ReadScopeFleet)
				if tc.revoke {
					if err := st.RevokeReadToken(context.Background(), id); err != nil {
						t.Fatal(err)
					}
				}
			}
			s := newTestServer(t, st, func(c *Config) {
				c.Listen = ":0"
				c.RequireReadCredential = true
				if tc.cfg != nil {
					tc.cfg(c)
				}
			})
			logged := captureLog(t)
			startServer(t, s)
			var warn string
			for _, l := range strings.Split(logged.String(), "\n") {
				if strings.Contains(l, "WARN") && strings.Contains(l, "--require-read-credential") {
					warn = l
				}
			}
			if tc.wantWarn != (warn != "") {
				t.Fatalf("startup WARN = %q, want one: %v\nlog:\n%s", warn, tc.wantWarn, logged)
			}
			if tc.wantWarn {
				for _, want := range []string{"401", "tokens create --read"} {
					if !strings.Contains(warn, want) {
						t.Errorf("WARN %q does not say %q", warn, want)
					}
				}
				// The command is the last thing the line says, unquoted, so
				// it can be copied as it stands (a quote around it, or inside
				// a quote, makes the shell see one word).
				const cmd = "upgradescope tokens create --read --teams '*'"
				if !strings.HasSuffix(warn, ": "+cmd) {
					t.Errorf("WARN %q does not end with the paste-ready command %q", warn, cmd)
				}
			}
			if strings.Contains(logged.String(), "open to anyone") {
				t.Errorf("a closed API logged the open-to-anyone warning:\n%s", logged)
			}
		})
	}
}

// The open-read-API startup warning still appears without the flag.
func TestOpenReadAPIStartupWarningRemains(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = ":0"; c.AllowAnonymousRead = true })
	logged := captureLog(t)
	startServer(t, s)
	if !strings.Contains(logged.String(), "open to anyone") {
		t.Errorf("no open-API warning:\n%s", logged)
	}
}

// nonLoopbackIPv4 is an address of this machine that is not loopback, to
// reach a wildcard listener the way a pod's IP is reached.
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("no interface addresses: %v", err)
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.String()
		}
	}
	t.Skip("this machine has no non-loopback IPv4 address")
	return ""
}

// The Host guard was armed only for a loopback bind. A wildcard listener
// (the chart's --listen=:8080) also accepts connections on 127.0.0.1, which
// is where kubectl port-forward and a DNS-rebinding page arrive, so an open
// read API on it answered Host: attacker.example with the fleet (#309).
// With no read credential (open), a request that names a host the server
// does not answer for is 421, over loopback and over the pod's own address.
func TestWildcardOpenReadAPIRefusesForeignHosts(t *testing.T) {
	podIP := nonLoopbackIPv4(t)
	s := newTestServer(t, newFakeStore(), func(c *Config) {
		c.Listen = ":0"
		c.AllowAnonymousRead = true
		c.AllowedHosts = []string{"upgradescope-server.monitoring.svc", "Upgradescope.Example.com"}
	})
	startServer(t, s)
	_, port, _ := net.SplitHostPort(s.Addr())
	for _, via := range []string{"127.0.0.1", podIP} {
		addr := net.JoinHostPort(via, port)
		for _, path := range []string{"/api/v1/clusters", "/api/v1/fleet", "/api/v1/registry", "/metrics", "/", "/healthz", "/readyz"} {
			for _, host := range []string{"attacker.example:" + port, "attacker.example", "127.0.0.1.nip.io:" + port, "localhost.attacker.example"} {
				if code, body := hostRequest(t, addr, http.MethodGet, path, host); code != http.StatusMisdirectedRequest {
					t.Errorf("via %s: GET %s with Host %q = %d %s, want 421", via, path, host, code, body)
				}
			}
		}
		if code, body := hostRequest(t, addr, http.MethodPost, "/api/v1/gate", "attacker.example:"+port); code != http.StatusMisdirectedRequest {
			t.Errorf("via %s: POST /api/v1/gate with Host attacker.example = %d %s, want 421", via, code, body)
		}
		// The names the server answers for: loopback and localhost (a
		// port-forward), the pod or arrival address (probes, scrapes) and
		// every --allowed-host (the chart's Service and Ingress names).
		answered := []string{
			"localhost:" + port, "localhost:8080", "127.0.0.1:" + port, "[::1]:" + port,
			"upgradescope-server.monitoring.svc:80", "upgradescope.example.com",
		}
		if via == podIP {
			answered = append(answered, podIP+":"+port, podIP)
		} else {
			// The pod's IP is the arrival address only for a request that
			// arrived on it.
			for _, host := range []string{podIP + ":" + port, podIP} {
				if code, body := hostRequest(t, addr, http.MethodGet, "/api/v1/clusters", host); code != http.StatusMisdirectedRequest {
					t.Errorf("via %s: Host %q (another address of this machine) = %d %s, want 421", via, host, code, body)
				}
			}
		}
		for _, host := range answered {
			for _, path := range []string{"/api/v1/clusters", "/metrics", "/healthz", "/readyz", "/"} {
				if code, body := hostRequest(t, addr, http.MethodGet, path, host); code != http.StatusOK {
					t.Errorf("via %s: GET %s with Host %q = %d %s, want 200", via, path, host, code, body)
				}
			}
		}
	}
}

// A kubelet probe or a Prometheus scrape names the pod's IP, which is the
// address it arrived on: the probe and scrape paths keep working on an open
// wildcard server without any --allowed-host.
func TestWildcardOpenReadAPIKeepsProbesAndScrapes(t *testing.T) {
	podIP := nonLoopbackIPv4(t)
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = ":0"; c.AllowAnonymousRead = true })
	startServer(t, s)
	_, port, _ := net.SplitHostPort(s.Addr())
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		if code, body := hostRequest(t, net.JoinHostPort(podIP, port), http.MethodGet, path, net.JoinHostPort(podIP, port)); code != http.StatusOK {
			t.Errorf("probe/scrape GET %s as the pod IP = %d %s, want 200", path, code, body)
		}
	}
}

// With a read token, on a routable address, no trusted header: nothing
// changes, any Host is answered (the guard has nothing to protect).
func TestWildcardServerWithAReadTokenAnswersAnyHost(t *testing.T) {
	podIP := nonLoopbackIPv4(t)
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = ":0"; c.ReadToken = "r" })
	startServer(t, s)
	_, port, _ := net.SplitHostPort(s.Addr())
	for _, via := range []string{"127.0.0.1", podIP} {
		if code, body := getWith(t, net.JoinHostPort(via, port), "/api/v1/clusters", "attacker.example", "Bearer r"); code != http.StatusOK {
			t.Errorf("via %s, read token, Host attacker.example = %d %s, want 200", via, code, body)
		}
	}
	s2 := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = ":0"; c.RequireReadCredential = true })
	startServer(t, s2)
	_, port2, _ := net.SplitHostPort(s2.Addr())
	if code, body := getWith(t, net.JoinHostPort("127.0.0.1", port2), "/api/v1/clusters", "upgradescope.example.com", ""); code != http.StatusUnauthorized {
		t.Errorf("--require-read-credential, any Host, no credential = %d %s, want 401 (not 421: the API is not open)", code, body)
	}
}

// Whether the guard applies is decided per request, so minting a read
// token turns it off and the store failing turns it on, without a restart.
func TestOpenReadAPIHostGuardFollowsTheStore(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.Listen = ":0"; c.AllowAnonymousRead = true })
	clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
	s.openClock = clock.now
	startServer(t, s)
	_, port, _ := net.SplitHostPort(s.Addr())
	addr := net.JoinHostPort("127.0.0.1", port)
	foreign := "attacker.example:" + port
	if code, _ := getWith(t, addr, "/api/v1/clusters", foreign, ""); code != http.StatusMisdirectedRequest {
		t.Fatalf("open, foreign Host = %d, want 421", code)
	}

	// The store cannot say whether the API is open: assume it is.
	st.setErr("ListReadTokens", errors.New("store down"))
	clock.set(clock.now().Add(readOpenWindow)) // the kept answer lapses: the store is asked, and fails
	if code, _ := getWith(t, addr, "/api/v1/clusters", foreign, ""); code != http.StatusMisdirectedRequest {
		t.Errorf("store down, foreign Host = %d, want 421 (guard on when unsure)", code)
	}
	// Names the server answers for never need the store.
	if code, _ := getWith(t, addr, "/healthz", "localhost:"+port, ""); code != http.StatusOK {
		t.Errorf("store down, /healthz as localhost = %d, want 200", code)
	}
	st.setErr("ListReadTokens", nil)

	mintReadToken(t, st, "minted-tok", store.ReadScopeFleet)
	if code, _ := getWith(t, addr, "/api/v1/clusters", foreign, ""); code != http.StatusUnauthorized {
		t.Errorf("a read token minted, anonymous read with a foreign Host = %d, want 401 (closed: no guard needed)", code)
	}
	if code, body := getWith(t, addr, "/api/v1/clusters", foreign, "Bearer minted-tok"); code != http.StatusOK {
		t.Errorf("a read token minted, the token with a foreign Host = %d %s, want 200", code, body)
	}
}

// The guard protects what no credential protects: a request that presents
// a bearer is not an anonymous read. An agent pushing to an open hub under
// a name not in --allowed-host keeps working (its token is checked), and a
// bearer nothing knows reads nothing either way.
func TestOpenReadAPIHostGuardLeavesBearerRequestsToTheirCredentials(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = ":0"; c.AllowAnonymousRead = true })
	startServer(t, s)
	_, port, _ := net.SplitHostPort(s.Addr())
	addr := net.JoinHostPort("127.0.0.1", port)
	foreign := "hub.elsewhere.example"
	if code, body := hostRequest(t, addr, http.MethodPost, "/api/v1/snapshots", foreign, "Authorization", "Bearer wrong-tok"); code != http.StatusUnauthorized {
		t.Errorf("push with a wrong token under a foreign Host = %d %s, want 401 from the token check", code, body)
	}
	if code, body := getWith(t, addr, "/api/v1/clusters", foreign, "Bearer wrong-tok"); code != http.StatusUnauthorized {
		t.Errorf("read with an unknown bearer under a foreign Host = %d %s, want 401", code, body)
	}
	if code, body := getWith(t, addr, "/api/v1/clusters", foreign, ""); code != http.StatusMisdirectedRequest {
		t.Errorf("anonymous read under a foreign Host = %d %s, want 421", code, body)
	}
}

// A loopback server with an open read API keeps its guard whatever the
// request carries (#240), and the Start-time guard still comes on for a
// loopback bind with a read token.
func TestLoopbackBindKeepsItsGuardWithAReadToken(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = "127.0.0.1:0"; c.ReadToken = "r" })
	startServer(t, s)
	_, port, _ := net.SplitHostPort(s.Addr())
	if code, _ := getWith(t, net.JoinHostPort("127.0.0.1", port), "/api/v1/clusters", "attacker.example", "Bearer r"); code != http.StatusMisdirectedRequest {
		t.Errorf("loopback bind, valid token, foreign Host = %d, want 421", code)
	}
}

// anonymousGet serves an anonymous GET of path, naming host, through h.
func anonymousGet(h http.Handler, path, host string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// On an open read API every anonymous request whose Host is not on the
// allow-list asks whether the API is open, on every path ("/", assets,
// /healthz): a store query each. The answer is kept for readOpenWindow, so a
// flood of them costs one query per window, and a freshly minted first read
// token closes the API within that window, not at once (auth.md says so).
func TestOpenReadAPIQueriesTheStoreOncePerWindow(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.Listen = ":0"; c.AllowAnonymousRead = true })
	clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
	s.openClock = clock.now
	h := s.Handler()

	for _, path := range []string{"/", "/api/v1/clusters", "/healthz", "/readyz", "/assets/app.js"} {
		for range 20 {
			if code := anonymousGet(h, path, "evil.example"); code != http.StatusMisdirectedRequest {
				t.Fatalf("open API, anonymous %s as evil.example = %d, want 421", path, code)
			}
		}
	}
	if n := st.readTokenListings(); n != 1 {
		t.Fatalf("100 anonymous requests made %d ListReadTokens queries, want 1 (one per window)", n)
	}

	// Within the window a token minted by another process (tokens create)
	// is not seen yet: the API stays open for at most readOpenWindow.
	mintReadToken(t, st, "minted-tok", store.ReadScopeFleet)
	clock.set(clock.now().Add(readOpenWindow - time.Millisecond))
	if code := anonymousGet(h, "/api/v1/clusters", "evil.example"); code != http.StatusMisdirectedRequest {
		t.Errorf("inside the window after a mint = %d, want 421 (the open answer is still kept)", code)
	}
	if n := st.readTokenListings(); n != 1 {
		t.Errorf("queries inside the window = %d, want still 1", n)
	}

	// After it the API is asked again, found closed, and closes: 401, not
	// 421 (the guard is for an open API only), and never asked again.
	clock.set(clock.now().Add(time.Millisecond))
	if code := anonymousGet(h, "/api/v1/clusters", "evil.example"); code != http.StatusUnauthorized {
		t.Errorf("after the window = %d, want 401 (a read token exists: closed)", code)
	}
	if n := st.readTokenListings(); n != 2 {
		t.Errorf("queries after the window = %d, want 2", n)
	}
	clock.set(clock.now().Add(time.Hour))
	for range 10 {
		if code := anonymousGet(h, "/api/v1/clusters", "evil.example"); code != http.StatusUnauthorized {
			t.Fatalf("closed API, anonymous read = %d, want 401", code)
		}
	}
	if n := st.readTokenListings(); n != 2 {
		t.Errorf("a closed API asked the store again: %d queries, want 2 (a minted token is remembered for good)", n)
	}
}

// An open API whose store keeps answering "no read token" is asked once a
// window for as long as it stays open, and a failing store is not cached:
// the next request asks again.
func TestOpenReadAPIWindowRenewsAndDoesNotCacheErrors(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st, func(c *Config) { c.Listen = ":0"; c.AllowAnonymousRead = true })
	clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
	s.openClock = clock.now
	h := s.Handler()
	for i := 1; i <= 3; i++ {
		anonymousGet(h, "/api/v1/clusters", "evil.example")
		anonymousGet(h, "/api/v1/clusters", "evil.example")
		if n := st.readTokenListings(); n != i {
			t.Fatalf("window %d: %d queries, want %d", i, n, i)
		}
		clock.set(clock.now().Add(readOpenWindow))
	}

	st.setErr("ListReadTokens", errors.New("store down"))
	before := st.readTokenListings()
	for range 3 {
		if code := anonymousGet(h, "/api/v1/clusters", "evil.example"); code != http.StatusMisdirectedRequest {
			t.Fatalf("store down = %d, want 421", code)
		}
	}
	if n := st.readTokenListings() - before; n != 3 {
		t.Errorf("3 requests against a failing store made %d queries, want 3 (an error is not cached)", n)
	}
}

// Concurrent anonymous requests at the start of a window share one query:
// while the first is still asking, the others wait for its answer instead of
// asking too (a slow store would otherwise be asked once per request).
func TestOpenReadAPIConcurrentRequestsShareOneQuery(t *testing.T) {
	st := newFakeStore()
	var entered atomic.Int32
	release := make(chan struct{})
	st.onListReadTokens = func() { entered.Add(1); <-release }
	s := newTestServer(t, st, func(c *Config) { c.Listen = ":0"; c.AllowAnonymousRead = true })
	clock := &fakeClock{t: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)}
	s.openClock = clock.now
	h := s.Handler()
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			anonymousGet(h, "/api/v1/clusters", "evil.example")
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for entered.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // let the others reach (and queue behind) the query
	if n := entered.Load(); n != 1 {
		t.Errorf("%d queries in flight at once, want 1 (the others wait for its answer)", n)
	}
	close(release)
	wg.Wait()
	if n := st.readTokenListings(); n != 1 {
		t.Errorf("32 concurrent anonymous requests made %d queries, want 1", n)
	}
}
