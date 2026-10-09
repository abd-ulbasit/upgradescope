package server

import (
	"strings"
	"testing"
)

// One bearer that both pushes and reads is refused for library callers
// too, not only by serve's flags (#250).
func TestNewRefusesReadTokenEqualToIngestToken(t *testing.T) {
	_, err := New(Config{Store: newFakeStore(), ReadToken: "same", IngestToken: "same"})
	if err == nil || !strings.Contains(err.Error(), "ReadToken") {
		t.Errorf("New with equal read and ingest tokens: err = %v, want a refusal", err)
	}
	if _, err := New(Config{Store: newFakeStore(), ReadToken: "r", IngestToken: "i"}); err != nil {
		t.Errorf("distinct tokens: %v", err)
	}
}
