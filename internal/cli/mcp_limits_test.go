package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abd-ulbasit/upgradescope/internal/mcp"
)

func callMCPWithin(t *testing.T, cs *mcpsdk.ClientSession, d time.Duration, tool string, args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s %v: %v (want a tool error within %s)", tool, args, err, d)
	}
	return res
}

// TestMCPInventoryFileIsBoundedAndARegularFile: an inventory_file larger
// than a default server takes from an agent is refused before it is read,
// a directory is refused, and a file that is not JSON is refused without
// quoting it.
func TestMCPInventoryFileIsBoundedAndARegularFile(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.json")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxInventoryFileBytes + 1); err != nil { // sparse
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	passwd := filepath.Join(dir, "passwd")
	if err := os.WriteFile(passwd, []byte("# s3cret\nroot:x:0:0::/root:/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cs := startMCP(t)
	for _, tc := range []struct {
		name, path, want string
	}{
		{"too large", big, "larger than 20 MiB"},
		{"a directory", dir, "is not a regular file (a directory)"},
		{"not JSON", passwd, "is not an upgradescope inventory"},
	} {
		res := callMCPWithin(t, cs, 5*time.Second, mcp.ToolListFindings, map[string]any{"inventory_file": tc.path, "target": "1.38"})
		if !res.IsError || !strings.Contains(mcpText(res), tc.want) || strings.Contains(mcpText(res), "s3cret") {
			t.Errorf("%s: isError=%v %q, want %q and nothing of the file", tc.name, res.IsError, mcpText(res), tc.want)
		}
	}
}

// TestRunMCPScanStopsWhenTheCallEnds: the cluster read of a scan call ends
// with the call's context, not when its own five minutes are up, and a scan
// cut short reports nothing. The API server here accepts each request and
// never answers.
func TestRunMCPScanStopsWhenTheCallEnds(t *testing.T) {
	hung := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-hung:
		}
	}))
	defer api.Close()
	defer close(hung)
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kc, []byte(strings.Replace(testKubeconfig, "https://127.0.0.1:6443", api.URL, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := scanOptions{target: "1.38", kubeconfig: kc, kubecontext: "test", output: "json", failOn: "never"}
	if err := validateScanOptions(&opts); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		reports, err := realRunMCPScan(ctx, []scanOptions{opts})
		if err == nil && len(reports) > 0 {
			t.Errorf("a scan cut short reported %d reports", len(reports))
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a scan whose call ended returned no error")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the scan went on after its call ended")
	}
}
