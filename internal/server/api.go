package server

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// DefaultMaxSnapshotBytes caps the snapshot push body when
// Config.MaxSnapshotBytes is unset — enforced on the wire bytes AND on the
// decompressed stream (gzip-bomb guard).
const DefaultMaxSnapshotBytes = 20 << 20 // 20 MiB

// DefaultMaxGateBytes caps the /gate manifest stream when
// Config.MaxGateBytes is unset. Lower than the snapshot cap: the gate body
// is buffered whole and is reachable with only the read token (or none).
const DefaultMaxGateBytes = 10 << 20 // 10 MiB

// Manifest stream shape limits for /gate, checked on the raw bytes before
// the request waits for the evaluation slot (checkManifestStream), and
// what aliases expand to in the slot, before decoding (checkAliases).
//
// Decoding costs memory per YAML node, not per byte: each document becomes
// a yaml.v3 node tree and kubectl's generic tree, and list items become
// objects. Measured live heap (4 MiB documents) runs from ~9 bytes per
// input byte for one big string to ~170 for a flow sequence `[1,1,…]` and
// ~330 for a List of empty items; the flow sequence peaked at ~900 MB of
// heap with garbage. Per node it is at most ~390 bytes, and a sequence
// entry (a possible list item) costs about four nodes. So the whole
// stream's node count (yamlCost.units) is capped at maxManifestUnits:
// the worst stream within it decodes in ~155 MB of live heap
// (TestGateDecodeHeapIsBounded), and a realistic ~4 MiB `kubectl get -o
// yaml` List of Deployments is ~360k units and fits. A larger one is
// refused with 413 asking to split it.
//
// 4 MiB per document fits any single Kubernetes object (etcd stores at
// most ~1.5 MiB) and caps the cheapest shape, one big value, at ~40 MB.
// The document count bounds CPU on streams of many tiny documents.
const (
	maxManifestDocBytes = 4 << 20
	maxManifestDocs     = 20000
	maxManifestUnits    = 400_000
)

// sizeString renders a byte limit for error messages: "20MiB" when it is
// a whole number of MiB, otherwise "1024 bytes".
func sizeString(n int64) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return fmt.Sprintf("%dMiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

func (s *Server) maxSnapshotBytes() int64 {
	if s.cfg.MaxSnapshotBytes > 0 {
		return s.cfg.MaxSnapshotBytes
	}
	return DefaultMaxSnapshotBytes
}

func (s *Server) maxGateBytes() int64 {
	if s.cfg.MaxGateBytes > 0 {
		return s.cfg.MaxGateBytes
	}
	return DefaultMaxGateBytes
}

// byteBudget caps the request body bytes held in memory across all
// requests of one kind (/gate manifest streams, snapshot pushes). It never
// blocks: a charge either fits now or is refused.
type byteBudget struct {
	mu   sync.Mutex
	used int64
	max  int64
}

func newByteBudget(limit int64) *byteBudget {
	return &byteBudget{max: limit}
}

// charge adds n bytes to a request that already holds held. If they do
// not fit, it gives back held in the same step and reports false, so a
// refused request's bytes never count against another request's charge.
func (b *byteBudget) charge(held, n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.max {
		b.used -= held
		return false
	}
	b.used += n
	return true
}

func (b *byteBudget) give(n int64) {
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
}

func (b *byteBudget) inUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// fits reports whether n more bytes would fit now.
func (b *byteBudget) fits(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used+n <= b.max
}

// Request bodies are read into chunks: the first is minBodyChunk bytes and
// each next one is as large as everything received so far, up to
// maxBodyChunk. A client that declares a large Content-Length and then
// sends little has little allocated, and a large body costs no copying.
const (
	minBodyChunk = 512
	maxBodyChunk = 64 << 10
)

// bufferedBody is a request body held in memory, in the chunks it was
// read in.
type bufferedBody [][]byte

// reader returns a fresh reader over the whole body.
func (b bufferedBody) reader() io.Reader {
	rs := make([]io.Reader, len(b))
	for i, c := range b {
		rs[i] = bytes.NewReader(c)
	}
	return io.MultiReader(rs...)
}

// bytes returns the body as one slice: its only chunk, or a copy.
func (b bufferedBody) bytes() []byte {
	if len(b) == 1 {
		return b[0]
	}
	return bytes.Join(b, nil)
}

// bodyError is why a request body was refused: the status and message to
// answer with.
type bodyError struct {
	status int
	msg    string
}

func (e *bodyError) write(w http.ResponseWriter) {
	if e.status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "10")
	}
	errJSON(w, e.status, e.msg)
}

// errBodyTooLarge ends a stream that grew past its cap after decoding (a
// decompressed snapshot); readBody answers it like *http.MaxBytesError.
var errBodyTooLarge = errors.New("body too large")

// readBody reads src into memory under a shared budget. Each read is
// charged for exactly the bytes it returned, as they arrive. declared (the
// Content-Length, or -1) only sizes the reads, so it reserves nothing: a
// client that stalls mid-upload holds only what it has sent, until
// ReadTimeout ends its request. When a charge does not fit, the request
// gives back everything it holds, in the same step, and is refused with
// 503 (busy) at once. Nothing waits for budget, let alone while holding
// some, so concurrent uploads cannot deadlock or queue behind a stalled
// one. A body over its cap is refused with 413 (tooLarge) whether or not
// its last bytes fit the budget, and one whose body does not arrive within
// ReadTimeout with 408. feed, when not nil, sees each piece as it is read
// and may refuse the body. On success the caller gives back held when it
// no longer needs the body; on failure everything is given back already.
func readBody(src io.Reader, declared int64, budget *byteBudget, busy, tooLarge string, feed func([]byte) *bodyError) (body bufferedBody, held int64, berr *bodyError) {
	for {
		if len(body) == 0 || len(body[len(body)-1]) == cap(body[len(body)-1]) {
			size := min(max(held, minBodyChunk), maxBodyChunk)
			if declared >= 0 {
				size = min(size, declared-held)
			}
			if size == 0 {
				break // the whole declared Content-Length is in
			}
			body = append(body, make([]byte, 0, size))
		}
		chunk := body[len(body)-1]
		n, err := src.Read(chunk[len(chunk):cap(chunk)])
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) || errors.Is(err, errBodyTooLarge) {
			// The read that crosses the cap returns the last bytes under
			// it: the request is too large whether or not they would fit.
			budget.give(held)
			return nil, 0, &bodyError{http.StatusRequestEntityTooLarge, tooLarge}
		}
		if n > 0 {
			if !budget.charge(held, int64(n)) { // gives back held too
				return nil, 0, &bodyError{http.StatusServiceUnavailable, busy}
			}
			held += int64(n)
			body[len(body)-1] = chunk[:len(chunk)+n]
			if feed != nil {
				if berr := feed(chunk[len(chunk) : len(chunk)+n]); berr != nil {
					budget.give(held)
					return nil, 0, berr
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			budget.give(held)
			return nil, 0, readError(err)
		}
	}
	return body, held, nil
}

// readError says why reading a request body failed without echoing the
// error, which names the connection's socket addresses. A body that did
// not arrive within ReadTimeout is 408, which clients (the agent among
// them) retry. Its message names no duration: the handler cannot see the
// ReadTimeout of the http.Server it runs in, which an embedder sets.
func readError(err error) *bodyError {
	var corrupt flate.CorruptInputError
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return &bodyError{http.StatusRequestTimeout,
			"the request body did not arrive within the server's read timeout; send it faster or make it smaller"}
	case errors.Is(err, gzip.ErrHeader) || errors.Is(err, gzip.ErrChecksum) || errors.As(err, &corrupt):
		return &bodyError{http.StatusUnprocessableEntity, "body is not valid gzip"}
	}
	return &bodyError{http.StatusBadRequest, "the request body ended early or could not be read"}
}

