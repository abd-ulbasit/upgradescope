package cli

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

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
