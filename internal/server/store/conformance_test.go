package store_test

import (
	"path/filepath"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
	"github.com/abd-ulbasit/upgradescope/internal/server/store/storetest"
)

func TestSQLiteConformance(t *testing.T) {
	storetest.RunStoreConformance(t, func(t *testing.T) store.Store {
		s, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
		if err != nil {
			t.Fatalf("open sqlite store: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
