package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
