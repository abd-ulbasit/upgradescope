package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/mcp"
)

// marker stands for what an inventory_file may hold that must not reach
// the assistant: the path comes from an assistant, so the file can be any
// file it was steered to, shaped as an inventory around a secret.
const marker = "S3CRET-MARKER"

// writeEditedInventory writes the mixed-everything inventory, edited, to a
// temporary file and returns its path.
func writeEditedInventory(t *testing.T, edit func(map[string]any)) string {
	t.Helper()
	raw, err := os.ReadFile(mixedInventory)
	if err != nil {
		t.Fatal(err)
	}
	var inv map[string]any
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatal(err)
	}
	edit(inv)
	out, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "inventory.json")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMCPInventoryFileIsJudgedAsIngestJudgesIt: an inventory_file is
// checked as a server checks a push (inventory.Admit): an invalid
// identifier, a serverVersion that is not 1.x and a value over a limit are
// refused, and a capability reason over its limit is cut and judged, as a
// default server judges it. A refusal names the field and the rule and
// quotes nothing of the file: no value, no map key.
func TestMCPInventoryFileIsJudgedAsIngestJudgesIt(t *testing.T) {
	over := marker + strings.Repeat("x", inventory.MaxStringBytes)
	refused := []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{"a string over the limit", func(inv map[string]any) {
			inv["unrecognizedImages"] = []any{over}
		}, "inventory.unrecognizedImages[0]: 16397 bytes, over the 16384-byte limit"},
		{"a map key over the limit", func(inv map[string]any) {
			inv["capabilities"].(map[string]any)[over] = map[string]any{"available": true}
		}, "inventory.capabilities (a key): 16397 bytes, over the 16384-byte limit"},
		{"a namespace key that is not a namespace", func(inv map[string]any) {
			inv["apiUsage"].([]any)[0].(map[string]any)["namespaces"] = map[string]any{marker: 2}
		}, "inventory.apiUsage[0].namespaces is not a namespace name: an RFC 1123 label"},
		{"a node name that is not one", func(inv map[string]any) {
			inv["nodes"] = []any{map[string]any{"name": marker + " node"}}
		}, "inventory.nodes[0].name is not an RFC 1123 subdomain"},
		{"a serverVersion that is not a version", func(inv map[string]any) {
			inv["serverVersion"] = marker
		}, "inventory.serverVersion is not a Kubernetes 1.x version"},
		{"a serverVersion that is not 1.x", func(inv map[string]any) {
			inv["serverVersion"] = "v2.0.0"
		}, "inventory.serverVersion is not a Kubernetes 1.x version"},
		// schemaVersion is already known to be right by the time Admit
		// speaks, so the refusal must not say it is wrong; the integer is
		// safe to quote.
		{"a later collectorSchema", func(inv map[string]any) {
			inv["collectorSchema"] = inventory.CurrentCollectorSchema + 1
		}, fmt.Sprintf("inventory.collectorSchema %d is not one this build knows", inventory.CurrentCollectorSchema+1)},
		{"a negative collectorSchema", func(inv map[string]any) {
			inv["collectorSchema"] = -3
		}, "inventory.collectorSchema -3 is not one this build knows"},
	}
	cs := startMCP(t)
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			path := writeEditedInventory(t, tc.edit)
			for _, tool := range []string{mcp.ToolGetReport, mcp.ToolListFindings} {
				res := callMCPWithin(t, cs, 30*time.Second, tool, map[string]any{"inventory_file": path, "target": "1.38"})
				msg := mcpText(res)
				if !res.IsError || !strings.Contains(msg, tc.want) || !strings.Contains(msg, path) {
					t.Errorf("%s: isError=%v %.300q, want a tool error naming the file and %q", tool, res.IsError, msg, tc.want)
				}
				if strings.Contains(msg, "schemaVersion") {
					t.Errorf("%s blames schemaVersion, which was right: %.300q", tool, msg)
				}
				if strings.Contains(msg, marker) || strings.Contains(msg, "xxxx") {
					t.Errorf("%s quotes the file: %.300q", tool, msg)
				}
			}
		})
	}

	// A reason over its limit, as an older agent pushed it (~320 forbidden
	// CRD lists), is cut and judged: a default server takes it.
	t.Run("a capability reason over its limit", func(t *testing.T) {
		path := writeEditedInventory(t, func(inv map[string]any) {
			inv["capabilities"].(map[string]any)["helm"] = map[string]any{
				"available": false, "reason": strings.Repeat("secrets is forbidden; ", 70<<10/22),
			}
		})
		res := callMCPWithin(t, cs, 30*time.Second, mcp.ToolGetReport, map[string]any{"inventory_file": path, "target": "1.38"})
		if res.IsError {
			t.Fatalf("a 70 KiB reason was refused: %.300q", mcpText(res))
		}
		// Cut at ingest's limit, then to what a result carries of the
		// cluster's free text.
		raw, _ := json.Marshal(res.StructuredContent)
		if !strings.Contains(string(raw), "…(cut by upgradescope mcp)") || strings.Contains(string(raw), strings.Repeat("secrets is forbidden; ", 2<<10/22+1)) {
			t.Errorf("the report does not carry the reason cut")
		}
	})
}