// firstRead is how many bytes a body's first read can return: the first
// chunk, or less when the body is declared smaller.
func firstRead(declared int64) int64 {
	if declared >= 0 {
		return min(declared, minBodyChunk)
	}
	return minBodyChunk
}

// readManifestBody reads a /gate manifest stream under the body cap and the
// shared buffered-body budget (see readBody), then checks its shape before
// anything decodes it (checkManifestStream). It writes the error itself.
// When the budget has no room even for the first read, the request is
// refused before reading any of its body: the first read would send a
// client waiting on Expect: 100-continue the go-ahead for all of it. On
// success the caller must run shape.checkAliases in the evaluation slot
// before decoding the body, and call release once it no longer needs the
// body; release is idempotent. On failure everything has already been
// given back.
func (s *Server) readManifestBody(w http.ResponseWriter, r *http.Request) (body bufferedBody, shape *manifestShape, release func(), ok bool) {
	const busy = "too many concurrent gate requests; retry shortly"
	limit := s.maxGateBytes()
	tooLarge := "manifest stream exceeds the " + sizeString(limit) + " limit"
	if r.ContentLength > limit {
		errJSON(w, http.StatusRequestEntityTooLarge, tooLarge)
		return nil, nil, nil, false
	}
	if r.ContentLength != 0 && !s.gateBuffered.fits(firstRead(r.ContentLength)) {
		(&bodyError{http.StatusServiceUnavailable, busy}).write(w)
		return nil, nil, nil, false
	}
	body, held, berr := readBody(http.MaxBytesReader(w, r.Body, limit), r.ContentLength, s.gateBuffered, busy, tooLarge, nil)
	if berr != nil {
		berr.write(w)
		return nil, nil, nil, false
	}
	release = sync.OnceFunc(func() { s.gateBuffered.give(held) })
	shape, status, msg := checkManifestStream(body)
	if status != 0 {
		release()
		errJSON(w, status, msg)
		return nil, nil, nil, false
	}
	return body, shape, release, true
}

// manifestShape is what checkManifestStream measured of a stream it let
// through: the YAML nodes it holds, each alias counted once, and the
// documents with aliases, whose expansion checkAliases measures.
type manifestShape struct {
	src     *byteSource
	total   yamlCost
	aliased []manifestDoc
}

// manifestDoc is the nth document of a stream, at src[start:end], which
// the meter measured at cost.
type manifestDoc struct {
	n, start, end int
	cost          yamlCost
}

// checkManifestStream splits a buffered manifest stream into documents as
// kubectl does (a line starting with "---" ends one; only white space or a
// comment may follow it) and checks it against the shape limits before
// anything decodes it: the document count, each document's size, and the
// YAML nodes the whole stream holds. It reads the body in place, in
// memory that does not grow with it, so it runs before the request waits
// for the evaluation slot; what aliases expand to is checked in the slot
// (checkAliases). It returns the status and message to refuse the stream
// with, or 0 and the stream's shape.
func checkManifestStream(body bufferedBody) (shape *manifestShape, status int, msg string) {
	src := newByteSource(body)
	if off := src.utf16BOM(); off >= 0 {
		// yaml.v3 and kubectl's decoder read a document that starts with
		// one as UTF-16, which the meter, reading UTF-8, cannot measure.
		return nil, http.StatusUnprocessableEntity, fmt.Sprintf(
			"invalid manifest stream: a UTF-16 byte order mark at byte %d; /gate reads UTF-8 only", off)
	}
	shape = &manifestShape{src: src}
	n := 0
	check := func(start, end int) (int, string) {
		if start == end {
			return 0, ""
		}
		if n++; n > maxManifestDocs {
			return http.StatusRequestEntityTooLarge, fmt.Sprintf("manifest stream has more than %d documents", maxManifestDocs)
		}
		if end-start > maxManifestDocBytes {
			return http.StatusRequestEntityTooLarge,
				fmt.Sprintf("manifest document %d is %d bytes, over the %s per-document limit (split large Lists into separate documents)",
					n, end-start, sizeString(maxManifestDocBytes))
		}
		cost := measureYAMLRange(src, start, end)
		if cost.aliases > 0 {
			shape.aliased = append(shape.aliased, manifestDoc{n: n, start: start, end: end, cost: cost})
		}
		return shape.charge(n, cost, cost)
	}
	docStart := 0
	for off := 0; off < src.size; {
		next := off
		for next < src.size && src.at(next) != '\n' {
			next++
		}
		next++ // past the newline
		if src.at(off) == '-' && src.at(off+1) == '-' && src.at(off+2) == '-' {
			i := off + 3
			for i < next { // white space, as the stream parser's bytes.TrimSpace
				r, size := src.runeAt(i)
				if !unicode.IsSpace(r) {
					break
				}
				i += size
			}
			if c := src.at(i); i < min(next, src.size) && c != '\n' && c != '#' {
				return nil, http.StatusUnprocessableEntity, fmt.Sprintf(
					"invalid manifest stream: invalid YAML document separator at byte %d (only a comment may follow ---)", off)
			}
			if status, msg := check(docStart, off); status != 0 {
				return nil, status, msg
			}
			docStart = min(next, src.size)
		}
		off = next
	}
	if status, msg := check(docStart, src.size); status != 0 {
		return nil, status, msg
	}
	return shape, 0, ""
}

// charge adds what document n adds to the stream's cost, and refuses the
// stream once it is over the node budget, naming the document when it is
// over the budget alone at docCost.
func (m *manifestShape) charge(n int, docCost, added yamlCost) (status int, msg string) {
	if m.total = m.total.add(added); m.total.units() > maxManifestUnits {
		what := "the manifest stream"
		if docCost.units() > maxManifestUnits {
			what = fmt.Sprintf("manifest document %d", n)
		}
		return http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"%s is too large to evaluate in one request: decoding YAML costs memory per node, and the stream holds "+
				"over %d node units (each node 1, each sequence entry 4, each alias what it names); split it into several requests "+
				"(a large List into separate, smaller ones)", what, maxManifestUnits)
	}
	return 0, ""
}

