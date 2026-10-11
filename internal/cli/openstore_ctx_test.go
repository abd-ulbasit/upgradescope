package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

type openStoreCtxKey struct{}

// Every command that opens a Postgres database opens it under the
// command's own context (serve: the signal context derived from it), so
// cancelling the command, as SIGTERM cancels serve, abandons a long
// migration instead of the open running on context.Background or a
// fixed deadline (#364).
func TestOpenStorePassesCommandContext(t *testing.T) {
	errStub := errors.New("stub postgres open")
	var got []context.Context
	orig := openPostgresStore
	openPostgresStore = func(ctx context.Context, dsn string) (store.Store, error) {
		got = append(got, ctx)
		return nil, errStub
	}
	t.Cleanup(func() { openPostgresStore = orig })

	const url = "postgres://u:p@db.invalid:5432/x"
	for _, tc := range []struct {
		name string
		cmd  func() *cobra.Command
		args []string
	}{
		{"tokens create", newTokensCmd, []string{"create", "prod", "--db-url", url}},
		{"tokens create --read", newTokensCmd, []string{"create", "--read", "--teams", "*", "--db-url", url}},
		{"tokens list", newTokensCmd, []string{"list", "--db-url", url}},
		{"tokens revoke", newTokensCmd, []string{"revoke", "prod", "--all", "--db-url", url}},
		{"clusters list", newClustersCmd, []string{"list", "--db-url", url}},
		{"serve", newServeCmd, []string{"--db-url", url}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("UPGRADESCOPE_DB_URL", "")
			got = nil
			cmd := tc.cmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(tc.args)
			ctx := context.WithValue(t.Context(), openStoreCtxKey{}, tc.name)
			err := cmd.ExecuteContext(ctx)
			if !errors.Is(err, errStub) {
				t.Fatalf("err = %v, want the stub's open error", err)
			}
			if !strings.Contains(err.Error(), "open postgres store") {
				t.Errorf("err = %q, want it to name the postgres open", err)
			}
			if len(got) != 1 {
				t.Fatalf("OpenPostgres called %d times, want 1", len(got))
			}
			if v := got[0].Value(openStoreCtxKey{}); v != tc.name {
				t.Errorf("open ctx carries %v, want the command's context (%q)", v, tc.name)
			}
		})
	}
}
