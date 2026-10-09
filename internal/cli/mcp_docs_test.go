package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abd-ulbasit/upgradescope/internal/mcp"
)

const mcpPage = "docs/getting-started/mcp.md"

// TestDocsMCPPageListsExactlyTheTools: the tools table of the MCP page names
// every tool the server exposes (fleet mode included) and no other, so the
// documented set is the served set.
func TestDocsMCPPageListsExactlyTheTools(t *testing.T) {
	page := readDoc(t, mcpPage)
	section := page[strings.Index(page, "\n## Tools"):]
	section = section[:strings.Index(section[1:], "\n## ")+1]
	var documented []string
	for _, m := range regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|").FindAllStringSubmatch(section, -1) {
		documented = append(documented, m[1])
	}
	slices.Sort(documented)
	if want := mcp.ToolNames(true); !slices.Equal(documented, want) {
		t.Errorf("%s documents tools %v, the server exposes %v", mcpPage, documented, want)
	}
}

// TestDocsMCPExamplesParse: the mcpServers JSON of the MCP page is valid and
// its arguments are flags `upgradescope mcp` accepts, and so is every
// `upgradescope mcp ...` command the page and the README show, including the
// `claude mcp add` line that wraps one.
func TestDocsMCPExamplesParse(t *testing.T) {
	parses := func(t *testing.T, where string, args []string) {
		t.Helper()
		cmd, rest, err := Root().Find(args)
		if err == nil {
			err = cmd.ParseFlags(rest)
		}
		if err == nil {
			err = cmd.ValidateArgs(cmd.Flags().Args())
		}
		if err == nil && cmd.Name() != "mcp" {
			err = errors.New("not the mcp command")
		}
		if err != nil {
			t.Errorf("%s: `upgradescope %s` does not parse: %v", where, strings.Join(args, " "), err)
		}
	}

	servers := 0
	for _, m := range regexp.MustCompile("(?s)```json\n(.*?)```").FindAllStringSubmatch(readDoc(t, mcpPage), -1) {
		var cfg struct {
			MCPServers map[string]struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(m[1]), &cfg); err != nil {
			t.Errorf("%s: a json example is not valid JSON: %v\n%s", mcpPage, err, m[1])
			continue
		}
		for name, s := range cfg.MCPServers {
			servers++
			if name != "upgradescope" || s.Command != "upgradescope" {
				t.Errorf("%s: server %q runs %q, want upgradescope's own", mcpPage, name, s.Command)
			}
			parses(t, mcpPage, s.Args)
		}
	}
	if servers < 3 {
		t.Errorf("%s has %d mcpServers examples, want the Claude Code, Claude Desktop and fleet ones", mcpPage, servers)
	}

	cmdRE := regexp.MustCompile("\\bupgradescope (mcp(?: --[^`\\n#\\\\|]*)?)")
	seen := 0
	for _, page := range []string{mcpPage, "README.md"} {
		for _, m := range cmdRE.FindAllStringSubmatch(readDoc(t, page), -1) {
			seen++
			parses(t, page, strings.Fields(m[1]))
		}
	}
	if seen < 3 {
		t.Errorf("found %d `upgradescope mcp` commands in the docs; the pattern is stale", seen)
	}
}

// TestDocsMCPPageSaysWhatTheClusterChooses: the MCP page names the report
// fields whose text the cluster chooses and who chooses it, and says how
// the server marks and cuts it, with the marker, _meta key and cap the
// server uses.
func TestDocsMCPPageSaysWhatTheClusterChooses(t *testing.T) {
	page := strings.Join(strings.Fields(readDoc(t, mcpPage)), " ")
	for _, want := range []string{
		"`objects[].ignore` and `objects[].ignoreReason`",
		"`upgradescope.dev/ignore-reason`",
		"listed even when they suppress nothing",
		"`suppressed[].reason`",
		"`notAssessed[].reason`",
		"whoever can create or annotate an object there chooses",
		"**" + mcp.ClusterTextMarker + "**",
		"`" + mcp.MetaClusterText + "`",
		fmt.Sprintf("longer than %d KiB is cut, ending in `%s`", mcp.MaxClusterTextBytes>>10, strings.TrimSpace(mcp.ClusterTextCutMark)),
		// The rule is inverted: not a list of fields that hold such text,
		// but every string, but for the few the tool writes itself.
		"not a list of fields that hold such text",
		"every string in every tool's result, values and object keys alike",
		"except numbers, booleans, the report's own field names, and the values of the few keys upgradescope writes itself",
		"A field added to the report later is outside text until it is listed there",
		"Every result of every tool (`scan`, `get_report`, `list_findings`, `registry_lookup` and `fleet_summary`)",
		"never through a team's name or another key the document chose",
		"A tool error is outside text whole",
		// The output contract: two text blocks, the notice first.
		"has two text blocks: a notice about the cluster's text in it (see [below](#what-to-keep-in-mind)), then the JSON",
		"A tool error has two as well, the notice and then the error",
		"read the JSON from `structuredContent`, or from the second text block, never the first",
		"does not fall back to the pod's service account",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("%s does not say %q", mcpPage, want)
		}
	}
	// The keys it lists as the tool's own are the ones the server treats so.
	var listed []string
	for _, k := range mcp.ToolWords() {
		listed = append(listed, "`"+k+"`")
	}
	if want := strings.Join(listed, ", "); !strings.Contains(page, want) {
		t.Errorf("%s does not list the keys the tool writes itself, %s", mcpPage, want)
	}
}

// TestDocsMCPPageStatesTheHTTPLimits: the limits the MCP page states for
// --http are the ones the server applies.
func TestDocsMCPPageStatesTheHTTPLimits(t *testing.T) {
	page := strings.Join(strings.Fields(readDoc(t, mcpPage)), " ")
	l := mcpHTTPLimits
	for _, want := range []string{
		fmt.Sprintf("headers must arrive within %d seconds", int(l.readHeaderTimeout.Seconds())),
		fmt.Sprintf("whole request within %d seconds", int(l.readTimeout.Seconds())),
		fmt.Sprintf("at most %d KiB (`431` past that)", l.maxHeaderBytes>>10),
		fmt.Sprintf("body at most %d MiB (`413`)", mcpsdk.DefaultMaxRequestBodyBytes>>20),
		fmt.Sprintf("idle for %d seconds is closed", int(l.idleTimeout.Seconds())),
		fmt.Sprintf("no request for %d minutes is closed", int(l.sessions.SessionTimeout.Minutes())),
		fmt.Sprintf("at most %d sessions are open at once", l.sessions.MaxSessions),
		"refused with `503`",
		"connection is closed without reading the body",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("%s does not say %q", mcpPage, want)
		}
	}
}