// checkAliases charges each document with aliases what kubectl's decoder
// expands them to (aliasExpansion): their nodes against the stream's node
// budget, and their bytes against the document's size and against
// maxStreamBytes (the body cap), which the stream with every alias
// expanded must fit as the stream itself does. Without the stream's
// charge, documents each within 4 MiB added up: 136 of 40 KB that named
// 100 objects each after one anchor answered 532 MB. Measuring that
// reads the document into yaml.v3 nodes, which costs about what decoding
// it does (no more: the stream is within the node budget), so it runs in
// the evaluation slot, one document at a time, and requests waiting for
// the slot hold no node trees. It returns the status and message to
// refuse the stream with, or 0.
func (m *manifestShape) checkAliases(maxStreamBytes int64) (status int, msg string) {
	stream := int64(m.src.size)
	for _, d := range m.aliased {
		extra, scalars, err := aliasExpansion(m.src.slice(d.start, d.end))
		if err != nil {
			return http.StatusUnprocessableEntity, fmt.Sprintf(
				"invalid manifest document %d: %v (a document that holds YAML aliases must parse, so that what they expand to can be measured)", d.n, err)
		}
		if size := d.end - d.start + scalars; size > maxManifestDocBytes {
			return http.StatusRequestEntityTooLarge, fmt.Sprintf(
				"manifest document %d is too large to evaluate in one request: its YAML aliases expand it to %d bytes, "+
					"and every alias is decoded as a full copy of what it names; the per-document limit is %s "+
					"(write the values out, or split it into smaller documents)", d.n, size, sizeString(maxManifestDocBytes))
		}
		if stream += int64(scalars); stream > maxStreamBytes {
			return http.StatusRequestEntityTooLarge, fmt.Sprintf(
				"the manifest stream is too large to evaluate in one request: its YAML aliases expand it past the %s limit "+
					"(by document %d), and every alias is decoded as a full copy of what it names "+
					"(write the values out, or split the stream into several requests)", sizeString(maxStreamBytes), d.n)
		}
		if status, msg := m.charge(d.n, d.cost.add(extra), extra); status != 0 {
			return status, msg
		}
	}
	return 0, ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = encodeJSON(w, v)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// internalErr logs the real error server-side and returns a fixed 500 body:
// store/internal error text (paths, DSNs, SQL) must never reach clients.
// 4xx responses keep their detail — that's useful to agents and operators.
func internalErr(w http.ResponseWriter, what string, err error) {
	log.Printf("server: %s: %v", what, err)
	errJSON(w, http.StatusInternalServerError, "internal error")
}

// bearerToken extracts the raw "Authorization: Bearer <token>" value, ""
// when the header is absent or malformed. The scheme is case-insensitive
// (RFC 7235).
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return h[len(prefix):]
}

// authIngest authorizes a snapshot push. Two token kinds are accepted:
// the optional shared Config.IngestToken (fleet-wide; single-cluster/dev
// setups — when it is "" only per-cluster tokens work) and per-cluster
// tokens minted via `upgradescope tokens create` (P3, spec §8) — those
// return the cluster name the token is bound to.
// boundCluster == "" means the push may target any cluster. The HTTP error
// (401 / 500) is written here; the cluster-match check happens in
// handleIngest once the body names its cluster (403 there).
func (s *Server) authIngest(w http.ResponseWriter, r *http.Request) (boundCluster string, ok bool) {
	token := bearerToken(r)
	if token != "" && s.cfg.IngestToken != "" &&
		subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.IngestToken)) == 1 {
		return "", true
	}
	if token == "" {
		errJSON(w, http.StatusUnauthorized, "invalid or missing bearer token")
		return "", false
	}
	name, valid, err := s.cfg.Store.ValidToken(r.Context(), token)
	if err != nil {
		internalErr(w, "validating ingest token", err)
		return "", false
	}
	if !valid {
		errJSON(w, http.StatusUnauthorized, "invalid or missing bearer token")
		return "", false
	}
	return name, true
}

// maxSnapshotUnits caps what decoding one snapshot push may build, in
// jsonCost units. Decoding costs memory per JSON value, not per byte:
// measured heap per unit is at most ~66 bytes (a map of unique keys; a
// list of `{}` structs is ~50 per unit), so the worst push within it
// decodes, evaluates and stores in up to ~216 MiB of heap on SQLite at
// the 20 MiB size cap, with four --targets and notifications, ~119 MiB
// with neither (TestIngestDecodeHeapIsBounded,
// TestStoredSnapshotHeapIsBounded), where 20 MiB of `{}`
// ObjectRefs used to take ~2.6 GB. A real agent's inventory is far below
// it: API usage covers only the APIs the knowledge base flags, with at
// most 100 object refs each (~1,200 units), so even 5,000 nodes (~55k)
// and a few hundred flagged kinds fit.
const maxSnapshotUnits = 1_000_000

// readSnapshotBody reads a snapshot push, decompressing gzip, under the
// snapshot cap (on the wire and decompressed: a tiny gzip bomb must not
// bypass it), the shared buffered-body budget (see readBody, which
// charges the decompressed bytes) and the node budget, which it checks as
// the bytes arrive, so a push over it is refused without reading the
// rest. It writes the error itself; on success the caller must call
// release (idempotent) once it no longer needs the body.
func (s *Server) readSnapshotBody(w http.ResponseWriter, r *http.Request) (body bufferedBody, release func(), ok bool) {
	const busy = "too many concurrent snapshot pushes; retry shortly"
	limit := s.maxSnapshotBytes()
	tooLarge := "snapshot exceeds the " + sizeString(limit) + " limit (on the wire and after decompression)"
	if r.ContentLength > limit {
		errJSON(w, http.StatusRequestEntityTooLarge, tooLarge)
		return nil, nil, false
	}
	enc := r.Header.Get("Content-Encoding")
	if enc != "" && enc != "identity" && enc != "gzip" {
		errJSON(w, http.StatusUnsupportedMediaType, fmt.Sprintf("unsupported Content-Encoding %q (use gzip or identity)", enc))
		return nil, nil, false
	}
	if r.ContentLength != 0 && !s.ingestBuffered.fits(firstRead(r.ContentLength)) {
		(&bodyError{http.StatusServiceUnavailable, busy}).write(w)
		return nil, nil, false
	}
	var src io.Reader = http.MaxBytesReader(w, r.Body, limit)
	declared := r.ContentLength
	if enc == "gzip" {
		gz, err := gzip.NewReader(src)
		if err != nil {
			var mbe *http.MaxBytesError
			switch berr := readError(err); {
			case errors.As(err, &mbe):
				errJSON(w, http.StatusRequestEntityTooLarge, tooLarge)
			case berr.status == http.StatusRequestTimeout:
				berr.write(w)
			default:
				errJSON(w, http.StatusUnprocessableEntity, "body is not valid gzip")
			}
			return nil, nil, false
		}
		defer gz.Close()
		src, declared = &capReader{r: gz, left: limit}, -1
	}
	var meter jsonMeter
	feed := func(p []byte) *bodyError {
		if meter.feed(p); meter.cost.units() > maxSnapshotUnits {
			return &bodyError{http.StatusRequestEntityTooLarge, fmt.Sprintf(
				"snapshot is too large to evaluate: decoding JSON costs memory per value, and it holds over %d units "+
					"(each value 1, each object 8)", maxSnapshotUnits)}
		}
		return nil
	}
	body, held, berr := readBody(src, declared, s.ingestBuffered, busy, tooLarge, feed)
	if berr != nil {
		berr.write(w)
		return nil, nil, false
	}
	return body, sync.OnceFunc(func() { s.ingestBuffered.give(held) }), true
}

