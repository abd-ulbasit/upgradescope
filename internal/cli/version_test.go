package cli

import (
	"bytes"
	"encoding/json"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

func TestResolveBuild(t *testing.T) {
	info := func(settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) { return &debug.BuildInfo{Settings: settings}, true }
	}
	rev := debug.BuildSetting{Key: "vcs.revision", Value: "4bca610f1e2d3c4b5a69788796a5b4c3d2e1f0a9"}
	at := debug.BuildSetting{Key: "vcs.time", Value: "2026-10-01T12:00:00Z"}

	tests := []struct {
		name               string
		commit, date       string
		read               func() (*debug.BuildInfo, bool)
		wantCommit, wantAt string
	}{
		{"ldflags stamps win", "abc123", "2026-09-30T08:00:00Z", info(rev, at), "abc123", "2026-09-30T08:00:00Z"},
		// A build inside a git checkout (go build, go install ./cmd/...).
		{"vcs build info", "", "", info(rev, at), "4bca610f1e2d3c4b5a69788796a5b4c3d2e1f0a9", "2026-10-01T12:00:00Z"},
		// go install …@vX builds from the module zip: no VCS stamp.
		{"no vcs info", "", "", info(), "unknown", "unknown"},
		{"no build info", "", "", func() (*debug.BuildInfo, bool) { return nil, false }, "unknown", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, d := resolveBuild(tt.commit, tt.date, tt.read)
			if c != tt.wantCommit || d != tt.wantAt {
				t.Errorf("resolveBuild = (%q, %q), want (%q, %q)", c, d, tt.wantCommit, tt.wantAt)
			}
		})
	}
}

func runRoot(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := Root()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errb.String(), err
}

func TestVersionCommand(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := runRoot(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	for _, want := range []string{
		"upgradescope " + version + "\n",
		"commit:",
		"built:",
		"go:            " + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH + "\n",
		"kb:            " + k.Version + "\n",
		"kb horizon:    Kubernetes " + k.MaxKnownK8s.String() + "\n",
		"registry date:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("version output lacks %q:\n%s", want, out)
		}
	}

	// --version prints the same block as the subcommand.
	flag, _, err := runRoot(t, "--version")
	if err != nil {
		t.Fatalf("--version: %v", err)
	}
	if flag != out {
		t.Errorf("--version output differs from `version`:\n--version:\n%s\nversion:\n%s", flag, out)
	}
}

func TestVersionJSON(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := runRoot(t, "version", "--output", "json")
	if err != nil {
		t.Fatalf("version --output json: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not a JSON object of strings: %v\n%s", err, out)
	}
	want := map[string]string{
		"version":   version,
		"goVersion": runtime.Version(),
		"platform":  runtime.GOOS + "/" + runtime.GOARCH,
		"kbVersion": k.Version,
		"kbHorizon": k.MaxKnownK8s.String(),
	}
	for key, w := range want {
		if got[key] != w {
			t.Errorf("%s = %q, want %q", key, got[key], w)
		}
	}
	for _, key := range []string{"commit", "buildDate", "registryDate"} {
		if got[key] == "" {
			t.Errorf("%s is empty (want a value or \"unknown\")", key)
		}
	}
}

func TestVersionBadOutput(t *testing.T) {
	_, _, err := runRoot(t, "version", "--output", "yaml")
	if err == nil || !strings.Contains(err.Error(), `invalid --output "yaml" (want text or json)`) {
		t.Fatalf("err = %v, want the invalid --output message", err)
	}
}
