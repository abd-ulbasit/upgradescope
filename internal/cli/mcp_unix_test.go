//go:build unix

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abd-ulbasit/upgradescope/internal/mcp"
)

// TestMCPHelperProcess is `upgradescope mcp` on this process's real stdin
// and stdout, for TestMCPStdioDoesNotReadItsOwnStdin; it is not a test.
func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("UPGRADESCOPE_TEST_MCP_HELPER") != "1" {
		t.Skip("the stdio server TestMCPStdioDoesNotReadItsOwnStdin starts")
	}
	cmd := Root()
	cmd.SetArgs([]string{"mcp"})
	cmd.SetIn(os.Stdin)
	cmd.SetOut(os.Stdout)
	cmd.SetErr(os.Stderr)
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// TestMCPStdioDoesNotReadItsOwnStdin: on the stdio transport, /dev/stdin is
// the client's stream of JSON-RPC frames. A report_file or inventory_file
// naming it, or a FIFO, is a prompt tool error, and the session goes on;
// reading it would take the client's next frames.
func TestMCPStdioDoesNotReadItsOwnStdin(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMCPHelperProcess$")
	cmd.Env = append(os.Environ(), "UPGRADESCOPE_TEST_MCP_HELPER=1", "KUBECONFIG="+filepath.Join(t.TempDir(), "none"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(ctx, &mcpsdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	for _, args := range []map[string]any{
		{"report_file": "/dev/stdin"},
		{"inventory_file": "/dev/stdin", "target": "1.38"},
		{"report_file": fifo},
		{"inventory_file": fifo, "target": "1.38"},
	} {
		res := callMCPWithin(t, cs, 10*time.Second, mcp.ToolGetReport, args)
		if !res.IsError || !strings.Contains(mcpText(res), "is not a regular file") {
			t.Errorf("get_report %v: isError=%v %q", args, res.IsError, mcpText(res))
		}
	}
	res := callMCPWithin(t, cs, 10*time.Second, mcp.ToolRegistryLookup, map[string]any{"query": "ingress-nginx"})
	if res.IsError {
		t.Errorf("the session did not go on: %q", mcpText(res))
	}
}