// capReader passes at most left bytes through and then fails with
// errBodyTooLarge if there are more.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		var one [1]byte
		n, err := c.r.Read(one[:])
		if n > 0 {
			return 0, errBodyTooLarge
		}
		return 0, err
	}
	n, err := c.r.Read(p[:min(int64(len(p)), c.left)])
	c.left -= int64(n)
	return n, err
}

// pushRequest is the snapshot push protocol body (schemaVersion 1).
type pushRequest struct {
	SchemaVersion int             `json:"schemaVersion"`
	ClusterName   string          `json:"clusterName"`
	AgentVersion  string          `json:"agentVersion"`
	KBVersion     string          `json:"kbVersion"`
	Inventory     json.RawMessage `json:"inventory"`
}

// handleIngest implements POST /api/v1/snapshots: bearer auth, gzip or
// identity body under the size, node and buffered-body budgets
// (readSnapshotBody), the ingest slot, schema validation, cluster upsert (409 on a cluster UID
// conflict), then ingestSnapshot: every target evaluated and committed
// with the snapshot in one transaction (202), or — for a duplicate of the
// latest snapshot — stale evaluations refreshed (200 duplicate).
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	boundCluster, ok := s.authIngest(w, r)
	if !ok {
		return
	}
	// A per-cluster token is checked again in the commit's transaction: the
	// push can take seconds to read, queue and evaluate, and a revoke in
	// between must stop it.
	var pushToken string
	if boundCluster != "" {
		pushToken = bearerToken(r)
	}
	body, release, ok := s.readSnapshotBody(w, r)
	if !ok {
		return
	}
	defer release()
	// One push is decoded, evaluated and stored at a time: each costs up
	// to ~216 MiB of heap at the size and node caps, four --targets and
	// notifications (TestStoredSnapshotHeapIsBounded). Waiting pushes hold
	// only their bodies, which the budget bounds.
	releaseSlot, ok := acquireSlot(w, nil, s.ingestSlots, s.ingestQueueTimeout, "too many concurrent snapshot pushes; retry shortly")
	if !ok {
		return
	}
	defer releaseSlot()
	raw := body.bytes()
	// raw (a copy, or the body's only chunk) is the slot's to hold now: one
	// ingest at a time, so giving the budget back here still bounds it.
	release()
	// encoding/json would decode each invalid byte to U+FFFD, three bytes
	// in every report that names it; JSON is UTF-8, and agents write it.
	if !utf8.Valid(raw) {
		errJSON(w, http.StatusUnprocessableEntity, "invalid JSON: the body is not valid UTF-8")
		return
	}
	var req pushRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		errJSON(w, http.StatusUnprocessableEntity, "invalid JSON: "+err.Error())
		return
	}
	if req.SchemaVersion != 1 {
		errJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf("unsupported schemaVersion %d (want 1)", req.SchemaVersion))
		return
	}
	if req.ClusterName == "" {
		errJSON(w, http.StatusUnprocessableEntity, "clusterName is required")
		return
	}
	// A cluster name is an RFC 1123 subdomain, checked before anything is
	// stored: "../<script>x" registered a cluster of that name (#37).
	if err := inventory.ValidateClusterName(req.ClusterName); err != nil {
		errJSON(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// Per-cluster tokens authenticate exactly one cluster. 403 (not 401):
	// the token is genuine, the target cluster is what's wrong. Checked
	// before any store write so a mismatched push registers nothing.
	if boundCluster != "" && boundCluster != req.ClusterName {
		errJSON(w, http.StatusForbidden,
			fmt.Sprintf("token is not valid for cluster %q", req.ClusterName))
		return
	}
	inv, msg := decodePushedInventory(req.Inventory)
	if msg != "" {
		errJSON(w, http.StatusUnprocessableEntity, msg)
		return
	}
	// The dedup hash is over a canonical form: the parsed inventory
	// re-marshaled, so wire key order and whitespace never change it.
	// Struct fields marshal in declared order; map keys marshal sorted.
	// CollectedAt is zeroed to match the agent's snapshotHash canonical
	// form (it changes every tick; hashing it would make force-sync pushes
	// never dedup to 200 duplicate). APIServerStartTime is zeroed too: it
	// says which apiserver answered the /metrics scrape, and an agent whose
	// connection moves between HA apiservers would otherwise store a new
	// snapshot and history point at every move (#204). The snapshot itself
	// stores the inventory as pushed, so collectedAt, the start time and
	// fields this server does not know (a newer agent's) are kept for a
	// server that does; a push that differs only in those is a duplicate,
	// since nothing judged changed.
	hashed := inv
	hashed.CollectedAt, hashed.APIServerStartTime = time.Time{}, time.Time{}
	hash, err := canonicalHash(hashed)
	if err != nil {
		internalErr(w, "canonicalizing inventory", err)
		return
	}

	ctx := r.Context()
	now := s.now()
	// The cluster is registered (or touched) in the same transaction as the
	// snapshot, so a push that fails to commit leaves no cluster row and no
	// last-seen bump. The lookup here only decides early: a UID conflict
	// needs no evaluation, and a known cluster's latest snapshot decides
	// whether this push is a duplicate.
	cluster := store.Cluster{Name: req.ClusterName, ClusterUID: inv.ClusterID, LastSeen: now}
	existing, err := s.cfg.Store.ClusterByName(ctx, req.ClusterName)
	switch {
	case err == nil:
		if existing.ClusterUID != "" && existing.ClusterUID != inv.ClusterID {
			writeUIDConflict(w, &store.ClusterUIDConflictError{Name: req.ClusterName, StoredUID: existing.ClusterUID, PushedUID: inv.ClusterID})
			return
		}
		cluster.ID = existing.ID
	case !errors.Is(err, store.ErrNotFound):
		internalErr(w, "loading cluster", err)
		return
	}
	// Detached from the request context: once the agent has sent the body,
	// its disconnecting must not abort the commit (it would retry and get a
	// duplicate); the transaction keeps the write all-or-nothing either way.
	snapID, duplicate, err := s.ingestSnapshot(context.WithoutCancel(ctx), cluster, store.Snapshot{
		ClusterID:     cluster.ID,
		Hash:          hash,
		KBVersion:     req.KBVersion,
		AgentVersion:  req.AgentVersion,
		ReceivedAt:    now,
		ServerVersion: inv.ServerVersion, // "" (degraded): ingestSnapshot inherits the last one
		Inventory:     req.Inventory,
	}, legacyView(inv, req.AgentVersion), pushToken)
	if errors.Is(err, store.ErrTokenRevoked) {
		errJSON(w, http.StatusUnauthorized, "invalid or missing bearer token: it was revoked while this push was processed, and nothing was stored")
		return
	}
	if errors.Is(err, store.ErrClusterChanged) {
		errJSON(w, http.StatusConflict, fmt.Sprintf(
			"cluster %q was renamed or deleted while this push was processed, and nothing was stored: push again", req.ClusterName))
		return
	}
	var conflict *store.ClusterUIDConflictError
	if errors.As(err, &conflict) { // another push bound the name meanwhile
		writeUIDConflict(w, conflict)
		return
	}
	if errors.Is(err, store.ErrConflict) {
		// Other writers replaced the notification baseline maxIngestAttempts
		// times while this push was evaluated: nothing was stored.
		w.Header().Set("Retry-After", "10")
		errJSON(w, http.StatusServiceUnavailable, "the cluster changed while this push was evaluated, and nothing was stored; retry shortly")
		return
	}
	var tooLarge *reportTooLargeError
	if errors.As(err, &tooLarge) {
		// Nothing was stored: every target is evaluated before the commit.
		errJSON(w, http.StatusRequestEntityTooLarge, err.Error()+
			"; no cluster's inventory names that much, and nothing was stored")
		return
	}
	if err != nil {
		internalErr(w, "storing snapshot and evaluations", err)
		return
	}
	if duplicate {
		writeJSON(w, http.StatusOK, map[string]any{"snapshotId": snapID, "duplicate": true})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"snapshotId": snapID})
}

