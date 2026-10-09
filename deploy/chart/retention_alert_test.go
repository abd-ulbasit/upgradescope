package chart

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestRetentionStaleAlert: the server group of the PrometheusRule alerts
// when no retention prune has completed in 2 days (172800s), or none has
// since a server started more than 2 days ago (the gauge is absent until
// the first one completes), on the server job like the other server
// alerts, and only where a prune runs: not without the server, and not
// with server.retention 0 in any spelling, where the server exports no
// retention series and an absent gauge means nothing.
func TestRetentionStaleAlert(t *testing.T) {
	on := []string{"metrics.prometheusRule.enabled=true", "server.enabled=true", "server.ingestToken=t"}
	rule := alerts(t, render(t, on...))["UpgradescopeRetentionStale"]
	if rule == nil {
		t.Fatal("UpgradescopeRetentionStale not rendered with the server and the default retention")
	}
	expr, _ := rule["expr"].(string)
	for _, want := range []string{
		`time() - upgradescope_retention_last_success_timestamp_seconds{job="upgradescope-server"} > 172800`,
		`absent(upgradescope_retention_last_success_timestamp_seconds{job="upgradescope-server"})`,
		`time() - min(process_start_time_seconds{job="upgradescope-server"}) > 172800`,
	} {
		if !strings.Contains(expr, want) {
			t.Errorf("UpgradescopeRetentionStale expr = %q, lacks %q", expr, want)
		}
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
	if e, _ := other["expr"].(string); !strings.Contains(e, `job="prod-upgradescope-server"`) {
		t.Errorf("expr under release prod = %q, want the server job of that release", e)
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
