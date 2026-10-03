package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
		_, err := readUnder(ctx, make(chan struct{}, 1), "/mnt/stalled/report.json", func() ([]byte, error) {
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

// TestCancelledReadsThatBlockAreBoundedInNumber: a call cancelled during a
// read that blocks returns at once, but its read goes on in the background
// and keeps its slot until it returns, so calls cancelled one after another
// against a stalled mount cannot pile up reads (each up to the file bound)
// beyond the slots. Once the reads return, the slots are free again.
func TestCancelledReadsThatBlockAreBoundedInNumber(t *testing.T) {
	const slots = 2
	sem := make(chan struct{}, slots)
	var started, most atomic.Int32
	release := make(chan struct{})
	blocked := func() ([]byte, error) {
		n := started.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		<-release
		return nil, nil
	}
	for i := range slots + 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		begun := time.Now()
		_, err := readUnder(ctx, sem, "/mnt/stalled/report.json", blocked)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "/mnt/stalled/report.json") {
			t.Errorf("call %d: %v, want the path and the call's deadline", i, err)
		}
		if d := time.Since(begun); d > 5*time.Second {
			t.Fatalf("call %d held the caller %s after it was cancelled", i, d)
		}
	}
	if got := started.Load(); got != slots {
		t.Errorf("%d reads started for %d cancelled calls, want %d (the slots)", got, slots+3, slots)
	}
	close(release)
	// The background reads return and give their slots back.
	for deadline := time.Now().Add(5 * time.Second); len(sem) != 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d slots still held 5s after every read was released", len(sem))
		}
	}
	for range slots + 1 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		raw, err := readUnder(ctx, sem, "r.json", func() ([]byte, error) { return []byte("{}"), nil })
		cancel()
		if err != nil || string(raw) != "{}" {
			t.Fatalf("a read after the blocked ones returned: %q, %v", raw, err)
		}
	}
	if got := len(sem); got != 0 {
		t.Errorf("%d slots held after reads that returned", got)
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

// TestReadFileIsBoundedWhateverTheFileSays: the read itself stops one byte
// past the bound, so a file that grows after it was stat'ed and opened (or
// one that reports a size it does not have) is refused as too large, not
// read whole.
func TestReadFileIsBoundedWhateverTheFileSays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const max = 1 << 10
	afterOpenChecks = func() {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		if _, err := f.Write(make([]byte, 4*max)); err != nil {
			t.Error(err)
		}
	}
	defer func() { afterOpenChecks = func() {} }()
	if raw, err := ReadFile(context.Background(), path, max); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("a file that grew after the checks: %d bytes, %v", len(raw), err)
	}
}

// TestReadFileReadsAFileThatReportsNoSize: a file whose stat says 0 bytes
// (on Linux, those under /proc) is read through the same bound: within it,
// read whole; over it, refused.
func TestReadFileReadsAFileThatReportsNoSize(t *testing.T) {
	const path = "/proc/self/status"
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != 0 {
		t.Skipf("no regular file reporting 0 bytes at %s here", path)
	}
	raw, err := ReadFile(context.Background(), path, 1<<20)
	if err != nil || len(raw) == 0 {
		t.Fatalf("%s within the bound: %d bytes, %v", path, len(raw), err)
	}
	if _, err := ReadFile(context.Background(), path, 8); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("%s over the bound: %v", path, err)
	}
}