// decodePushedInventory parses and checks a pushed inventory, returning a
// 422 message for one the server cannot judge: absent or null, a source
// other than a cluster, or refused by inventory.Admit, which the MCP
// server's inventory_file shares: another schemaVersion (which includes {}
// and a missing one), a collectorSchema this server does not know, a
// serverVersion that is not a Kubernetes 1.x version, an identifier (a
// namespace, object, node or Helm release name, a team label value) that
// is not valid for what it names, or a value beyond the limits collectors
// keep to (inventory.ValidateLimits), once the free text collectors copy
// whole is cut to them (inventory.CutFreeText). A degraded inventory with no
// serverVersion at all (the versions collector failed) is accepted and
// judged at the cluster's last reported version (ingestSnapshot).
func decodePushedInventory(raw json.RawMessage) (inventory.Inventory, string) {
	var inv inventory.Inventory
	if t := bytes.TrimSpace(raw); len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return inv, "inventory is required"
	}
	if err := json.Unmarshal(raw, &inv); err != nil {
		return inv, "invalid inventory: " + err.Error()
	}
	// The source decides what the engine requires before it calls a
	// cluster ready: a files inventory needs no versions or add-ons. Only
	// the agent pushes, and it collects from a cluster ("" from v0.1.x),
	// so another source is a claim that would turn a blocked cluster
	// ready on evidence the push does not carry (#194). This is ingest's
	// alone: the MCP server judges files inventories too.
	if inv.Source != "" && inv.Source != inventory.SourceCluster {
		return inv, fmt.Sprintf("unsupported inventory source %q (snapshots are cluster inventories: want %q or none)", inv.Source, inventory.SourceCluster)
	}
	if err := inv.Admit(); err != nil {
		var ie *inventory.IdentifierError
		var le *inventory.LimitError
		if errors.As(err, &ie) || errors.As(err, &le) {
			return inv, "invalid inventory: inventory." + err.Error()
		}
		return inv, err.Error()
	}
	return inv, ""
}

// writeUIDConflict answers a push whose clusterId does not match the one
// its cluster name is bound to.
func writeUIDConflict(w http.ResponseWriter, conflict *store.ClusterUIDConflictError) {
	// Two clusters reporting one name would interleave their snapshots in
	// one history and flap every score and alert, so the second one is
	// refused until an operator decides which cluster the name means. A
	// push without a clusterId is refused the same way: it cannot show
	// that it is the cluster the name is bound to.
	if conflict.PushedUID == "" {
		errJSON(w, http.StatusConflict, fmt.Sprintf(
			"cluster name %q is registered to clusterId %s, but this push carries no clusterId "+
				"(the agent could not read the kube-system namespace, which needs get on namespaces). "+
				"Fix the agent's access, or give it a distinct --cluster-name if it is another cluster",
			conflict.Name, conflict.StoredUID))
		return
	}
	errJSON(w, http.StatusConflict, fmt.Sprintf(
		"cluster name %q is registered to clusterId %s, but this push comes from clusterId %s. "+
			"If this is a different cluster, give its agent a distinct --cluster-name (chart value clusterName). "+
			"If the cluster was rebuilt, remove the old record (and its history) with "+
			"'upgradescope clusters delete %s --server <this server>' (admin token), then push again; the delete also removes the name's "+
			"per-cluster ingest tokens, so mint a new one with 'upgradescope tokens create %s' if the agent used one",
		conflict.Name, conflict.StoredUID, conflict.PushedUID, conflict.Name, conflict.Name))
}

// ----- read API -----

// readAuth (scope.go) gates every read handler and gives it its scope.

// handleHealthz is always unauthenticated: liveness probes carry no tokens.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// pathClusterID parses the {id} path value, writing the 400 itself on failure.
func (s *Server) pathClusterID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "invalid cluster id")
		return 0, false
	}
	return id, true
}

// requireCluster 404s (JSON) for unknown clusters so every per-cluster
// endpoint shares one existence check. A cluster outside the request's
// scope is answered exactly as an unknown one, so its id says nothing:
// the scope is looked up first, whether the id exists or not, and an id
// outside it is never looked up, so both cost the same one query.
func (s *Server) requireCluster(w http.ResponseWriter, r *http.Request) (store.Cluster, bool) {
	id, ok := s.pathClusterID(w, r)
	if !ok {
		return store.Cluster{}, false
	}
	in, err := s.inScope(r.Context(), scopeOf(r), id)
	var c store.Cluster
	switch {
	case err != nil:
	case !in:
		err = store.ErrNotFound
	default:
		c, err = s.cfg.Store.GetCluster(r.Context(), id)
	}
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "cluster not found")
		return store.Cluster{}, false
	}
	if err != nil {
		internalErr(w, "loading cluster", err)
		return store.Cluster{}, false
	}
	return c, true
}

