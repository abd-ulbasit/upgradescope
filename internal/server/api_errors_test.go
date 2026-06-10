package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIngestEvaluationSurvivesAgentDisconnect: the evaluation fan-out must
// run on a context detached from the request — an agent that disconnects
// right after the snapshot is durably stored must not hole the score
// history. fakeStore's InsertEvaluation/LatestEvaluation honor ctx
// cancellation, so this fails if evaluateSnapshot runs on r.Context().
func TestIngestEvaluationSurvivesAgentDisconnect(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // agent gone: request context already canceled

	req := httptest.NewRequest(http.MethodPost, "/api/v1/snapshots",
		bytes.NewReader(pushReqBody(t, testInventoryWithPSP()))).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ingest-tok")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	st.mu.Lock()
	evals := len(st.evals)
	st.mu.Unlock()
	if evals != 1 {
		t.Fatalf("stored evaluations = %d, want 1: evaluation must run detached from the request context", evals)
	}
}

// TestReportStoreFailureIsNot200WhatIf: a real LatestEvaluation failure
// (anything but ErrNotFound) must surface as a 500, not silently fall
// through to a what-if recompute that masks the store being broken.
func TestReportStoreFailureIsNot200WhatIf(t *testing.T) {
	st := newFakeStore()
	s := newTestServer(t, st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	id := seedViaPush(t, ts)

	st.errs["LatestEvaluation"] = errors.New("disk wedge: /var/lib/upgradescope")

	var body map[string]string
	resp := getJSON(t, ts, "/api/v1/clusters/1/report", "", &body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("cluster %d report status = %d, want 500", id, resp.StatusCode)
	}
	if body["error"] != "internal error" {
		t.Fatalf("error body = %q, want fixed %q", body["error"], "internal error")
	}
}

// TestInternalErrorsDoNotLeakDetail: 500 bodies carry a fixed message; the
// store's error text (which may include DSNs, paths, SQL) is logged only.
func TestInternalErrorsDoNotLeakDetail(t *testing.T) {
	const secret = "pg down: secret-dsn"

	t.Run("ingest upsert failure", func(t *testing.T) {
		st := newFakeStore()
		st.errs["UpsertCluster"] = errors.New(secret)
		s := newTestServer(t, st)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		resp, out := postSnapshot(t, ts, "ingest-tok", pushReqBody(t, testInventory()), false)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", resp.StatusCode)
		}
		if got, _ := out["error"].(string); got != "internal error" || strings.Contains(got, secret) {
			t.Fatalf("error body = %q, want fixed %q", got, "internal error")
		}
	})

	t.Run("list clusters failure", func(t *testing.T) {
		st := newFakeStore()
		st.errs["ListClusters"] = errors.New(secret)
		s := newTestServer(t, st)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		var body map[string]string
		resp := getJSON(t, ts, "/api/v1/clusters", "", &body)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", resp.StatusCode)
		}
		if body["error"] != "internal error" {
			t.Fatalf("error body = %q, want fixed %q", body["error"], "internal error")
		}
	})
}
