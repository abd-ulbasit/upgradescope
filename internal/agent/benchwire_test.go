package agent

import (
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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

// requestStat is the count of one verb and resource, the response bytes
// client-go read for them (decompressed), and the time they took in all,
// each from sending the request to the end of its body. The bytes make a
// resource's cost visible by itself (#233: the GitOps lists; #228: pods
// and nodes), not only as part of the tick's total; the time splits a cold
// tick between waiting on the network and decoding (#226). The time is a
// sum: requests in flight at once each count their own.
type requestStat struct {
	Verb     string `json:"verb"`
	Resource string `json:"resource"`
	Count    int    `json:"count"`
	Bytes    int64  `json:"bytes"`
	Millis   int64  `json:"ms"`
}

// requestEntry accumulates one verb and resource. Count is guarded by the
// recorder's mutex; bytes and millis are added without it, by the bodies
// of requests already counted, so a body still being read when the
// recorder is reset adds to the entry of the tick that sent it.
type requestEntry struct {
	count  int
	bytes  atomic.Int64
	millis atomic.Int64
}

// requestRecorder counts requests, response body bytes, and the time
// requests took, per verb and resource. Safe for concurrent use; reset
// between ticks.
type requestRecorder struct {
	mu        sync.Mutex
	entries   map[[2]string]*requestEntry
	total     int
	bodyBytes atomic.Int64
}

func newRequestRecorder() *requestRecorder {
	return &requestRecorder{entries: map[[2]string]*requestEntry{}}
}

func (r *requestRecorder) reset() {
	r.mu.Lock()
	r.entries, r.total = map[[2]string]*requestEntry{}, 0
	r.mu.Unlock()
	r.bodyBytes.Store(0)
}

// transport wraps rt (the shape of rest.Config.WrapTransport).
func (r *requestRecorder) transport(rt http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		verb, res := classifyRequest(req.Method, req.URL.Path, req.URL.RawQuery, req.Header.Get("Accept"))
		key := [2]string{verb, res}
		r.mu.Lock()
		e := r.entries[key]
		if e == nil {
			e = &requestEntry{}
			r.entries[key] = e
		}
		e.count++
		r.total++
		r.mu.Unlock()
		start := time.Now()
		resp, err := rt.RoundTrip(req)
		if err != nil || resp.Body == nil {
			e.millis.Add(time.Since(start).Milliseconds())
			return resp, err
		}
		resp.Body = &countingBody{ReadCloser: resp.Body, n: &r.bodyBytes, perResource: &e.bytes,
			done: func() { e.millis.Add(time.Since(start).Milliseconds()) }}
		return resp, err
	})
}

// stats returns the counts, most requests first.
func (r *requestRecorder) stats() (stats []requestStat, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.entries {
		stats = append(stats, requestStat{Verb: k[0], Resource: k[1], Count: e.count, Bytes: e.bytes.Load(), Millis: e.millis.Load()})
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

// countingBody adds what is read of a body to n (the tick's total) and to
// perResource (its verb and resource's), as it is read, and calls done once,
// at the body's end or when it is closed.
type countingBody struct {
	io.ReadCloser
	n           *atomic.Int64
	perResource *atomic.Int64
	done        func()
	once        sync.Once
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	b.perResource.Add(int64(n))
	if err != nil {
		b.finish()
	}
	return n, err
}

func (b *countingBody) Close() error {
	b.finish()
	return b.ReadCloser.Close()
}

func (b *countingBody) finish() {
	if b.done != nil {
		b.once.Do(b.done)
	}
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

// resetCounts zeroes the byte and connection counts, so the next reading is
// what crossed the wire, and the connections opened, since this call.
func (p *wireProxy) resetCounts() { p.up.Store(0); p.down.Store(0); p.conns.Store(0) }

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