// defaultTarget computes a cluster's default evaluation target: the next
// minor above the version its latest snapshot is judged at (versionOf).
// No inventory is loaded for a snapshot that stores its version.
// Errors: store.ErrNotFound (no snapshots) or an unparseable version.
func (s *Server) defaultTarget(ctx context.Context, clusterID int64) (inventory.Version, error) {
	head, err := s.cfg.Store.LatestSnapshotHead(ctx, clusterID)
	if err != nil {
		return inventory.Version{}, err
	}
	version, err := s.versionOf(ctx, head)
	if err != nil {
		return inventory.Version{}, err
	}
	server, err := inventory.ParseVersion(version)
	if err != nil {
		return inventory.Version{}, fmt.Errorf("latest snapshot has no parseable server version: %w", err)
	}
	return server.Next(), nil
}

// resolveTarget picks the evaluation target: explicit ?target= (422 when
// unparseable), else the cluster's default target (404 when there is no
// snapshot to derive one from). Writes the error response itself.
func (s *Server) resolveTarget(w http.ResponseWriter, r *http.Request, clusterID int64) (inventory.Version, bool) {
	if q := r.URL.Query().Get("target"); q != "" {
		v, err := inventory.ParseTarget(q)
		if err != nil {
			errJSON(w, http.StatusUnprocessableEntity, "invalid target: "+err.Error())
			return inventory.Version{}, false
		}
		return v, true
	}
	target, err := s.defaultTarget(r.Context(), clusterID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			errJSON(w, http.StatusNotFound, "no snapshots for cluster")
		} else {
			errJSON(w, http.StatusUnprocessableEntity, "cannot derive default target: "+err.Error())
		}
		return inventory.Version{}, false
	}
	return target, true
}

// errCorruptInventory marks a stored snapshot whose inventory JSON does not
// decode.
var errCorruptInventory = errors.New("stored inventory is corrupt")

// latestInventory loads and decodes the cluster's latest snapshot, as this
// server judges it (legacyView of the pushing agent's version).
// store.ErrNotFound means the cluster has no snapshots.
func (s *Server) latestInventory(ctx context.Context, clusterID int64) (store.Snapshot, inventory.Inventory, error) {
	snap, err := s.cfg.Store.LatestSnapshot(ctx, clusterID)
	if err != nil {
		return store.Snapshot{}, inventory.Inventory{}, err
	}
	inv, err := decodeInventory(snap)
	if err != nil {
		return store.Snapshot{}, inventory.Inventory{}, err
	}
	return snap, inv, nil
}

// decodeInventory decodes a stored snapshot's whole inventory, as this
// server judges it (legacyView), its free text cut as ingest cuts it
// (inventory.CutFreeText): the snapshot keeps the inventory as pushed, an
// older agent's longer reasons and ignore annotations included. Its cost
// follows the inventory's structure (~45 MB of heap for a snapshot at its
// node budget), so a request handler calls it only in the read slot
// (inReadSlot).
func decodeInventory(snap store.Snapshot) (inventory.Inventory, error) {
	var inv inventory.Inventory
	if err := json.Unmarshal(snap.Inventory, &inv); err != nil {
		return inventory.Inventory{}, fmt.Errorf("cluster %d (snapshot %d): %w: %v", snap.ClusterID, snap.ID, errCorruptInventory, err)
	}
	inv.CutFreeText()
	return legacyView(inv, snap.AgentVersion), nil
}

// latestHead loads the cluster's latest snapshot and decodes only its
// head, the server version and capabilities, as this server judges them
// (legacyView). encoding/json still checks that the whole document is
// valid JSON, so a truncated one is corrupt here too, but it builds
// nothing for what it skips: a 370 KB snapshot that decodes whole to
// ~45 MB of heap costs about its own bytes here. The snapshot, inventory
// bytes included, is returned for a caller that turns out to need the
// rest (decodeInventory).
func (s *Server) latestHead(ctx context.Context, clusterID int64) (store.Snapshot, inventory.Inventory, error) {
	snap, err := s.cfg.Store.LatestSnapshot(ctx, clusterID)
	if err != nil {
		return store.Snapshot{}, inventory.Inventory{}, err
	}
	// Source and CollectorSchema decide how legacyView judges the head, so
	// it is judged exactly as decodeInventory judges the whole (#194).
	var head struct {
		Source          inventory.Source                                    `json:"source"`
		CollectorSchema int                                                 `json:"collectorSchema"`
		ServerVersion   string                                              `json:"serverVersion"`
		Capabilities    map[inventory.Capability]inventory.CapabilityStatus `json:"capabilities"`
	}
	if err := json.Unmarshal(snap.Inventory, &head); err != nil {
		return store.Snapshot{}, inventory.Inventory{}, fmt.Errorf("cluster %d (snapshot %d): %w: %v", clusterID, snap.ID, errCorruptInventory, err)
	}
	inv := inventory.Inventory{Source: head.Source, CollectorSchema: head.CollectorSchema, ServerVersion: head.ServerVersion, Capabilities: head.Capabilities}
	inv.CutFreeText()
	return snap, legacyView(inv, snap.AgentVersion), nil
}

// evalSummary is the read API's compact evaluation view. Evaluations are
// always the cluster's current ones (of its latest snapshot).
type evalSummary struct {
	Target      string         `json:"target"`
	Score       int            `json:"score"`
	Ready       bool           `json:"ready"`
	Verdict     engine.Verdict `json:"verdict"`
	Blockers    int            `json:"blockers"`
	Warnings    int            `json:"warnings"`
	KBVersion   string         `json:"kbVersion"`
	EvaluatedAt time.Time      `json:"evaluatedAt"` // last confirmed; a re-evaluation with an unchanged result moves it
	SnapshotID  int64          `json:"snapshotId"`
	Outdated    bool           `json:"outdated,omitempty"` // evaluated before today UTC or under another KB or team map; the next pass replaces it
	// NotAssessed is the report's, bounded (gapsOf): what the verdict
	// could not cover. NotAssessedOmitted counts the gaps not listed.
	NotAssessed        []summaryGap `json:"notAssessed,omitempty"`
	NotAssessedOmitted int          `json:"notAssessedOmitted,omitempty"`
}

// summarize is e as the read API carries it, its notAssessed within budget
// bytes (gapsOf; 0 = every gap, each cut).
func (s *Server) summarize(e store.Evaluation, now time.Time, budget int) evalSummary {
	gaps, omitted := gapsOf(e, budget)
	return evalSummary{
		Target:             e.Target,
		Score:              e.Score,
		Ready:              e.Ready,
		Verdict:            verdictOf(e),
		Blockers:           e.Blockers,
		Warnings:           e.Warnings,
		KBVersion:          e.KBVersion,
		EvaluatedAt:        e.EvaluatedAt,
		SnapshotID:         e.SnapshotID,
		Outdated:           s.outdated(e, now),
		NotAssessed:        gaps,
		NotAssessedOmitted: omitted,
	}
}

