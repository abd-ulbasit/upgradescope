package server

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The Host allow-list (DNS rebinding). A page on attacker.example can make
// its own name resolve to 127.0.0.1 and then read, with same-origin
// requests, whatever a server on the victim's loopback answers: the
// browser connects to the loopback socket but still sends Host:
// attacker.example. Two kinds of server answer such a request with data
// that no credential protects: an open read API, which Start allows only
// on loopback, and the trusted-header mode, which trusts every connection
// from a --trusted-proxy-cidr address (kubectl port-forward delivers the
// browser's connections to the pod from 127.0.0.1, Host unchanged). So
// when the listener is loopback, or the team header is trusted, every
// request must name a host this server answers for, or it gets 421 before
// any route, scope or credential is looked at.
//
// The names it answers for are the ones rebinding cannot produce:
//   - an IP literal that is loopback, or the address the request arrived
//     on (a kubelet probe or a Prometheus scrape of the pod's IP): a page
//     whose origin is an IP literal was served from that IP, not rebound
//     to it;
//   - localhost;
//   - the --listen host (unless it is 0.0.0.0 or ::, an address to bind,
//     not one to reach), and every --allowed-host (Config.AllowedHosts):
//     the Service, Ingress or proxy names the operator serves it under.
//
// The port is not compared. Rebinding needs a name the attacker controls;
// the port of a loopback name or an allowed one says nothing about who
// sent the request, and it differs from the listen port wherever a
// tunnel (kubectl port-forward 9000:8080) or a Service maps it.
//
// http.NewCrossOriginProtection is no defence here: a rebound request is
// same-origin, and it exempts GET.

// hostGuardActive reports whether s refuses requests for hosts it does not
// answer for: always with a trusted team header, and once Start has bound
// a loopback address.
func (s *Server) hostGuardActive() bool {
	return s.hostGuard.Load()
}

// checkHost wraps next with the Host allow-list while it is active. A
// refusal comes before the metrics middleware, so it is counted here, under
// routeHostRefused, and logged (refusalLog).
func (s *Server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.hostGuardActive() && !s.hostAllowed(r) {
			start := time.Now()
			errJSON(w, http.StatusMisdirectedRequest, fmt.Sprintf(
				"this server does not answer for Host %q (DNS rebinding guard): "+
					"reach it as localhost or a loopback address, or add the name with serve --allowed-host", hostName(r.Host)))
			s.metrics.observe(routeHostRefused, http.StatusMisdirectedRequest, start)
			s.hostRefusals.note(s.now(), r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostRefusalLogEvery is how often at most a Host refusal is logged: a
// rebinding page or a misconfigured client repeats its request, so one
// line a minute, with the count since the last, says as much as a line
// each would, and a flood of them cannot fill the log.
const hostRefusalLogEvery = time.Minute

// maxLoggedHost bounds the Host a refusal line quotes (a DNS name is at
// most 253 bytes).
const maxLoggedHost = 256

// refusalLog rate-limits the log of Host refusals to one line every
// hostRefusalLogEvery. The zero value is ready.
type refusalLog struct {
	mu      sync.Mutex
	last    time.Time // when the last line was written; zero = never
	skipped int       // refusals since then not logged
}

// note logs r's refusal at now, unless a line was written less than
// hostRefusalLogEvery before (a clock stepped back does not count), in
// which case it is counted in the next line. The Host is quoted, escaped
// to ASCII and bounded: it is what the client sent.
func (l *refusalLog) note(now time.Time, r *http.Request) {
	l.mu.Lock()
	if d := now.Sub(l.last); !l.last.IsZero() && d >= 0 && d < hostRefusalLogEvery {
		l.skipped++
		l.mu.Unlock()
		return
	}
	skipped := l.skipped
	l.last, l.skipped = now, 0
	l.mu.Unlock()
	host, cut := r.Host, ""
	if len(host) > maxLoggedHost {
		host, cut = host[:maxLoggedHost], "…"
	}
	more := ""
	if skipped > 0 {
		more = fmt.Sprintf("; %d more refused since the last such line", skipped)
	}
	log.Printf("server: refused a request for Host %s%s from %s with 421: not a name this server answers for "+
		"(DNS rebinding guard; add one clients use with --allowed-host)%s",
		strconv.QuoteToASCII(host), cut, r.RemoteAddr, more)
}

// hostAllowed reports whether r's Host is one s answers for.
func (s *Server) hostAllowed(r *http.Request) bool {
	h := hostName(r.Host)
	if h == "" {
		return false
	}
	if h == "localhost" || slices.Contains(s.allowedHosts, h) {
		return true
	}
	ip, err := netip.ParseAddr(h)
	if err != nil {
		return false
	}
	ip = ip.Unmap().WithZone("")
	if ip.IsLoopback() {
		return true
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	ap, err := netip.ParseAddrPort(local.String())
	return err == nil && ap.Addr().Unmap().WithZone("") == ip
}

// hostName is the host of a Host header value, without its port or an
// IPv6 literal's brackets, lowercased and without a trailing dot.
func hostName(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// ParseAllowedHost checks one --allowed-host entry and returns it as the
// guard compares it: a DNS name or an IP address, without a port, scheme
// or path (any port matches), lowercased and without a trailing dot.
func ParseAllowedHost(raw string) (string, error) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if h == "" {
		return "", errors.New("empty host name")
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Unmap().WithZone("").String(), nil
	}
	if len(h) > 253 {
		return "", fmt.Errorf("%q is longer than a DNS name may be", raw)
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", fmt.Errorf("%q is not a host name: want a DNS name or an IP address without a port (any port matches)", raw)
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return "", fmt.Errorf("%q is not a host name: want a DNS name or an IP address without a port (any port matches)", raw)
			}
		}
	}
	return h, nil
}

// allowedHostsOf is what the guard compares a Host with besides loopback
// and the request's own address: cfg.AllowedHosts and the --listen host,
// unless that is localhost, a loopback or an unspecified address.
func allowedHostsOf(cfg Config) ([]string, error) {
	var out []string
	for _, raw := range cfg.AllowedHosts {
		h, err := ParseAllowedHost(raw)
		if err != nil {
			return nil, fmt.Errorf("server: invalid allowed host: %w", err)
		}
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	if lh, _, err := net.SplitHostPort(cfg.Listen); err == nil && lh != "" {
		// localhost and loopback literals are allowed already; an
		// unspecified address (0.0.0.0, ::) is where serve binds, not a
		// name it is reached under.
		if h, err := ParseAllowedHost(lh); err == nil && h != "localhost" && !loopbackOrUnspecified(h) && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out, nil
}

// loopbackOrUnspecified reports whether h is a loopback or an unspecified
// IP address.
func loopbackOrUnspecified(h string) bool {
	a, err := netip.ParseAddr(h)
	if err != nil {
		return false
	}
	a = a.Unmap()
	return a.IsLoopback() || a.IsUnspecified()
}
