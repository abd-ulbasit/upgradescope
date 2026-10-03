package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// addrListener is a real loopback listener that reports another address,
// standing in for a bind to a non-loopback interface.
type addrListener struct {
	net.Listener
	addr net.Addr
}

func (l addrListener) Addr() net.Addr { return l.addr }

// fakeBind makes s's Start bind a loopback listener that claims to be on
// ip, and returns whether Start closed it.
func fakeBind(t *testing.T, s *Server, ip string) (closed func() bool) {
	t.Helper()
	var ln net.Listener
	s.listen = func(network, _ string) (net.Listener, error) {
		real, err := net.Listen(network, "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		ln = addrListener{real, &net.TCPAddr{IP: net.ParseIP(ip), Port: real.Addr().(*net.TCPAddr).Port}}
		return ln, nil
	}
	return func() bool {
		_, err := ln.Accept()
		return err != nil
	}
}

// Whether the read API is open is decided on the address the server really
// bound, not on the --listen text: "localhost" that /etc/hosts maps to a
// routable address, a hostname, or ":8080" (every interface) is not
// loopback, and an unauthenticated read API there is refused (#126 SE-01).
func TestStartRejectsNonLoopbackBoundAddr(t *testing.T) {
	for _, ip := range []string{"172.17.0.2", "0.0.0.0", "::", "fd00::1"} {
		s := newTestServer(t, newFakeStore(), func(c *Config) { c.Listen = "localhost:0" })
		closed := fakeBind(t, s, ip)
		err := s.Start()
		if err == nil || !strings.Contains(err.Error(), "read token") {
			t.Fatalf("bound %s without a read token: Start = %v, want a refusal naming the read token", ip, err)
		}
		if !closed() {
			t.Errorf("bound %s: the listener was left open", ip)
		}
	}
	// Loopback (all of 127.0.0.0/8 and ::1), a read token, or an explicit
	// AllowAnonymousRead start.
	for _, tc := range []struct {
		ip  string
		cfg func(*Config)
	}{
		{"127.0.0.1", nil},
		{"127.3.4.5", nil},
		{"::1", nil},
		{"172.17.0.2", func(c *Config) { c.ReadToken = "r" }},
		{"0.0.0.0", func(c *Config) { c.AllowAnonymousRead = true }},
	} {
		opts := []func(*Config){func(c *Config) { c.Listen = "localhost:0" }}
		if tc.cfg != nil {
			opts = append(opts, tc.cfg)
		}
		s := newTestServer(t, newFakeStore(), opts...)
		fakeBind(t, s, tc.ip)
		startServer(t, s)
	}
}

// RFC 7235: the auth scheme is case-insensitive, so "bearer" and "BEARER"
// authenticate like "Bearer".
func TestBearerSchemeCaseInsensitive(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.ReadToken = "r" })
	for _, h := range []string{"Bearer r", "bearer r", "BEARER r"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
		req.Header.Set("Authorization", h)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Authorization %q: status = %d, want 200", h, rec.Code)
		}
	}
	for _, h := range []string{"Basic r", "Bearerr", "Bearer", "r"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/clusters", nil)
		req.Header.Set("Authorization", h)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status = %d, want 401", h, rec.Code)
		}
	}
}
