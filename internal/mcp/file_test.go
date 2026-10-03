package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestAReadThatBlocksGivesUpWhenTheCallEnds: a regular file whose read
// blocks (one on a stalled NFS or FUSE mount; on Linux, /proc/kmsg) has no
// deadline the platform honours, so the read gives up when the call's
// context ends instead of holding the call, and its read slot, until it
// unblocks.
func TestAReadThatBlocksGivesUpWhenTheCallEnds(t *testing.T) {
	unblock := make(chan struct{})
	defer close(unblock)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := readUnder(ctx, "/mnt/stalled/report.json", func() ([]byte, error) {
			<-unblock
			return nil, nil
		})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "/mnt/stalled/report.json") {
			t.Errorf("a blocked read: %v, want the path and the call's deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked read held the call after its context ended")
	}
}

func TestReadFileReadsARegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := ReadFile(context.Background(), path, 10)
	if err != nil || string(raw) != `{}` {
		t.Fatalf("ReadFile: %q, %v", raw, err)
	}
	if _, err := ReadFile(context.Background(), path, 1); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("ReadFile over the bound: %v", err)
	}
}

// TestInventoryIsJudgedUnderTheCallsContext: the inventory_file source gets
// the call's context, so its read gives up with the call too.
func TestInventoryIsJudgedUnderTheCallsContext(t *testing.T) {
	stopped := make(chan error, 1)
	cfg := localConfig()
	cfg.Inventory = func(ctx context.Context, _, _ string) (json.RawMessage, error) {
		<-ctx.Done()
		stopped <- ctx.Err()
		return nil, ctx.Err()
	}
	cs := connect(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: ToolGetReport, Arguments: map[string]any{"inventory_file": "/mnt/stalled/inv.json", "target": "1.37"}}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the call: %v, want its deadline", err)
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the inventory read stopped with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the inventory read went on after the client cancelled the call")
	}
}
