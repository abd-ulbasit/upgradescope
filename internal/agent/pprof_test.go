package agent

import (
	"strings"
	"testing"
)

// --pprof-addr serves the Go profiler, which exposes the process's memory
// and goroutines: only on a loopback address, never on the health listener's
// all-interfaces default (#247).
func TestValidatePprofAddr(t *testing.T) {
	for _, ok := range []string{"", "127.0.0.1:6060", "localhost:6060", "[::1]:6060", "127.0.0.1:0", "127.1.2.3:7"} {
		if err := ValidatePprofAddr(ok); err != nil {
			t.Errorf("ValidatePprofAddr(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		":6060", "0.0.0.0:6060", "[::]:6060", "10.0.0.1:6060", "example.com:6060",
		"127.0.0.1", "6060", "localhost", "127.0.0.1:http-alt-nope", "127.0.0.1:99999", "127.0.0.1:-1",
	} {
		if err := ValidatePprofAddr(bad); err == nil {
			t.Errorf("ValidatePprofAddr(%q) = nil, want an error", bad)
		}
	}
	if err := ValidatePprofAddr(":6060"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error for :6060 = %v, want it to say loopback", err)
	}
}

// With PprofAddr set the agent serves /debug/pprof on it; the health
// listener does not.
func TestRunServesPprofOnlyOnItsOwnLoopbackListener(t *testing.T) {
	cfg := Config{PprofAddr: "127.0.0.1:0"}
	logs, health, _ := startRun(t, cfg)
	start := linesWithMsg(logs.lines(t), msgStarting)
	addr, _ := start[0]["pprofAddr"].(string)
	if addr == "" {
		t.Fatalf("startup line has no pprofAddr: %v", start[0])
	}
	resp, body := get(t, "http://"+addr+"/debug/pprof/")
	if resp.StatusCode != 200 || !strings.Contains(body, "goroutine") {
		t.Errorf("GET /debug/pprof/ = %d %.80q, want 200 and the profile index", resp.StatusCode, body)
	}
	resp, body = get(t, "http://"+addr+"/debug/pprof/cmdline")
	if resp.StatusCode != 200 {
		t.Errorf("GET /debug/pprof/cmdline = %d %.80q, want 200", resp.StatusCode, body)
	}
	if resp, _ = get(t, health+"/debug/pprof/"); resp.StatusCode != 404 {
		t.Errorf("health listener GET /debug/pprof/ = %d, want 404: the profiler is not on it", resp.StatusCode)
	}
}

// Off by default: no listener, and the startup line says so.
func TestRunServesNoPprofByDefault(t *testing.T) {
	logs, _, _ := startRun(t, Config{})
	start := linesWithMsg(logs.lines(t), msgStarting)
	if addr, _ := start[0]["pprofAddr"].(string); addr != "" {
		t.Errorf("pprofAddr = %q, want empty", addr)
	}
}
