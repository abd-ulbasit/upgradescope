package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
	"github.com/abd-ulbasit/upgradescope/internal/server/notify"
	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

const webhookSchemaPath = "../../api/webhook.schema.json"

// compileWebhookSchema compiles api/webhook.schema.json and returns it
// with its parsed form, for the field check.
func compileWebhookSchema(t *testing.T) (*jsonschema.Schema, any) {
	t.Helper()
	raw, err := os.ReadFile(webhookSchemaPath)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource("webhook.schema.json", doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile("webhook.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return sch, doc
}

func checkWebhookPayload(t *testing.T, sch *jsonschema.Schema, schemaDoc any, name string, body []byte) {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s: not JSON: %v", name, err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Errorf("%s does not validate against api/webhook.schema.json: %v\n%s", name, err, body)
	}
	if extra := unlistedFields(schemaDoc, schemaDoc, inst, ""); len(extra) > 0 {
		t.Errorf("%s has fields api/webhook.schema.json does not list: %v", name, extra)
	}
}

// TestWebhookPayloadMatchesSchema: what the generic webhook really sends,
// through ingest, the outbox and a signed delivery, validates against the
// published schema, and so does the documented example.
func TestWebhookPayloadMatchesSchema(t *testing.T) {
	sch, schemaDoc := compileWebhookSchema(t)

	var mu sync.Mutex
	var bodies [][]byte
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if got, want := r.Header.Get(notify.SignatureHeader), notify.Sign("s3cret", b); got != want {
			t.Errorf("signature header %q, want %q", got, want)
		}
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
	}))
	defer receiver.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	hook := notify.NewGenericWebhook(receiver.URL)
	hook.Secret = "s3cret"
	srv, err := New(Config{Store: st, KB: k, Notifier: hook, IngestToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	clean := inventory.Inventory{
		SchemaVersion:   1,
		ClusterID:       "uid-1",
		CollectorSchema: inventory.CurrentCollectorSchema,
		CollectedAt:     time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		ServerVersion:   "v1.35.0",
		Capabilities:    collectedCaps(),
	}
	withPSP := clean
	withPSP.APIUsage = []inventory.APIUsage{{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 1}}
	ctx := context.Background()
	// blocked (the baseline, sends nothing), ready again, blocked again.
	for _, inv := range []inventory.Inventory{withPSP, clean, withPSP} {
		pushInventory(t, ts.URL, "tok", inv)
		srv.deliverOutbox(ctx)
	}

	mu.Lock()
	defer mu.Unlock()
	kinds := map[string]bool{}
	for i, b := range bodies {
		checkWebhookPayload(t, sch, schemaDoc, fmt.Sprintf("delivery %d", i+1), b)
		var n notify.Notification
		if err := json.Unmarshal(b, &n); err != nil {
			t.Fatal(err)
		}
		for _, c := range n.Changes {
			kinds[c.Kind] = true
		}
	}
	if !kinds[notify.KindBecameReady] || !kinds[notify.KindNewBlocker] {
		t.Fatalf("deliveries cover kinds %v, want became-ready and new-blocker: %d bodies", kinds, len(bodies))
	}

	// The wire-format fixture and the example on the reference page cover
	// eol-approaching and omitted.
	example, err := os.ReadFile("notify/testdata/notification_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	checkWebhookPayload(t, sch, schemaDoc, "notify/testdata/notification_v1.json", example)
	page, err := os.ReadFile("../../docs/reference/webhook.md")
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(page), "```json\n")
	if block, _, _ = strings.Cut(block, "```"); !ok || block == "" {
		t.Fatal("docs/reference/webhook.md has no ```json example")
	}
	checkWebhookPayload(t, sch, schemaDoc, "the docs/reference/webhook.md example", []byte(block))

	// The schema's formats are asserted, not just annotations: a timestamp
	// that is not RFC 3339 must fail, or the checks above prove less.
	var bad map[string]any
	if err := json.Unmarshal(example, &bad); err != nil {
		t.Fatal(err)
	}
	bad["timestamp"] = "yesterday"
	b, _ := json.Marshal(bad)
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := sch.Validate(inst); err == nil {
		t.Error(`a payload with "timestamp": "yesterday" validates; the schema's date-time format is not checked`)
	}
}