// summaryGap is a report's gap as an evaluation summary carries it (cut
// by gapsOf), with SkippedOmitted counting the skipped entries it does
// not list. The report has every gap whole.
type summaryGap struct {
	engine.CapabilityGap
	SkippedOmitted int `json:"skippedOmitted,omitempty"`
}

// fleetSummaryBytes is how much of what an evaluation could not assess
// the fleet-wide reads (/clusters, /fleet) list per evaluation, encoded.
// They carry a summary for every cluster and target, and a push within
// the inventory limits may name 32 capabilities: each gap cut to the
// store column's bounds is up to ~900 bytes (more with escapes), so
// listing them all made a 500-cluster /fleet of three targets a 10 MB
// answer that grew the heap 45 MiB, almost three times the ~16 MiB the
// server's worst case allows each of its two fleet reads. A genuine gap is a few hundred
// bytes, so the gaps of a typical evaluation fit, the required ones
// (which make a verdict unknown) listed first; the rest are counted, and
// a cluster's own detail and its report list them all.
const fleetSummaryBytes = 1 << 10

// gapsOf decodes the evaluation's notAssessed, which the store keeps beside
// the report, cut to its bounds, so the summaries that carry a verdict also
// say what it could not cover without loading the report. It lists the
// required gaps (those that make a verdict unknown) first, then the rest,
// each in the report's order, and with budget > 0 stops before the gap
// that would take the list past budget bytes encoded, returning how many
// it left out. Each gap is cut to the column's bounds again, for a row an
// earlier build wrote with wider ones. A report that does not decode has
// none; the report endpoint says it is corrupt.
func gapsOf(e store.Evaluation, budget int) ([]summaryGap, int) {
	var all []summaryGap
	if json.Unmarshal(e.NotAssessed, &all) != nil || len(all) == 0 {
		return nil, 0
	}
	gaps := make([]summaryGap, 0, len(all))
	size := len("[]")
	full := false
	for _, required := range []bool{true, false} {
		for _, g := range all {
			if g.Required != required || full {
				continue
			}
			g.Capability = inventory.Capability(store.CutString(string(g.Capability), store.SummaryCapabilityBytes))
			g.Reason = store.CutString(g.Reason, store.SummaryReasonBytes)
			if n := len(g.Skipped) - store.SummarySkipped; n > 0 {
				g.Skipped, g.SkippedOmitted = g.Skipped[:store.SummarySkipped], g.SkippedOmitted+n
			}
			for i, s := range g.Skipped {
				g.Skipped[i] = store.CutString(s, store.SummarySkippedBytes)
			}
			if budget > 0 {
				enc, err := marshalJSON(g)
				if err != nil || size+len(enc)+1 > budget {
					full = true
					continue
				}
				size += len(enc) + 1
			}
			gaps = append(gaps, g)
		}
	}
	if len(gaps) == 0 {
		gaps = nil
	}
	return gaps, len(all) - len(gaps)
}

type clusterSummary struct {
	store.Cluster
	Stale  bool         `json:"stale"`            // no push within the server's --stale-after
	Latest *evalSummary `json:"latest,omitempty"` // default-target evaluation, if any
}

// handleListClusters: GET /api/v1/clusters — every cluster plus its current
// default-target score summary (omitted when no snapshot/evaluation exists).
// Snapshot heads come from one store call (clusterStates), so no inventory
// is decoded, and each summary is one CurrentEvaluationSummary, which
// loads no report: the request costs about its response, whatever the
// fleet pushed.
func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	states, err := s.clusterStates(ctx, scopeOf(r))
	if err != nil {
		internalErr(w, "listing clusters", err)
		return
	}
	out := make([]clusterSummary, 0, len(states))
	now := s.now()
	for _, c := range states {
		cs := clusterSummary{Cluster: c.Cluster, Stale: s.clusterStale(c.Cluster, now)}
		if server, err := inventory.ParseVersion(c.version); c.hasSnapshot && err == nil {
			target := server.Next()
			if e, err := s.cfg.Store.CurrentEvaluationSummary(ctx, c.ID, target.String()); err == nil {
				sum := s.summarize(e, now, fleetSummaryBytes)
				sum.NotAssessed = scopeOf(r).summaryGaps(sum.NotAssessed)
				cs.Latest = &sum
			}
		}
		out = append(out, cs)
	}
	writeJSON(w, http.StatusOK, out)
}

type clusterDetail struct {
	store.Cluster
	Stale         bool                                                `json:"stale"` // no push within the server's --stale-after
	ServerVersion string                                              `json:"serverVersion,omitempty"`
	Capabilities  map[inventory.Capability]inventory.CapabilityStatus `json:"capabilities,omitempty"`
	Evaluations   []evalSummary                                       `json:"evaluations"`
}

