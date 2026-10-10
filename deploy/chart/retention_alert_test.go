package chart

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestRetentionStaleAlert: the server group of the PrometheusRule alerts
// when no retention prune has completed in 2 days (172800s), or none has
// since a server started more than 2 days ago (the gauge is absent until
// the first one completes), or, for a server that restarts more often than
// that, when the gauge is absent and a prune failure has been counted: the
// startup prune fails every time, so the gauge never appears and the
// process never gets 2 days old. The third arm reads the failures
// counter's value, not its increase: the startup prune fails within
// milliseconds, before the first scrape, so the series is first seen at 1
// and increase() over it is 0 (#296). On the server job like the other
// server alerts, and only where a prune runs: not without the server, and
// not with server.retention 0 in any spelling, where the server exports no
// retention series and an absent gauge means nothing.
//
// The expression is checked as text here. It is evaluated with the
// Prometheus PromQL engine in tools/promrule-test (a module of its own,
// since the engine is too heavy a dependency for this one), which renders
// this chart: it fires for pods that restart every 12h with a failing
// startup prune whose every counter series is first seen at 1, and stays
// quiet with no failure, with a recent success, after a restart that
// followed a successful prune, and with nothing scraped. The arms must be
// joined with on(): absent() carries only the job label, the failures
// counter carries instance and store as well.
func TestRetentionStaleAlert(t *testing.T) {
	on := []string{"metrics.prometheusRule.enabled=true", "server.enabled=true", "server.ingestToken=t"}
	rule := alerts(t, render(t, on...))["UpgradescopeRetentionStale"]
	if rule == nil {
		t.Fatal("UpgradescopeRetentionStale not rendered with the server and the default retention")
	}
	expr, _ := rule["expr"].(string)
	norm := strings.Join(strings.Fields(expr), " ")
	const gauge = `upgradescope_retention_last_success_timestamp_seconds{job="upgradescope-server"}`
	for _, want := range []string{
		// Arm 1: the last complete prune is more than 2 days old.
		`time() - ` + gauge + ` > 172800`,
		// Arm 2: no prune since a server that started more than 2 days ago.
		`(absent(` + gauge + `) and on() (time() - min(process_start_time_seconds{job="upgradescope-server"}) > 172800))`,
		// Arm 3: a failure counted by a process that has completed no
		// prune, however young. The value, not increase(): the series is
		// first seen at 1 (#296).
		`(absent(` + gauge + `) and on() max(upgradescope_retention_prune_failures_total{job="upgradescope-server"}) > 0)`,
	} {
		if !strings.Contains(norm, want) {
			t.Errorf("UpgradescopeRetentionStale expr = %q, lacks %q", norm, want)
		}
	}
	if strings.Contains(norm, "increase(") {
		t.Errorf("UpgradescopeRetentionStale expr = %q, uses increase(), which is 0 over a series first seen at 1 (#296)", norm)
	}
	if n := strings.Count(norm, " or "); n != 2 {
		t.Errorf("UpgradescopeRetentionStale expr joins its arms with %d 'or', want 2: %q", n, norm)
	}
	if rule["for"] != "15m" {
		t.Errorf("for = %v, want 15m", rule["for"])
	}
	if sev, _, _ := unstructured.NestedString(rule, "labels", "severity"); sev != "warning" {
		t.Errorf("severity = %q, want warning", sev)
	}
	if d, _, _ := unstructured.NestedString(rule, "annotations", "description"); !strings.Contains(d, "logs deploy/upgradescope-server") {
		t.Errorf("description = %q, want the logs command for the server Deployment", d)
	}

	// Another release name moves the job label with the Service name.
	other := alerts(t, renderRelease(t, "prod", on...))["UpgradescopeRetentionStale"]
	// Every selector of every arm moves: five of them, none left behind.
	e, _ := other["expr"].(string)
	if n := strings.Count(e, `job="prod-upgradescope-server"`); n != 5 || strings.Contains(e, `job="upgradescope-server"`) {
		t.Errorf("expr under release prod = %q, want the server job of that release in all 5 selectors", e)
	}

	for _, off := range []string{"0", "0d", "0h", "00d", "0.h"} {
		set := append([]string{"server.retention=" + off}, on...)
		if _, ok := alerts(t, render(t, set...))["UpgradescopeRetentionStale"]; ok {
			t.Errorf("UpgradescopeRetentionStale rendered with server.retention=%s, which keeps everything", off)
		}
	}
	for _, kept := range []string{"90d", "2160h", "24h", "10d"} {
		set := append([]string{"server.retention=" + kept}, on...)
		if _, ok := alerts(t, render(t, set...))["UpgradescopeRetentionStale"]; !ok {
			t.Errorf("UpgradescopeRetentionStale missing with server.retention=%s", kept)
		}
	}
	// --set passes 0 as a number.
	if _, ok := alerts(t, render(t, append([]string{"server.retention=0"}, on...)...))["UpgradescopeRetentionStale"]; ok {
		t.Error("UpgradescopeRetentionStale rendered with the number 0")
	}
	if _, ok := alerts(t, render(t, "metrics.prometheusRule.enabled=true"))["UpgradescopeRetentionStale"]; ok {
		t.Error("UpgradescopeRetentionStale rendered without server.enabled")
	}
}
