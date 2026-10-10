package cli

import (
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/upgradescope/internal/textsafe"
)

// version is stamped via -ldflags "-X …/internal/cli.version=…" by release
// builds (GoReleaser, the Dockerfile). Builds that skip the stamp — notably
// `go install …@vX` — fall back to the Go build info, so --version, SARIF and
// agent snapshots name a real version instead of "dev". It must stay a
// constant initializer: -X has no effect on a variable initialized by a call.
var version = "dev"

func init() { version = resolveVersion(version, debug.ReadBuildInfo) }

// resolveVersion returns stamped unless it is the "dev" placeholder;
// otherwise the main module version recorded by the go command (set for
// `go install …@vX` and, since Go 1.24, for builds inside a tagged git
// checkout) without its leading "v", matching GoReleaser's {{ .Version }}
// stamp; then the VCS revision ("dev+<12 hex>[.dirty]"), then "dev".
func resolveVersion(stamped string, readBuildInfo func() (*debug.BuildInfo, bool)) string {
	if stamped != "" && stamped != "dev" {
		return stamped
	}
	info, ok := readBuildInfo()
	if !ok || info == nil {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return strings.TrimPrefix(v, "v")
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		return "dev+" + rev + ".dirty"
	}
	return "dev+" + rev
}

// registerVersionTemplate makes --version print what `upgradescope version`
// does. The text is computed when --version is used, not when the command
// tree is built, so other commands never pay for loading the KB.
var registerVersionTemplate = sync.OnceFunc(func() {
	cobra.AddTemplateFunc("upgradescopeVersion", func() string {
		var b strings.Builder
		writeVersionText(&b, currentVersionInfo())
		return b.String()
	})
})

// unknownCommandError is the refusal of an argument that names no
// subcommand of the root: the command as the user typed it, and the
// commands it resembles. It is typed so that ErrorText can print cobra's
// layout (the message, a blank line, the suggestions, one per line) with
// real newlines while still escaping the typed name, which is
// attacker-influenced text (#245): the generic escaping of every other error
// would turn the layout's own newlines into literal \n and \t (#338).
type unknownCommandError struct {
	name, path  string
	suggestions []string
}

// Error is cobra's own text, with the name quoted and so escaped.
func (e *unknownCommandError) Error() string {
	s := fmt.Sprintf("unknown command %q for %q", e.name, e.path)
	if len(e.suggestions) > 0 {
		s += "\n\nDid you mean this?\n"
		for _, sug := range e.suggestions {
			s += fmt.Sprintf("\t%v\n", sug)
		}
	}
	return s
}

// text is the message as ErrorText prints it: the layout of Error with real
// newlines, and every user-supplied part escaped.
func (e *unknownCommandError) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "unknown command \"%s\" for \"%s\"", textsafe.Escape(e.name), textsafe.Escape(e.path))
	if len(e.suggestions) > 0 {
		b.WriteString("\n\nDid you mean this?")
		for _, sug := range e.suggestions {
			b.WriteString("\n\t" + textsafe.Escape(sug))
		}
	}
	return b.String()
}

// Execute runs root and returns its error. The root stays non-runnable, so
// cobra finds an unknown command before it parses any flag and a mistyped
// subcommand followed by that subcommand's flags ("scann --target 1.37") is
// still refused as an unknown command, not as an unknown flag of the root.
// That refusal is cobra's untyped error; Execute turns it into the typed
// unknownCommandError, so that ErrorText can print its layout (#338).
func Execute(root *cobra.Command) error {
	cmd, err := root.ExecuteC()
	if err != nil && cmd == root {
		if typed := asUnknownCommand(root, err); typed != nil {
			return typed
		}
	}
	return err
}

// asUnknownCommand reads the typed name out of cobra's "unknown command %q
// for %q" refusal of the root and rebuilds it with the suggestions as data.
// It returns nil for any other error.
func asUnknownCommand(root *cobra.Command, err error) *unknownCommandError {
	const prefix = "unknown command "
	rest, ok := strings.CutPrefix(err.Error(), prefix)
	if !ok {
		return nil
	}
	quoted, qerr := strconv.QuotedPrefix(rest)
	if qerr != nil {
		return nil
	}
	name, qerr := strconv.Unquote(quoted)
	if qerr != nil {
		return nil
	}
	var suggestions []string
	if !root.DisableSuggestions {
		suggestions = root.SuggestionsFor(name)
	}
	return &unknownCommandError{name: name, path: root.CommandPath(), suggestions: suggestions}
}

func Root() *cobra.Command {
	registerVersionTemplate()
	root := &cobra.Command{
		Use:   "upgradescope",
		Short: "Continuous Kubernetes upgrade-readiness scanner",
		Long: `upgradescope finds what blocks a Kubernetes upgrade before you start it:
deprecated and removed APIs (stored objects and live callers), end-of-life
add-ons, version skew and chart compatibility, rolled up into a readiness
score and verdict.

Run it once with 'scan', continuously in the cluster with 'agent', and across
a fleet with 'serve'.`,
		Example: `  # Is the current kubeconfig context's cluster ready for Kubernetes 1.37?
  upgradescope scan --target 1.37

  # Gate a pull request on rendered manifests
  helm template ./chart --output-dir rendered
  upgradescope scan --files rendered --target 1.37 --output sarif > upgradescope.sarif`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate(`{{ upgradescopeVersion }}`)
	// Usage is silenced (an error prints one line, not the whole usage), so
	// a mistyped flag says where the usage is. Subcommands inherit it.
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return fmt.Errorf("%w (run '%s --help' for usage)", err, c.CommandPath())
	})
	root.AddCommand(newScanCmd())
	root.AddCommand(newAgentCmd())
	root.AddCommand(newServeCmd())
	root.AddCommand(newTokensCmd())
	root.AddCommand(newClustersCmd())
	root.AddCommand(newMCPCmd())
	root.AddCommand(newVersionCmd())
	return root
}
