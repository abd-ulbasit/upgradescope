package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runMainEnv makes the test binary run main() instead of the tests, so a
// test can see what the real entry point writes to stderr.
const runMainEnv = "UPGRADESCOPE_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		// The arguments after "--" are the command line main() sees.
		for i, a := range os.Args {
			if a == "--" {
				os.Args = append([]string{"upgradescope"}, os.Args[i+1:]...)
				break
			}
		}
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// main prints a failing command's error through cli.ErrorText (SE-21): a
// control character in the error is shown as an escape on stderr, never as
// itself. pflag puts an unknown flag's name in its error unquoted, so the
// raw error holds the ESC; what main prints must not.
func TestMainPrintsErrorsEscaped(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$", "--", "scan", "--bad\x1b[2J\rflag")
	cmd.Env = append(os.Environ(), runMainEnv+"=1", "KUBECONFIG="+os.DevNull)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("want exit 1, got %v (stderr %q)", err, stderr.String())
	}
	got := stderr.String()
	if strings.ContainsAny(got, "\x1b\r") {
		t.Errorf("stderr carries a raw control character: %q", got)
	}
	if !strings.Contains(got, `--bad\x1b[2J\rflag`) {
		t.Errorf("stderr does not show the flag's name as escapes: %q", got)
	}
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Errorf("want one line on stderr, got %q", got)
	}
}
