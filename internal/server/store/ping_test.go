package store

import (
	"context"
	"path/filepath"
	"testing"
)

// The server's /readyz pings the store; a closed database must fail it.
func TestSQLitePing(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ping.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on an open store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(context.Background()); err == nil {
		t.Fatal("Ping on a closed store: want an error")
	}
}
