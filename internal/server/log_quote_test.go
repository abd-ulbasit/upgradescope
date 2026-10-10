package server

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/server/store"
)

// A cluster name is chosen by whoever holds an ingest token. The outbox's
// log lines quote it with %q, so a name holding a newline or an escape
// sequence cannot forge a log line or drive the terminal of whoever reads
// the log, on a retry, a give-up after the last attempt or a give-up of a
// message past its lifetime.
func TestOutboxLogsQuoteClusterNames(t *testing.T) {
	logged := captureLog(t)
	s := newTestServer(t, newFakeStore(), func(c *Config) {
		c.Notifier = &rawURLNotifier{url: "https://hooks.example.com/x"}
	})
	name := "prod\nserver: FAKE line\x1b[2J\r\u202e"
	payload, err := json.Marshal(map[string]string{
		"Cluster": name, "Target": "1.35", "Kind": "became-ready", "Title": "ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []store.OutboxMessage{
		{ID: 1, Attempts: 1, CreatedAt: s.now()},                                // retried
		{ID: 2, Attempts: outboxMaxAttempts, CreatedAt: s.now()},                // last attempt
		{ID: 3, Attempts: 1, CreatedAt: s.now().Add(-outboxMaxAge - time.Hour)}, // past its lifetime
	} {
		// Claimed with a lease that outlasts the attempt, as ClaimOutbox
		// leaves it, so deliver attempts it rather than putting it back.
		m.NextAttemptAt = s.now().Add(s.outboxLease)
		m.Sink, m.Payload = s.sinks[0].name, payload
		s.deliver(context.Background(), m)
	}
	text := logged.String()
	if strings.ContainsAny(text, "\x1b\r\u202e") || strings.Contains(text, "\nserver: FAKE") {
		t.Errorf("the log carries the cluster name raw:\n%q", text)
	}
	quoted := strconv.Quote(name)
	for _, want := range []string{"retrying at", "after", "queued"} {
		found := false
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, want) && strings.Contains(line, "(cluster "+quoted+", sink ") {
				found = true
			}
		}
		if !found {
			t.Errorf("no %q line names the cluster as %s:\n%s", want, quoted, text)
		}
	}
}

// Every log line of the server that names a cluster by its name quotes it
// (outboxFor's encoding error among them, which no test can make fail):
// no log.Printf format in the package says "cluster %s" or "cluster %v".
func TestServerLogLinesQuoteClusterNames(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	unquoted := regexp.MustCompile(`cluster %[sv]`)
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "log" || !strings.HasSuffix(sel.Sel.Name, "f") {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			checked++
			if format, _ := strconv.Unquote(lit.Value); unquoted.MatchString(format) {
				t.Errorf("%s: log format %s prints a cluster name unquoted: use %%q", fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("found no log.Printf calls: the check no longer looks at the server's code")
	}
}
