// Package mcp is upgradescope's Model Context Protocol server: read-only
// tools that let an AI assistant ask for a cluster's upgrade readiness,
// its findings, the add-on registry and, against a fleet server, the fleet.
//
// It is the protocol layer only. What a scan does and how an inventory is
// judged stay in the CLI's code, which the command hands in through Config,
// so a report an assistant gets is the one `scan --output json` writes.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abd-ulbasit/upgradescope/registry"
)

// The tools. Every one is read-only; TestToolsAreReadOnly holds the set and
// the hint to that, and docs/getting-started/mcp.md documents it.
const (
	ToolScan           = "scan"
	ToolListFindings   = "list_findings"
	ToolGetReport      = "get_report"
	ToolRegistryLookup = "registry_lookup"
	ToolFleetSummary   = "fleet_summary"
)

// ToolNames are the tools a server exposes, sorted; fleet_summary only in
// fleet mode, where a server is configured to ask.
func ToolNames(fleet bool) []string {
	names := []string{ToolGetReport, ToolListFindings, ToolRegistryLookup, ToolScan}
	if fleet {
		names = append(names, ToolFleetSummary)
	}
	slices.Sort(names)
	return names
}

// maxScanTargets bounds one scan call: the cluster is read once, and judged
// at each target, each a report in the result.
const maxScanTargets = 4

// ScanRequest is one call of the scan tool. It names the targets and
// nothing else: which cluster is read (and with which credentials) is the
// command's --kubeconfig and --context and the environment, never an
// assistant's choice.
type ScanRequest struct {
	Targets []string // distinct Kubernetes minors, in the order asked
}

// Config wires the server to what the CLI already does.
type Config struct {
	// Version is the build version, for the server's implementation info.
	Version string
	// Scan reads the cluster once and judges it at each target of req, as
	// `upgradescope scan --output json` would: one report document per
	// target, in req's order. It stops when ctx ends: the client cancelled
	// the call (notifications/cancelled), or went away (on stdio, closed
	// its end; over HTTP, closed the connection that carried the call,
	// which NewHTTPHandler turns into a cancel). Required.
	Scan func(ctx context.Context, req ScanRequest) ([]json.RawMessage, error)
	// Inventory judges an inventory file (the JSON an agent pushes) at a
	// target and returns the report document, giving up when ctx ends.
	// Required.
	Inventory func(ctx context.Context, path, target string) (json.RawMessage, error)
	// Fleet, when set, is the serve instance get_report and list_findings
	// read from (with cluster) and fleet_summary summarises.
	Fleet *Fleet
	// Registry returns the add-on registry; nil means the embedded one.
	Registry func() ([]registry.AddOn, error)
}

// maxConcurrentReads is how many get_report, list_findings and
// fleet_summary calls run at once. Each holds its report several times
// over (MaxReportBytes says how), and a client can issue calls in parallel,
// which prompt-injected content can ask for; the bound keeps the memory
// they take to that of two calls however many are issued. A call waits for
// a slot, or gives up when the client cancels it.
const maxConcurrentReads = 2

type server struct {
	cfg Config

	// scanSlot holds a token while a scan runs: one at a time, since each
	// reads the whole cluster. A channel, not a mutex, so a waiting call
	// gives up when its context ends.
	scanSlot chan struct{}

	// readSlots holds a token while a tool reads and returns a report or
	// the fleet summary (maxConcurrentReads says why).
	readSlots chan struct{}

	mu       sync.Mutex
	lastScan map[string]json.RawMessage // target → report of the latest scan call
}

