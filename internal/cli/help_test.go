package cli

import (
	"strings"
	"testing"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// walk calls f for cmd and every command below it, the ones cobra adds
// (help, completion) included.
func walk(cmd *cobra.Command, f func(*cobra.Command)) {
	f(cmd)
	for _, c := range cmd.Commands() {
		walk(c, f)
	}
}

// The --help of every command reads the same way: a Short that is one
// capitalized line without a closing period, an Example for every command
// a user runs, and a description for every flag. Man pages and shell
// completions are generated from the same text (tools/gen-docs).
func TestHelpTextConventions(t *testing.T) {
	root := Root()
	root.InitDefaultCompletionCmd()
	walk(root, func(c *cobra.Command) {
		path := c.CommandPath()
		if strings.HasPrefix(path, "upgradescope completion") || strings.HasPrefix(path, "upgradescope help") {
			return // cobra's own wording
		}
		s := c.Short
		switch {
		case s == "":
			t.Errorf("%s: no Short", path)
		case !unicode.IsUpper([]rune(s)[0]):
			t.Errorf("%s: Short %q does not start with a capital", path, s)
		case strings.HasSuffix(s, "."):
			t.Errorf("%s: Short %q ends with a period", path, s)
		case strings.Contains(s, "\n") || len(s) > 80:
			t.Errorf("%s: Short %q is not one line of at most 80 characters", path, s)
		}
		if c.Runnable() && c.Example == "" {
			t.Errorf("%s: no Example", path)
		}
		if c.Runnable() && !c.SilenceUsage {
			t.Errorf("%s: SilenceUsage is false: an error would print the whole usage instead of the error", path)
		}
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Name == "help" {
				return
			}
			if f.Usage == "" {
				t.Errorf("%s --%s: no usage text", path, f.Name)
			}
		})
	})
}

// A mistyped flag says how to get help, on stderr (main prints the error).
func TestFlagErrorPointsToHelp(t *testing.T) {
	_, _, err := runRoot(t, "scan", "--taget", "1.37")
	if err == nil {
		t.Fatal("unknown flag accepted")
	}
	if want := "unknown flag: --taget (run 'upgradescope scan --help' for usage)"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}
