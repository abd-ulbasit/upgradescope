package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

func execClusters(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	cmd := newClustersCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), err
}

// TestClustersDeleteFreesTheName is the resolution the server's 409 points
// at for a rebuilt cluster: deleting the old record lets the new cluster
// UID register under the same name.
func TestClustersDeleteFreesTheName(t *testing.T) {
	db := filepath.Join(t.TempDir(), "clusters.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.UpsertCluster(ctx, store.Cluster{Name: "prod", ClusterUID: "uid-old"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := execClusters(t, "delete", "prod", "--db", db)
	if err != nil {
		t.Fatalf("clusters delete: %v", err)
	}
	if !strings.Contains(out, `"prod"`) {
		t.Errorf("stdout = %q, want it to name the deleted cluster", out)
	}

	st, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.UpsertCluster(ctx, store.Cluster{Name: "prod", ClusterUID: "uid-new"}); err != nil {
		t.Fatalf("register rebuilt cluster after delete: %v", err)
	}

	_, err = execClusters(t, "delete", "never-existed", "--db", db)
	if err == nil || !strings.Contains(err.Error(), `no cluster "never-existed"`) {
		t.Fatalf("delete unknown: err = %v, want a not-found message", err)
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Errorf("error should be the CLI message, not the raw store sentinel")
	}
}

// TestClustersNeedAnExplicitTarget: without --server or a database flag,
// list/delete/rename used to open (and create) ./upgradescope.db and, for
// list, print an empty fleet. They now refuse and create nothing.
func TestClustersNeedAnExplicitTarget(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("UPGRADESCOPE_DB_URL", "")
	for _, args := range [][]string{{"list"}, {"delete", "prod"}, {"rename", "prod", "prod-eu"}} {
		_, err := execClusters(t, args...)
		if err == nil || !strings.Contains(err.Error(), "--server") || !strings.Contains(err.Error(), "--db") {
			t.Errorf("clusters %v: err = %v, want a refusal naming --server and --db", args, err)
		}
	}
	if _, err := os.Stat("upgradescope.db"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("./upgradescope.db exists (stat err %v), want nothing created", err)
	}
}
