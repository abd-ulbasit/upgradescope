package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abd-ulbasit/upgradescope/registry"
)

// maxReportFileBytes bounds a report or inventory file the tools read: a
// file path comes from the assistant.
const maxReportFileBytes = 64 << 20

const (
	defaultLimit = 50
	maxLimit     = 500
)

var minorPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

// Input schemas are written out, not inferred, so an assistant sees the
// enums and bounds, and the contract is readable in one place.

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func targetProp(desc string) map[string]any {
	return map[string]any{"type": "string", "pattern": minorPattern.String(), "description": desc}
}

func objectSchema(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// sourceProps are the properties that choose which report a tool reads.
func sourceProps() map[string]any {
	return map[string]any{
		"report_file":    strProp("Path of a JSON report written by 'upgradescope scan --output json' (or --write-baseline) to read instead of the latest scan."),
		"inventory_file": strProp("Path of an inventory JSON file (what an agent pushes) to judge at target, instead of the latest scan."),
		"cluster":        strProp("Fleet mode only: a cluster's name (or numeric id) on the upgradescope server."),
		"target":         targetProp("Target Kubernetes minor, e.g. 1.37. Required with inventory_file. With the latest scan, report_file or a cluster it picks the report when there are several (a cluster defaults to its next minor)."),
	}
}

func scanInputSchema() map[string]any {
	return objectSchema([]string{"targets"}, map[string]any{
		"targets": map[string]any{
			"type": "array", "minItems": 1, "maxItems": maxScanTargets, "uniqueItems": true,
			"items":       targetProp("A target Kubernetes minor, e.g. 1.37."),
			"description": fmt.Sprintf("Target minors to judge the cluster against, at most %d. Each is a full scan.", maxScanTargets),
		},
		"kubeconfig": strProp("Path of a kubeconfig. Default: what the server was started with (--kubeconfig, else $KUBECONFIG, else ~/.kube/config)."),
		"context":    strProp("Kubeconfig context to scan. Default: what the server was started with (--context, else the kubeconfig's current context)."),
		"files":      strProp("Scan rendered manifests in this file or directory (*.yaml, *.yml, *.json) instead of a cluster; cannot be combined with kubeconfig or context."),
	})
}

func listFindingsInputSchema() map[string]any {
	props := sourceProps()
	props["severity"] = map[string]any{"enum": []any{"blocker", "warning", "info"}, "description": "Only findings of this severity."}
	props["category"] = strProp("Only findings of this category, e.g. eol-addon, removed-api, deprecated-api-in-use, version-skew, chart-incompat.")
	props["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": maxLimit, "description": fmt.Sprintf("Most findings to return, default %d; the result says when more match.", defaultLimit)}
	return objectSchema(nil, props)
}

func getReportInputSchema() map[string]any { return objectSchema(nil, sourceProps()) }

func registryInputSchema() map[string]any {
	return objectSchema([]string{"query"}, map[string]any{
		"query": map[string]any{"type": "string", "minLength": 1, "description": "Case-insensitive text to find in an add-on's id, name, Helm chart, container image or node runtime, e.g. ingress-nginx, cert-manager, containerd."},
		"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": maxLimit, "description": fmt.Sprintf("Most add-ons to return, default %d.", defaultLimit)},
	})
}

func fleetInputSchema() map[string]any {
	return objectSchema(nil, map[string]any{
		"targets": map[string]any{
			"type": "array", "maxItems": 16, "items": targetProp("A target Kubernetes minor."),
			"description": "Targets to show a score for. Default: each cluster's next minor plus the server's configured targets.",
		},
	})
}

type scanInput struct {
	Targets    []string `json:"targets"`
	Kubeconfig string   `json:"kubeconfig"`
	Context    string   `json:"context"`
	Files      string   `json:"files"`
}

type sourceInput struct {
	ReportFile    string `json:"report_file"`
	InventoryFile string `json:"inventory_file"`
	Cluster       string `json:"cluster"`
	Target        string `json:"target"`
}

type listFindingsInput struct {
	ReportFile    string `json:"report_file"`
	InventoryFile string `json:"inventory_file"`
	Cluster       string `json:"cluster"`
	Target        string `json:"target"`
	Severity      string `json:"severity"`
	Category      string `json:"category"`
	Limit         int    `json:"limit"`
}

type registryInput struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type fleetInput struct {
	Targets []string `json:"targets"`
}