// New returns the MCP server for cfg.
func New(cfg Config) *mcpsdk.Server {
	if cfg.Registry == nil {
		cfg.Registry = sync.OnceValues(registry.Load)
	}
	s := &server{cfg: cfg, scanSlot: make(chan struct{}, 1), readSlots: make(chan struct{}, maxConcurrentReads)}
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "upgradescope",
		Title:   "upgradescope",
		Version: cfg.Version,
	}, &mcpsdk.ServerOptions{Instructions: instructions(cfg.Fleet != nil)})

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:         ToolScan,
		Title:        "Scan a cluster for upgrade readiness",
		Description:  "Scan the Kubernetes cluster this server was started against (its --kubeconfig and --context, else the environment's) for what blocks an upgrade to each target minor: removed and deprecated APIs, end-of-life add-ons, version skew, chart compatibility. Read-only. Returns one report per target and keeps them for list_findings and get_report. A scan reads the whole cluster and can take minutes.",
		InputSchema:  scanInputSchema(),
		OutputSchema: ScanOutputSchema(),
		Annotations:  readOnly("Scan a cluster", true),
	}, s.scan)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:         ToolListFindings,
		Title:        "List findings, filtered",
		Description:  "List the findings of a readiness report, filtered by severity and category. The report is the latest scan, a report_file written by 'scan --output json', an inventory_file judged at a target, or, in fleet mode, a cluster on the server.",
		InputSchema:  listFindingsInputSchema(),
		OutputSchema: FindingsOutputSchema(),
		Annotations:  readOnly("List findings", cfg.Fleet != nil),
	}, s.listFindings)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:         ToolGetReport,
		Title:        "Get a readiness report",
		Description:  "Get the full readiness report: score, verdict, findings, what was not assessed. The report is the latest scan, a report_file, an inventory_file judged at a target, or, in fleet mode, a cluster on the server.",
		InputSchema:  getReportInputSchema(),
		OutputSchema: ReportOutputSchema(),
		Annotations:  readOnly("Get a report", cfg.Fleet != nil),
	}, s.getReport)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:         ToolRegistryLookup,
		Title:        "Look up an add-on in the registry",
		Description:  "Look up an add-on's end-of-life dates, release lines and Kubernetes compatibility in the registry compiled into this binary, by name, id, Helm chart, image or node runtime.",
		InputSchema:  registryInputSchema(),
		OutputSchema: registryOutputSchema(),
		Annotations:  readOnly("Look up an add-on", false),
	}, s.registryLookup)
	if cfg.Fleet != nil {
		mcpsdk.AddTool(srv, &mcpsdk.Tool{
			Name:         ToolFleetSummary,
			Title:        "Summarise the fleet",
			Description:  "Summarise the fleet from the upgradescope server: one row per cluster with its readiness score for each upgrade target.",
			InputSchema:  fleetInputSchema(),
			OutputSchema: fleetOutputSchema(),
			Annotations:  readOnly("Summarise the fleet", true),
		}, s.fleetSummary)
	}
	// The slot is held around the whole call, the SDK's check and encoding
	// of the result included, which is where most of a report's copies are.
	// markErrors is the outermost of New's middleware, so it marks every
	// tool error, the SDK's own argument checks' included. (Over HTTP,
	// NewHTTPHandler adds cancelWithRequest after, which wraps it; that
	// one returns errCallerGone, a JSON-RPC error and not a tool result,
	// so there is nothing for markErrors to mark.)
	srv.AddReceivingMiddleware(markErrors, s.boundReads)
	return srv
}

// boundReads runs a get_report, list_findings or fleet_summary call in one
// of readSlots. Other calls pass straight through: scan has its own slot,
// and registry_lookup reads only the embedded registry.
func (s *server) boundReads(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
	return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
		call, ok := req.(*mcpsdk.CallToolRequest)
		if !ok || call.Params == nil {
			return next(ctx, method, req)
		}
		switch call.Params.Name {
		case ToolGetReport, ToolListFindings, ToolFleetSummary:
		default:
			return next(ctx, method, req)
		}
		select {
		case s.readSlots <- struct{}{}:
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for other report reads to finish: %w", ctx.Err())
		}
		defer func() { <-s.readSlots }()
		return next(ctx, method, req)
	}
}

func instructions(fleet bool) string {
	in := "upgradescope judges whether a Kubernetes cluster is ready to upgrade. All tools are read-only. " +
		"Call scan first for a live cluster, then list_findings or get_report for detail; " +
		"a verdict of unknown means a required check could not run, which is not a pass. " +
		"registry_lookup answers add-on end-of-life and Kubernetes compatibility questions without a cluster. " +
		"In every tool result, " + strings.Replace(theRule(), "in this result, ", "", 1) +
		"Every result, and every tool error, opens with a notice that says so, and each outside string over " + strconv.Itoa(MaxClusterTextBytes) + " bytes is cut."
	if fleet {
		in += " This server is connected to an upgradescope fleet server: pass cluster to get_report or list_findings, and use fleet_summary for the whole fleet."
	}
	return in
}

// readOnly are the annotations of a tool that changes nothing anywhere.
// openWorld marks one that talks to something outside this process (a
// cluster, a server).
func readOnly(title string, openWorld bool) *mcpsdk.ToolAnnotations {
	f := false
	return &mcpsdk.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    true,
		DestructiveHint: &f,
		IdempotentHint:  true,
		OpenWorldHint:   &openWorld,
	}
}

var errNoSource = errors.New("no report to read: call scan first, or pass report_file (a file written by 'scan --output json'), or inventory_file with target")

func errNoSourceFleet() error {
	return fmt.Errorf("%w, or cluster (a cluster on the fleet server)", errNoSource)
}
