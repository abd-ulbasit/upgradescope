//go:build unix

package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestReportFileRefusesWhatIsNotAFile: a path an assistant names can be a
// FIFO (a plain open blocks until a writer comes, so the call would hang
// and leak a goroutine), a device that never ends, or /dev/stdin (on the
// stdio transport, the client's own JSON-RPC frames). Each is a tool error,
// and it comes back at once.
func TestReportFileRefusesWhatIsNotAFile(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{
		"a FIFO":             fifo,
		"a character device": "/dev/zero",
		"a directory":        t.TempDir(),
	}
	if fi, err := os.Stat("/dev/stdin"); err == nil && !fi.Mode().IsRegular() {
		paths["/dev/stdin"] = "/dev/stdin"
	}
	cs := connect(t, localConfig())
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			for _, tool := range []string{ToolGetReport, ToolListFindings} {
				res := callWithin(t, cs, 5*time.Second, tool, map[string]any{"report_file": path})
				if !res.IsError || !strings.Contains(text(res), "is not a regular file") {
					t.Errorf("%s: isError=%v %q", tool, res.IsError, text(res))
				}
			}
		})
	}
}

// TestReadFileDoesNotBlockOnAFIFOSwappedIn: the open itself does not wait
// for a writer, so a FIFO that appears between the stat and the open is
// refused too, not waited on.
func TestReadFileDoesNotBlockOnAFIFOSwappedIn(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_RDONLY|openNonblock, 0)
		if err == nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("open: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO with no writer blocked")
	}
}
