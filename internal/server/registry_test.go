package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// registryResponse mirrors the wire shape of GET /api/v1/registry.
type registryTestResponse struct {
	Addons []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Support     struct {
			Status string `json:"status"`
		} `json:"support"`
	} `json:"addons"`
}

func TestRegistryEndpoint(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	var out registryTestResponse
	resp := getJSON(t, ts, "/api/v1/registry", "", &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if len(out.Addons) == 0 {
		t.Fatal("addons is empty, want the embedded registry entries")
	}
	found := false
	for i, a := range out.Addons {
		if a.ID == "" {
			t.Fatalf("addons[%d] has empty id", i)
		}
		if a.Support.Status == "" {
			t.Fatalf("addon %s has empty support.status", a.ID)
		}
		if a.ID == "ingress-nginx" {
			found = true
		}
		if i > 0 && out.Addons[i-1].ID > a.ID {
			t.Fatalf("addons not sorted by id: %q after %q", a.ID, out.Addons[i-1].ID)
		}
	}
	if !found {
		t.Fatal("registry response missing the ingress-nginx entry")
	}
}

func TestRegistryEndpointReadAuth(t *testing.T) {
	s := newTestServer(t, newFakeStore(), func(c *Config) { c.ReadToken = "read-tok" })
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	if resp := getJSON(t, ts, "/api/v1/registry", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", resp.StatusCode)
	}
	var out registryTestResponse
	if resp := getJSON(t, ts, "/api/v1/registry", "read-tok", &out); resp.StatusCode != http.StatusOK {
		t.Fatalf("with token: status = %d, want 200", resp.StatusCode)
	}
	if len(out.Addons) == 0 {
		t.Fatal("addons is empty, want the embedded registry entries")
	}
}
