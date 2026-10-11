package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unreachablePG is a Postgres URL that fails fast: a command that reaches
// the Postgres path errors with "postgres" in it, one that wrongly falls
// back to SQLite succeeds (and leaves ./upgradescope.db behind).
const unreachablePG = "postgres://127.0.0.1:1/nope?connect_timeout=1"

// noStrayDB fails if the working directory holds the default SQLite file,
// the symptom of a command that ignored $UPGRADESCOPE_DB_URL (#382).
func noStrayDB(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("upgradescope.db"); err == nil {
		t.Errorf("./upgradescope.db was created: the command fell back to the default SQLite store")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// `tokens create --read` honours $UPGRADESCOPE_DB_URL like the ingest path
// does (#382): the by-value dbFlags copy resolved into the original and
// opened the empty copy, so it minted into ./upgradescope.db.
func TestTokensCreateReadHonoursDBURLEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("UPGRADESCOPE_DB_URL", unreachablePG)
	stdout, _, err := execTokens(t, "create", "--read", "--teams", "web")
	if err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("create --read with $UPGRADESCOPE_DB_URL: err = %v, want a postgres open error", err)
	}
	if stdout != "" {
		t.Errorf("a token was printed (%q) although no store took it", stdout)
	}
	noStrayDB(t)
}

func TestTokensCreateReadHonoursDBURLFile(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("UPGRADESCOPE_DB_URL", "")
	f := filepath.Join(t.TempDir(), "db-url")
	if err := os.WriteFile(f, []byte(unreachablePG+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := execTokens(t, "create", "--read", "--teams", "web", "--db-url-file", f)
	if err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("create --read --db-url-file: err = %v, want a postgres open error", err)
	}
	if stdout != "" {
		t.Errorf("a token was printed (%q) although no store took it", stdout)
	}
	noStrayDB(t)
}

// Every way to name the database reaches the right backend for the read
// subcommands: the environment and --db-url(-file) go to Postgres, an
// explicit --db wins over the environment, and with nothing set the SQLite
// default stays.
func TestTokensReadFlagsMatrix(t *testing.T) {
	urlFile := filepath.Join(t.TempDir(), "db-url")
	if err := os.WriteFile(urlFile, []byte(unreachablePG), 0o600); err != nil {
		t.Fatal(err)
	}
	subs := map[string][]string{
		"create": {"create", "--read", "--teams", "web"},
		"list":   {"list", "--read"},
		"revoke": {"revoke", "--read", "--id", "1"},
	}
	with := func(sub []string, extra ...string) []string {
		return append(append([]string{}, sub...), extra...)
	}
	for name, sub := range subs {
		t.Run(name+"/env", func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("UPGRADESCOPE_DB_URL", unreachablePG)
			if _, _, err := execTokens(t, sub...); err == nil || !strings.Contains(err.Error(), "postgres") {
				t.Fatalf("err = %v, want a postgres open error", err)
			}
			noStrayDB(t)
		})
		t.Run(name+"/db-url", func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("UPGRADESCOPE_DB_URL", "")
			if _, _, err := execTokens(t, with(sub, "--db-url", unreachablePG)...); err == nil || !strings.Contains(err.Error(), "postgres") {
				t.Fatalf("err = %v, want a postgres open error", err)
			}
			noStrayDB(t)
		})
		t.Run(name+"/db-url-file", func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("UPGRADESCOPE_DB_URL", "")
			if _, _, err := execTokens(t, with(sub, "--db-url-file", urlFile)...); err == nil || !strings.Contains(err.Error(), "postgres") {
				t.Fatalf("err = %v, want a postgres open error", err)
			}
			noStrayDB(t)
		})
		t.Run(name+"/explicit-db-wins-over-env", func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("UPGRADESCOPE_DB_URL", unreachablePG)
			db := filepath.Join(t.TempDir(), "t.db")
			// revoke of a missing id is "no active read token": still a
			// SQLite answer, not a postgres error.
			_, _, err := execTokens(t, with(sub, "--db", db)...)
			if err != nil && strings.Contains(err.Error(), "postgres") {
				t.Fatalf("explicit --db must win over $UPGRADESCOPE_DB_URL: %v", err)
			}
			if _, statErr := os.Stat(db); statErr != nil {
				t.Errorf("--db file not used: %v", statErr)
			}
			noStrayDB(t)
		})
		t.Run(name+"/default-sqlite", func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("UPGRADESCOPE_DB_URL", "")
			_, _, err := execTokens(t, sub...)
			if err != nil && strings.Contains(err.Error(), "postgres") {
				t.Fatalf("with nothing set the SQLite default stays: %v", err)
			}
			if _, statErr := os.Stat("upgradescope.db"); statErr != nil {
				t.Errorf("default ./upgradescope.db not used: %v", statErr)
			}
		})
	}
}

// No function in internal/cli takes dbFlags by value: its secretFlag points
// into the original, so a copy resolves into one struct and opens the
// other (#382). Methods with a value receiver only read and are fine.
func TestNoDBFlagsPassedByValue(t *testing.T) {
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ft, ok := n.(*ast.FuncType)
			if !ok || ft.Params == nil {
				return true
			}
			for _, p := range ft.Params.List {
				checked++
				if id, ok := p.Type.(*ast.Ident); ok && id.Name == "dbFlags" {
					t.Errorf("%s: dbFlags passed by value; take *dbFlags", fset.Position(p.Pos()))
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("scanned no parameters: the check proves nothing")
	}
}
