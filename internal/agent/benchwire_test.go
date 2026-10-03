package agent

import (
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// The scale benchmark (bench_load_test.go, #71) counts what one agent tick
// asks of the apiserver. The agent has no request-debug hook, so these
// helpers live with the test: a RoundTripper wrapper that counts requests
// by verb and resource and the response bytes client-go read, and a TCP
// forwarder that counts the bytes on the wire (after the apiserver's gzip
// compression, TLS included), which the HTTP layer cannot see.

// classifyRequest names a request the way an audit log would: its verb
// (LIST, GET, WATCH, CREATE, UPDATE, PATCH, DELETE) and resource, with
// "/sub" for a subresource and " (metadata)" when the client asked for
// PartialObjectMetadata. Requests that name no resource (/version,
// /metrics, API discovery) come back as GET with the path, discovery
// documents under "discovery".
func classifyRequest(method, path, query, accept string) (verb, resource string) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	var rest []string
	switch {
	case len(segs) >= 1 && segs[0] == "api":
		rest = segs[1:]
		if len(rest) <= 1 { // /api, /api/v1: discovery
			return "GET", "discovery"
		}
		rest = rest[1:] // the version
	case len(segs) >= 1 && segs[0] == "apis":
		rest = segs[1:]
		if len(rest) <= 2 { // /apis, /apis/<group>/<version>: discovery
			return "GET", "discovery"
		}
		rest = rest[2:] // group and version
	default:
		return method, path
	}
	if len(rest) >= 3 && rest[0] == "namespaces" { // namespaced resource
		rest = rest[2:]
	}
	resource = rest[0]
	name := ""
	if len(rest) >= 2 {
		name = rest[1]
	}
	if len(rest) >= 3 {
		resource += "/" + rest[2]
	}
	switch method {
	case http.MethodGet, http.MethodHead:
		switch {
		case strings.Contains(query, "watch=true") || strings.Contains(query, "watch=1"):
			verb = "WATCH"
		case name == "":
			verb = "LIST"
		default:
			verb = "GET"
		}
	case http.MethodPost:
		verb = "CREATE"
	case http.MethodPut:
		verb = "UPDATE"
	case http.MethodPatch:
		verb = "PATCH"
	case http.MethodDelete:
		verb = "DELETE"
	default:
		verb = method
	}
	if strings.Contains(accept, "PartialObjectMetadata") {
		resource += " (metadata)"
	}
	return verb, resource
}

// requestStat is the count of one verb and resource.
type requestStat struct {
	Verb     string `json:"verb"`
	Resource string `json:"resource"`
	Count    int    `json:"count"`
}

// requestRecorder counts requests and response body bytes. Safe for
// concurrent use; reset between ticks.
type requestRecorder struct {
	mu        sync.Mutex
	counts    map[[2]string]int
	total     int
	bodyBytes atomic.Int64
}

func newRequestRecorder() *requestRecorder {
	return &requestRecorder{counts: map[[2]string]int{}}
}

func (r *requestRecorder) reset() {
	r.mu.Lock()
	r.counts, r.total = map[[2]string]int{}, 0
	r.mu.Unlock()
	r.bodyBytes.Store(0)
}

// transport wraps rt (the shape of rest.Config.WrapTransport).
func (r *requestRecorder) transport(rt http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		verb, res := classifyRequest(req.Method, req.URL.Path, req.URL.RawQuery, req.Header.Get("Accept"))
		r.mu.Lock()
		r.counts[[2]string{verb, res}]++
		r.total++
		r.mu.Unlock()
		resp, err := rt.RoundTrip(req)
		if err == nil && resp.Body != nil {
			resp.Body = &countingBody{ReadCloser: resp.Body, n: &r.bodyBytes}
		}
		return resp, err
	})
}

// stats returns the counts, most requests first.
func (r *requestRecorder) stats() (stats []requestStat, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, n := range r.counts {
		stats = append(stats, requestStat{Verb: k[0], Resource: k[1], Count: n})
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].Count != stats[j].Count {
			return stats[i].Count > stats[j].Count
		}
		return stats[i].Verb+stats[i].Resource < stats[j].Verb+stats[j].Resource
	})
	return stats, r.total
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type countingBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

// wireProxy forwards TCP to an upstream and counts the bytes each way. The
// client keeps its own TLS session with the apiserver through it, so the
// counts are the encrypted bytes on the wire: what the network carries,
// after the apiserver compressed its responses.
type wireProxy struct {
	ln         net.Listener
	up, down   atomic.Int64 // client to server, server to client
	conns      atomic.Int64
	wg         sync.WaitGroup
	closeOnce  sync.Once
	upstreamAt string
	mu         sync.Mutex
	shut       bool
	open       map[net.Conn]struct{} // closed by close, so idle keep-alive connections do not hold it
}

func newWireProxy(upstream string) (*wireProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &wireProxy{ln: ln, upstreamAt: upstream, open: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.serve()
	return p, nil
}

func (p *wireProxy) addr() string { return p.ln.Addr().String() }

func (p *wireProxy) resetBytes() { p.up.Store(0); p.down.Store(0) }

func (p *wireProxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.conns.Add(1)
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			u, err := net.Dial("tcp", p.upstreamAt)
			if err != nil {
				c.Close()
				return
			}
			p.mu.Lock()
			p.open[c], p.open[u] = struct{}{}, struct{}{}
			shut := p.shut
			p.mu.Unlock()
			if shut { // close ran meanwhile
				c.Close()
				u.Close()
				return
			}
			defer c.Close()
			defer u.Close()
			var inner sync.WaitGroup
			inner.Add(2)
			pipe := func(dst, src net.Conn, n *atomic.Int64) {
				defer inner.Done()
				m, _ := io.Copy(dst, countingReader{src, n})
				_ = m
				if tc, ok := dst.(*net.TCPConn); ok {
					_ = tc.CloseWrite()
				}
			}
			go pipe(u, c, &p.up)
			go pipe(c, u, &p.down)
			inner.Wait()
		}()
	}
}

type countingReader struct {
	io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (p *wireProxy) close() {
	p.closeOnce.Do(func() {
		_ = p.ln.Close()
		p.mu.Lock()
		p.shut = true
		for c := range p.open {
			_ = c.Close()
		}
		p.mu.Unlock()
	})
	p.wg.Wait()
}