// handleGetCluster: GET /api/v1/clusters/{id} — cluster row, the latest
// snapshot's server version and capability map, and the current evaluation
// summaries for the default target plus every applicable extra target.
func (s *Server) handleGetCluster(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireCluster(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	now := s.now()
	detail := clusterDetail{Cluster: c, Stale: s.clusterStale(c, now), Evaluations: []evalSummary{}}
	var targets []inventory.Version
	if snap, head, err := s.latestHead(ctx, c.ID); err == nil {
		detail.ServerVersion = judgedAt(snap, head)
		detail.Capabilities = scopeOf(r).capabilities(head.Capabilities)
		targets = s.evalTargets(detail.ServerVersion)
	} else {
		targets = s.extraTargets
	}
	for _, t := range targets {
		if e, err := s.cfg.Store.CurrentEvaluationSummary(ctx, c.ID, t.String()); err == nil {
			sum := s.summarize(e, now, 0)
			sum.NotAssessed = scopeOf(r).summaryGaps(sum.NotAssessed)
			detail.Evaluations = append(detail.Evaluations, sum)
		}
	}
	writeJSON(w, http.StatusOK, detail)
}

// Report sources: a stored evaluation of the latest snapshot, or a what-if
// computed on request from the latest snapshot.
const (
	sourceStored = "stored"
	sourceWhatIf = "what-if"
)

// reportMeta says what a served report is: which snapshot, when it was
// evaluated, whether it was stored or computed, and whether the target is
// one the cluster already runs.
type reportMeta struct {
	EvaluatedAt   time.Time `json:"evaluatedAt"`
	SnapshotID    int64     `json:"snapshotId"`
	Source        string    `json:"source"`                  // sourceStored | sourceWhatIf
	ServerVersion string    `json:"serverVersion,omitempty"` // the version the latest snapshot is judged at (judgedVersion)
	NotApplicable bool      `json:"notApplicable,omitempty"` // target at or below ServerVersion
	Outdated      bool      `json:"outdated,omitempty"`      // a stored evaluation the next pass replaces (evalSummary.Outdated)
	// nsTeams is the evaluated inventory's namespace teams
	// (namespaceTeams), for a scoped read only: what readScope cuts by.
	nsTeams map[string]string
}

// loadOrComputeReport returns the current stored evaluation's report for
// (cluster, target) when one exists, else a what-if computed from the
// latest snapshot. Only a missing evaluation (store.ErrNotFound) falls
// through to the what-if path — any other store failure is returned, never
// masked by a recompute that would hide a broken store behind a 200.
// A store.ErrNotFound result means the cluster has no snapshots at all.
// A stored report of a fleet-wide read loads no inventory (the version
// is the snapshot head's, versionOf); a what-if decodes the whole
// inventory, and a read scoped to teams its namespaces
// (reportMeta.nsTeams).
func (s *Server) loadOrComputeReport(ctx context.Context, clusterID int64, target inventory.Version, sc readScope) (engine.Report, reportMeta, error) {
	head, err := s.cfg.Store.LatestSnapshotHead(ctx, clusterID)
	if err != nil {
		return engine.Report{}, reportMeta{}, err
	}
	version, err := s.versionOf(ctx, head)
	if err != nil {
		return engine.Report{}, reportMeta{}, err
	}
	meta := reportMeta{ServerVersion: version, NotApplicable: notApplicable(version, target)}
	var snap store.Snapshot // the inventory, loaded only when needed
	loadSnapshot := func() error {
		if snap.Inventory != nil {
			return nil
		}
		snap, err = s.cfg.Store.LatestSnapshot(ctx, clusterID)
		return err
	}
	if !sc.fleet() {
		if err := loadSnapshot(); err != nil {
			return engine.Report{}, reportMeta{}, err
		}
		if meta.nsTeams, err = s.namespaceTeams(snap); err != nil {
			return engine.Report{}, reportMeta{}, err
		}
	}
	e, err := s.cfg.Store.CurrentEvaluation(ctx, clusterID, target.String())
	switch {
	case err == nil:
		var rep engine.Report
		if err := json.Unmarshal(e.Report, &rep); err != nil {
			return engine.Report{}, reportMeta{}, fmt.Errorf("stored report for evaluation %d is corrupt: %w", e.ID, err)
		}
		meta.EvaluatedAt, meta.SnapshotID, meta.Source = e.EvaluatedAt, e.SnapshotID, sourceStored
		meta.Outdated = s.outdated(e, s.now())
		return rep, meta, nil
	case errors.Is(err, store.ErrNotFound):
		if err := loadSnapshot(); err != nil {
			return engine.Report{}, reportMeta{}, err
		}
		inv, err := decodeInventory(snap)
		if err != nil {
			return engine.Report{}, reportMeta{}, err
		}
		now := s.now().UTC() // as stored evaluations read back
		meta.EvaluatedAt, meta.SnapshotID, meta.Source = now, snap.ID, sourceWhatIf
		rep, err := s.evaluateWhatIf(inv, target, now)
		return rep, meta, err
	default:
		return engine.Report{}, reportMeta{}, fmt.Errorf("loading current evaluation: %w", err)
	}
}

// reportForRequest is the shared resolve-cluster → resolve-target → load/
// compute pipeline behind the report, findings and teams endpoints.
func (s *Server) reportForRequest(w http.ResponseWriter, r *http.Request) (engine.Report, reportMeta, bool) {
	c, ok := s.requireCluster(w, r)
	if !ok {
		return engine.Report{}, reportMeta{}, false
	}
	target, ok := s.resolveTarget(w, r, c.ID)
	if !ok {
		return engine.Report{}, reportMeta{}, false
	}
	rep, meta, err := s.loadOrComputeReport(r.Context(), c.ID, target, scopeOf(r))
	if errors.Is(err, store.ErrNotFound) {
		errJSON(w, http.StatusNotFound, "no snapshots for cluster")
		return engine.Report{}, reportMeta{}, false
	}
	var tooLarge *reportTooLargeError
	if errors.As(err, &tooLarge) {
		errJSON(w, http.StatusRequestEntityTooLarge, "what-if: "+err.Error())
		return engine.Report{}, reportMeta{}, false
	}
	if err != nil {
		internalErr(w, "loading or computing report", err)
		return engine.Report{}, reportMeta{}, false
	}
	return rep, meta, true
}

// reportResponse is the report endpoint's body: the engine report, per-team
// scores, and what the report is (reportMeta).
type reportResponse struct {
	reportWithTeams
	reportMeta
}

// handleReport: GET /api/v1/clusters/{id}/report?target= — full engine.Report
// plus presentation-time per-team scores (`teams`, omitted when empty) and
// evaluatedAt/snapshotId/source.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	rep, meta, ok := s.reportForRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, reportResponse{s.versioned(withTeamsIn(rep, scopeOf(r), meta.nsTeams)), meta})
}

// handleFindings: GET /api/v1/clusters/{id}/findings?target=&severity=&category=
// — the report's findings, exact-match filtered. Unknown filter values simply
// match nothing.
func (s *Server) handleFindings(w http.ResponseWriter, r *http.Request) {
	rep, meta, ok := s.reportForRequest(w, r)
	if !ok {
		return
	}
	severity := r.URL.Query().Get("severity")
	category := r.URL.Query().Get("category")
	findings := []engine.Finding{} // non-nil so JSON renders []
	for _, f := range scopeOf(r).findings(rep.Findings, meta.nsTeams) {
		if severity != "" && string(f.Severity) != severity {
			continue
		}
		if category != "" && string(f.Category) != category {
			continue
		}
		findings = append(findings, f)
	}
	writeJSON(w, http.StatusOK, struct {
		Target   string           `json:"target"`
		Findings []engine.Finding `json:"findings"`
		reportMeta
	}{rep.Target.String(), findings, meta})
}

// maxHistoryLimit caps /history's ?limit=, so the response does not grow
// with how long the server has kept evaluations.
const maxHistoryLimit = 1000

// handleHistory: GET /api/v1/clusters/{id}/history?target=&limit= —
// []store.ScorePoint, oldest first, default limit 100, at most
// maxHistoryLimit.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	c, ok := s.requireCluster(w, r)
	if !ok {
		return
	}
	target, ok := s.resolveTarget(w, r, c.ID)
	if !ok {
		return
	}
	limit := 100
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 || n > maxHistoryLimit {
			errJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf("limit must be an integer from 1 to %d", maxHistoryLimit))
			return
		}
		limit = n
	}
	points, err := s.cfg.Store.ScoreHistory(r.Context(), c.ID, target.String(), limit)
	if err != nil {
		internalErr(w, "loading history", err)
		return
	}
	if points == nil {
		points = []store.ScorePoint{}
	}
	writeJSON(w, http.StatusOK, points)
}
