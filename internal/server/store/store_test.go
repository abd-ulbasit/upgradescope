package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestErrNotFoundIsSentinel(t *testing.T) {
	if ErrNotFound == nil {
		t.Fatal("ErrNotFound is nil")
	}
	wrapped := fmt.Errorf("get cluster 42: %w", ErrNotFound)
	if !errors.Is(wrapped, ErrNotFound) {
		t.Fatal("wrapped ErrNotFound is not matched by errors.Is")
	}
}

func TestBlobFieldsExcludedFromJSON(t *testing.T) {
	snap, err := json.Marshal(Snapshot{ID: 1, ClusterID: 2, Hash: "abc", Inventory: []byte(`{"secret":1}`)})
	if err != nil {
		t.Fatalf("marshal Snapshot: %v", err)
	}
	if strings.Contains(string(snap), "inventory") || strings.Contains(string(snap), "secret") {
		t.Errorf("Snapshot JSON leaks inventory blob: %s", snap)
	}
	if !strings.Contains(string(snap), `"hash":"abc"`) {
		t.Errorf("Snapshot JSON missing hash: %s", snap)
	}

	eval, err := json.Marshal(Evaluation{ID: 1, Target: "1.36", Report: []byte(`{"big":2}`)})
	if err != nil {
		t.Fatalf("marshal Evaluation: %v", err)
	}
	if strings.Contains(string(eval), "report") || strings.Contains(string(eval), "big") {
		t.Errorf("Evaluation JSON leaks report blob: %s", eval)
	}
	if !strings.Contains(string(eval), `"target":"1.36"`) {
		t.Errorf("Evaluation JSON missing target: %s", eval)
	}

	cl, err := json.Marshal(Cluster{ClusterUID: "u-1"})
	if err != nil {
		t.Fatalf("marshal Cluster: %v", err)
	}
	if !strings.Contains(string(cl), `"clusterUid":"u-1"`) {
		t.Errorf("Cluster JSON tag wrong: %s", cl)
	}
}

func TestTimeFormatFixedWidthUTC(t *testing.T) {
	pkt := time.FixedZone("PKT", 5*3600)
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{"whole second", time.Date(2026, 6, 10, 12, 0, 5, 0, time.UTC), "2026-06-10T12:00:05.000000000Z"},
		{"sub-second", time.Date(2026, 6, 10, 12, 0, 5, 500000000, time.UTC), "2026-06-10T12:00:05.500000000Z"},
		{"non-UTC normalized to Z", time.Date(2026, 6, 10, 17, 0, 5, 0, pkt), "2026-06-10T12:00:05.000000000Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatTime(tt.in); got != tt.want {
				t.Errorf("formatTime(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	// The reason for fixed width: with RFC3339Nano, "…05Z" sorts AFTER
	// "…05.5Z" as a string although it is the earlier instant.
	whole := formatTime(tests[0].in) // 12:00:05.000000000Z
	half := formatTime(tests[1].in)  // 12:00:05.500000000Z
	if !(whole < half) {
		t.Errorf("string order broken: %q must sort before %q", whole, half)
	}
}

func TestParseStoredTimeRoundTrip(t *testing.T) {
	in := time.Date(2026, 6, 10, 12, 0, 5, 123456789, time.FixedZone("X", -7*3600))
	got, err := parseStoredTime(formatTime(in))
	if err != nil {
		t.Fatalf("parseStoredTime: %v", err)
	}
	if !got.Equal(in) {
		t.Errorf("round trip lost the instant: got %v, want %v", got, in)
	}
	if got.Location() != time.UTC {
		t.Errorf("round trip returned location %v, want UTC", got.Location())
	}
	if _, err := parseStoredTime("not-a-time"); err == nil {
		t.Error("parseStoredTime accepted garbage")
	}
}