func (s *server) scan(ctx context.Context, _ *mcpsdk.CallToolRequest, in scanInput) (*mcpsdk.CallToolResult, json.RawMessage, error) {
	if len(in.Targets) == 0 || len(in.Targets) > maxScanTargets {
		return nil, nil, fmt.Errorf("targets: want 1 to %d target minors, got %d", maxScanTargets, len(in.Targets))
	}
	seen := map[string]bool{}
	for _, t := range in.Targets {
		if !minorPattern.MatchString(t) {
			return nil, nil, fmt.Errorf("target %q: want a Kubernetes minor such as 1.37", t)
		}
		if seen[t] {
			return nil, nil, fmt.Errorf("target %q is listed twice", t)
		}
		seen[t] = true
	}
	if in.Files != "" && (in.Kubeconfig != "" || in.Context != "") {
		return nil, nil, errors.New("files scans rendered manifests and cannot be combined with kubeconfig or context")
	}

	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	reports := make([]json.RawMessage, 0, len(in.Targets))
	byTarget := make(map[string]json.RawMessage, len(in.Targets))
	for _, t := range in.Targets {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		doc, err := s.cfg.Scan(ctx, ScanRequest{Target: t, Kubeconfig: in.Kubeconfig, Context: in.Context, Files: in.Files})
		if err != nil {
			return nil, nil, fmt.Errorf("scan --target %s: %w", t, err)
		}
		reports = append(reports, doc)
		byTarget[t] = doc
	}
	s.mu.Lock()
	s.lastScan = byTarget
	s.mu.Unlock()
	out, err := json.Marshal(struct {
		Reports []json.RawMessage `json:"reports"`
	}{reports})
	return nil, out, err
}

func (s *server) getReport(ctx context.Context, _ *mcpsdk.CallToolRequest, in sourceInput) (*mcpsdk.CallToolResult, json.RawMessage, error) {
	doc, _, err := s.report(ctx, in)
	return nil, doc, err
}

