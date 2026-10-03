package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// `tokens create` refuses a cluster name the server would refuse pushes
// under (#37), before it creates the database.
func TestTokensCreateRefusesInvalidClusterNames(t *testing.T) {
	for _, name := range []string{"../<script>x", strings.Repeat("a", 254), "Prod", "prod eu"} {
		db := filepath.Join(t.TempDir(), "tokens.db")
		stdout, _, err := execTokens(t, "create", name, "--db", db)
		if err == nil || !strings.Contains(err.Error(), "RFC 1123 subdomain") || stdout != "" {
			t.Fatalf("tokens create %.40q: err %v, stdout %q; want an error naming the rule and no token", name, err, stdout)
		}
		if _, statErr := os.Stat(db); statErr == nil {
			t.Fatalf("tokens create %.40q created %s", name, db)
		}
	}
}

// The agent checks --cluster-name and --team-label at startup, before
// any cluster access: the server would refuse every push under an
// invalid name, and an invalid label key names no label.
func TestAgentRefusesInvalidNames(t *testing.T) {
	for _, args := range [][]string{
		{"--cluster-name", "../<script>x"},
		{"--cluster-name", strings.Repeat("a", 254)},
		{"--cluster-name", "Prod_EU"},
		{"--team-label", "team label"},
		{"--team-label", "a/b/c"},
	} {
		if _, err := execAgent(t, args...); err == nil || !strings.Contains(err.Error(), "invalid "+args[0]) {
			t.Errorf("agent %v: err %v, want an invalid %s error", args, err, args[0])
		}
	}
	for _, args := range [][]string{
		{"--cluster-name", "prod-eu-1"},
		{"--team-label", "example.com/team"},
		{}, // the cluster UID, and the label "team"
	} {
		if _, err := execAgent(t, args...); err != nil {
			t.Errorf("agent %v: %v", args, err)
		}
	}
}

// `clusters rename --db` is the way off a name a v0.1 server registered
// and pushes are now refused under ("Prod_EU"): it renames such a cluster
// to a valid name, and refuses an invalid new name, as the server's
// PATCH does.
func TestClustersRenameTakesOnlyValidNewNames(t *testing.T) {
	db := filepath.Join(t.TempDir(), "clusters.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.UpsertCluster(ctx, store.Cluster{Name: "Prod_EU", ClusterUID: "uid-1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := execClusters(t, "rename", "Prod_EU", "Prod-EU-1", "--db", db); err == nil || !strings.Contains(err.Error(), "RFC 1123 subdomain") {
		t.Fatalf("rename to Prod-EU-1: err %v, want the rule", err)
	}
	if _, err := execClusters(t, "rename", "Prod_EU", "prod-eu", "--db", db); err != nil {
		t.Fatalf("rename Prod_EU to prod-eu: %v", err)
	}
	st, err = store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.ClusterByName(ctx, "prod-eu"); err != nil {
		t.Errorf("prod-eu after the rename: %v", err)
	}
}
