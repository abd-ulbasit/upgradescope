package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// marshalJSON writes what encoding/json would, minus the escapes that make
// a string longer than its UTF-8: HTML characters and U+2028/U+2029 are
// written as themselves. What it writes decodes to the same value.
func TestMarshalJSONDoesNotAmplify(t *testing.T) {
	v := map[string]any{
		"html":  "<a href=\"x\">&amp;</a>",
		"seps":  "a\u2028b\u2029c",
		"slash": `C:\u2028 is a backslash, then u2028`,
		"ctl":   "tab\there\x01",
		"other": "é→✓",
	}
	got, err := marshalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{`\u003c`, `\u003e`, `\u0026`, `\u2028`, `\u2029`} {
		if strings.Contains(strings.ReplaceAll(string(got), `\\u2028`, ""), unwanted) {
			t.Errorf("marshalJSON wrote %s: %s", unwanted, got)
		}
	}
	if !strings.Contains(string(got), `C:\\u2028 is a backslash`) {
		t.Errorf("an escaped backslash followed by u2028 was rewritten: %s", got)
	}
	var back map[string]any
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("output does not decode: %v (%s)", err, got)
	}
	for k, want := range v {
		if back[k] != want {
			t.Errorf("%s: decoded %q, want %q", k, back[k], want)
		}
	}
	if bytes.HasSuffix(got, []byte("\n")) {
		t.Errorf("marshalJSON ends with a newline: %q", got)
	}

	// writeJSON writes the same bytes, plus the newline json.Encoder ends
	// a value with.
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, v)
	if want := string(got) + "\n"; rec.Body.String() != want {
		t.Errorf("writeJSON = %q, want %q", rec.Body.String(), want)
	}
}

// A snapshot's strings reach its stored reports and the read API as they
// were pushed, never longer: encoding/json's HTML escapes turned each `<`
// or `&` in an object name into six bytes, so a 17 MB push of such names
// at the node budget stored three 80 MB reports (a +323 MiB ingest, and a
// +170 MiB report read) where the same push of `x` stored 17.5 MB ones.
// Names of `<`, `&` or U+2028 now store and read back exactly as long
// as names of as many bytes of `x`.
func TestSnapshotStringsAreNotAmplified(t *testing.T) {
	sizes := func(name string) (stored, read int) {
		t.Helper()
		s := newTestServer(t, newFakeStore())
		inv := testInventoryWithPSP()
		inv.APIUsage[0].Objects = []inventory.ObjectRef{{Name: name}}
		rec := httptest.NewRecorder()
		serveIngest(s, rec, pushReqBody(t, inv), false)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("push of %q: status = %d (%s)", name, rec.Code, rec.Body)
		}
		e, err := s.cfg.Store.CurrentEvaluation(context.Background(), 1, "1.35")
		if err != nil {
			t.Fatal(err)
		}
		rec = httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/clusters/1/report?target=1.35", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), name) {
			t.Fatalf("report: status = %d, name %q in it: %v", rec.Code, name, strings.Contains(rec.Body.String(), name))
		}
		return len(e.Report), rec.Body.Len()
	}
	const n = 300
	wantStored, wantRead := sizes(strings.Repeat("x", 3*n))
	for _, name := range []string{strings.Repeat("<", 3*n), strings.Repeat("&", 3*n), strings.Repeat("\u2028", n)} {
		stored, read := sizes(name)
		if stored != wantStored || read != wantRead {
			t.Errorf("names of %q: stored report %d bytes, report read %d bytes; want %d and %d, as for x",
				name[:3], stored, read, wantStored, wantRead)
		}
	}
}

// encoding/json decodes each byte of invalid UTF-8 to U+FFFD, three bytes,
// so a snapshot of it would store reports three times its size. No agent
// sends one (encoding/json writes valid UTF-8), and a push that does is
// refused with 422 before it is decoded.
func TestIngestRefusesInvalidUTF8(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	inv := testInventoryWithPSP()
	inv.APIUsage[0].Objects = []inventory.ObjectRef{{Name: "NAME"}}
	body := strings.Replace(string(pushReqBody(t, inv)), "NAME", "o-\xff\xfe", 1)
	rec := httptest.NewRecorder()
	serveIngest(s, rec, []byte(body), false)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "UTF-8") {
		t.Fatalf("status = %d (%s), want 422 naming UTF-8", rec.Code, rec.Body)
	}
	if used := s.ingestBuffered.inUse(); used != 0 {
		t.Fatalf("%d body bytes still charged", used)
	}
}

// The dedup hash is json.Marshal's, byte for byte, so a push after an
// upgrade is still a duplicate of the snapshot an older server hashed:
// HTML characters, line separators, newlines and backslashes in strings,
// and raw JSON included.
func TestCanonicalHashIsJSONMarshals(t *testing.T) {
	inv := testInventoryWithPSP()
	inv.APIUsage[0].Objects = []inventory.ObjectRef{
		{Namespace: "a<b>&c", Name: "line\nbreak    \"q\" \\ \u0001 é", Manager: "x"},
		{Name: unicodeEscape(0x3c) + " already escaped"},
	}
	for _, v := range []any{inv, map[string]any{"raw": json.RawMessage(`{"k":"<&>"}`), "s": "<<>>&&"}, "plain"} {
		want, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		got, err := canonicalHash(v)
		if err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256(want); got != hex.EncodeToString(sum[:]) {
			t.Errorf("canonicalHash(%T) = %s, want the SHA-256 of %s", v, got, want)
		}
	}
}