func (s *server) listFindings(ctx context.Context, _ *mcpsdk.CallToolRequest, in listFindingsInput) (*mcpsdk.CallToolResult, json.RawMessage, error) {
	switch in.Severity {
	case "", "blocker", "warning", "info":
	default:
		return nil, nil, fmt.Errorf("severity %q: want blocker, warning or info", in.Severity)
	}
	limit := in.Limit
	switch {
	case limit == 0:
		limit = defaultLimit
	case limit < 0 || limit > maxLimit:
		return nil, nil, fmt.Errorf("limit %d: want 1 to %d", limit, maxLimit)
	}
	doc, cluster, err := s.report(ctx, sourceInput{in.ReportFile, in.InventoryFile, in.Cluster, in.Target})
	if err != nil {
		return nil, nil, err
	}
	var rep struct {
		Target    string            `json:"target"`
		KBVersion string            `json:"kbVersion"`
		Findings  []json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal(doc, &rep); err != nil {
		return nil, nil, fmt.Errorf("decoding the report: %w", err)
	}
	var matched []json.RawMessage
	for _, raw := range rep.Findings {
		var f struct {
			Severity string `json:"severity"`
			Category string `json:"category"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, nil, fmt.Errorf("decoding a finding: %w", err)
		}
		if (in.Severity == "" || f.Severity == in.Severity) && (in.Category == "" || f.Category == in.Category) {
			matched = append(matched, raw)
		}
	}
	total := len(matched)
	if total > limit {
		matched = matched[:limit]
	}
	if matched == nil {
		matched = []json.RawMessage{}
	}
	out, err := json.Marshal(struct {
		Target    string            `json:"target"`
		KBVersion string            `json:"kbVersion,omitempty"`
		Cluster   string            `json:"cluster,omitempty"`
		Findings  []json.RawMessage `json:"findings"`
		Total     int               `json:"total"`
		Truncated bool              `json:"truncated"`
	}{rep.Target, rep.KBVersion, cluster, matched, total, total > limit})
	return nil, out, err
}

// report resolves which report a tool reads and returns it with the
// cluster's name when it came from the fleet server. Exactly one source is
// named, or none, for the latest scan.
func (s *server) report(ctx context.Context, in sourceInput) (json.RawMessage, string, error) {
	named := 0
	for _, v := range []string{in.ReportFile, in.InventoryFile, in.Cluster} {
		if v != "" {
			named++
		}
	}
	if named > 1 {
		return nil, "", errors.New("name one source: report_file, inventory_file or cluster")
	}
	if in.Target != "" && !minorPattern.MatchString(in.Target) {
		return nil, "", fmt.Errorf("target %q: want a Kubernetes minor such as 1.37", in.Target)
	}
	switch {
	case in.ReportFile != "":
		doc, err := readReportFile(in.ReportFile)
		if err != nil {
			return nil, "", err
		}
		if t := reportTarget(doc); in.Target != "" && t != in.Target {
			return nil, "", fmt.Errorf("%s is a report for target %s, not %s", in.ReportFile, t, in.Target)
		}
		return doc, "", nil
	case in.InventoryFile != "":
		if in.Target == "" {
			return nil, "", errors.New("inventory_file needs target, the minor to judge it at")
		}
		doc, err := s.cfg.Inventory(in.InventoryFile, in.Target)
		return doc, "", err
	case in.Cluster != "":
		if s.cfg.Fleet == nil {
			return nil, "", errors.New("cluster needs fleet mode: start the server with --server-url (and a read token if the server requires one)")
		}
		return s.cfg.Fleet.Report(ctx, in.Cluster, in.Target)
	}
	return s.latestScan(in.Target)
}

func (s *server) latestScan(target string) (json.RawMessage, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lastScan) == 0 {
		if s.cfg.Fleet != nil {
			return nil, "", errNoSourceFleet()
		}
		return nil, "", errNoSource
	}
	if target != "" {
		doc, ok := s.lastScan[target]
		if !ok {
			return nil, "", fmt.Errorf("the latest scan did not judge target %s (it judged %s)", target, strings.Join(s.scanTargets(), ", "))
		}
		return doc, "", nil
	}
	if len(s.lastScan) > 1 {
		return nil, "", fmt.Errorf("the latest scan judged several targets (%s): pass target", strings.Join(s.scanTargets(), ", "))
	}
	for _, doc := range s.lastScan {
		return doc, "", nil
	}
	panic("unreachable")
}

// scanTargets are the latest scan's targets, sorted. Callers hold s.mu.
func (s *server) scanTargets() []string {
	ts := make([]string, 0, len(s.lastScan))
	for t := range s.lastScan {
		ts = append(ts, t)
	}
	slices.Sort(ts)
	return ts
}

func reportTarget(doc json.RawMessage) string {
	var r struct {
		Target string `json:"target"`
	}
	_ = json.Unmarshal(doc, &r)
	return r.Target
}

// readReportFile reads a JSON report and refuses a file that is not one,
// by its schemaVersion and findings, with a reason in place of the schema
// validator's output error.
func readReportFile(path string) (json.RawMessage, error) {
	raw, err := readCapped(path)
	if err != nil {
		return nil, err
	}
	var probe struct {
		SchemaVersion *int              `json:"schemaVersion"`
		Findings      []json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("%s is not a JSON report: %w", path, err)
	}
	if probe.SchemaVersion == nil || *probe.SchemaVersion != 1 {
		return nil, fmt.Errorf("%s is not an upgradescope JSON report of schemaVersion 1 (write one with 'upgradescope scan --output json')", path)
	}
	return json.RawMessage(bytes.TrimSpace(raw)), nil
}

func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxReportFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxReportFileBytes {
		return nil, fmt.Errorf("%s is larger than %d MiB", path, maxReportFileBytes>>20)
	}
	return raw, nil
}

func (s *server) registryLookup(_ context.Context, _ *mcpsdk.CallToolRequest, in registryInput) (*mcpsdk.CallToolResult, json.RawMessage, error) {
	q := strings.ToLower(strings.TrimSpace(in.Query))
	if q == "" {
		return nil, nil, errors.New("query is empty")
	}
	limit := in.Limit
	switch {
	case limit == 0:
		limit = defaultLimit
	case limit < 0 || limit > maxLimit:
		return nil, nil, fmt.Errorf("limit %d: want 1 to %d", limit, maxLimit)
	}
	addons, err := s.cfg.Registry()
	if err != nil {
		return nil, nil, fmt.Errorf("loading the embedded registry: %w", err)
	}
	matched := []registry.AddOn{}
	for _, a := range addons {
		if addOnMatches(a, q) {
			matched = append(matched, a)
		}
	}
	total := len(matched)
	if total > limit {
		matched = matched[:limit]
	}
	out, err := json.Marshal(struct {
		AddOns    []registry.AddOn `json:"addons"`
		Total     int              `json:"total"`
		Truncated bool             `json:"truncated"`
	}{matched, total, total > limit})
	return nil, out, err
}

// addOnMatches reports whether the lower-case query q is part of the add-on's
// id, name or any of its matchers.
func addOnMatches(a registry.AddOn, q string) bool {
	for _, field := range slices.Concat([]string{a.ID, a.DisplayName}, a.Matchers.Charts, a.Matchers.Images, a.Matchers.Runtimes) {
		if strings.Contains(strings.ToLower(field), q) {
			return true
		}
	}
	return false
}

func (s *server) fleetSummary(ctx context.Context, _ *mcpsdk.CallToolRequest, in fleetInput) (*mcpsdk.CallToolResult, json.RawMessage, error) {
	for _, t := range in.Targets {
		if !minorPattern.MatchString(t) {
			return nil, nil, fmt.Errorf("target %q: want a Kubernetes minor such as 1.37", t)
		}
	}
	doc, err := s.cfg.Fleet.Summary(ctx, in.Targets)
	return nil, doc, err
}
