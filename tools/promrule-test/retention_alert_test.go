// Package promruletest evaluates the chart's PrometheusRule expressions with
// the Prometheus PromQL engine. It is a module of its own because the
// engine is too heavy a dependency for the main one.
package promruletest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/util/teststorage"
	"go.yaml.in/yaml/v3"
)

const job = "upgradescope-server"

// renderedRule is the named alert of the chart's PrometheusRule, rendered
// with helm template for the server with its default retention. Without
// helm the test skips, as the chart tests do, unless
// UPGRADESCOPE_CHART_TEST=1.
func renderedRule(t *testing.T, alert string) (expr, forDuration string) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("UPGRADESCOPE_CHART_TEST") == "1" {
			t.Fatal("helm not found in PATH (UPGRADESCOPE_CHART_TEST=1)")
		}
		t.Skip("helm not found in PATH")
	}
	cmd := exec.Command(helm, "template", "upgradescope", "../../deploy/chart", "--namespace", "upgradescope",
		"--set", "metrics.prometheusRule.enabled=true", "--set", "server.enabled=true", "--set", "server.ingestToken=t",
		"--show-only", "templates/prometheusrule.yaml")
	cmd.Env = append(os.Environ(), "KUBECONFIG=/dev/null")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	var pr struct {
		Spec struct {
			Groups []struct {
				Rules []struct {
					Alert string `yaml:"alert"`
					Expr  string `yaml:"expr"`
					For   string `yaml:"for"`
				} `yaml:"rules"`
			} `yaml:"groups"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(out, &pr); err != nil {
		t.Fatalf("decode PrometheusRule: %v", err)
	}
	for _, g := range pr.Spec.Groups {
		for _, r := range g.Rules {
			if r.Alert == alert {
				return strings.Join(strings.Fields(r.Expr), " "), r.For
			}
		}
	}
	t.Fatalf("%s not rendered", alert)
	return "", ""
}

// run is a promqltest script, with the 5 minute staleness lookback of a
// real Prometheus. The test storage appends one series after another, so
// it takes samples out of order (by up to 30 days) or a longer series
// loaded first would make the shorter ones "out of bounds", depending on
// the order of a map.
func run(t *testing.T, script string) {
	t.Helper()
	promqltest.RunTestWithStorage(t, script, promqltest.NewTestEngine(t, false, 5*time.Minute, promqltest.DefaultMaxSamplesPerQuery),
		func(tt testing.TB) storage.Storage {
			return teststorage.New(tt, func(o *tsdb.Options) { o.OutOfOrderTimeWindow = (30 * 24 * time.Hour).Milliseconds() })
		})
}

// series is one scraped process: the retention failures counter and
// process_start_time_seconds of instance, each sample a minute apart,
// failures one value a sample (the load syntax: "1x719", "0 1x718",
// "_x720 1x719"), started at the epoch second start; success is its
// last-success gauge values ("" for none).
type series struct {
	instance string
	failures string
	start    int
	samples  int // the process_start_time_seconds samples, after the gap
	gap      int // minutes before the first sample
	success  string
}

func (s series) load() string {
	var b strings.Builder
	lead := ""
	if s.gap > 0 {
		lead = fmt.Sprintf("_x%d ", s.gap)
	}
	fmt.Fprintf(&b, "  upgradescope_retention_prune_failures_total{job=%q,instance=%q,store=\"sqlite\"} %s%s\n", job, s.instance, lead, s.failures)
	fmt.Fprintf(&b, "  process_start_time_seconds{job=%q,instance=%q} %s%d+0x%d\n", job, s.instance, lead, s.start, s.samples-1)
	if s.success != "" {
		fmt.Fprintf(&b, "  upgradescope_retention_last_success_timestamp_seconds{job=%q,instance=%q} %s%s\n", job, s.instance, lead, s.success)
	}
	return b.String()
}

// fires is the script evaluating expr at each of the times, expecting the
// alert's one series {job} with value 1; quiet, expecting none.
func fires(expr string, at ...string) string {
	var b strings.Builder
	for _, a := range at {
		fmt.Fprintf(&b, "\neval instant at %s %s\n  {job=%q} 1\n", a, expr, job)
	}
	return b.String()
}

func quiet(expr string, at ...string) string {
	var b strings.Builder
	for _, a := range at {
		fmt.Fprintf(&b, "\neval instant at %s %s\n", a, expr)
	}
	return b.String()
}

// every5m is the evaluation times, a 5 minute step, of the 15 minutes
// before end (inclusive): an alert with for: 15m is firing when its
// expression is true at all of them.
func every5m(endMinutes int) []string {
	var at []string
	for m := endMinutes - 15; m <= endMinutes; m += 5 {
		at = append(at, fmt.Sprintf("%dm", m))
	}
	return at
}

// TestRetentionStaleFiresWhenEverySeriesIsFirstSeenAtOne is the case of
// #296. The server restarts every 12h and its startup prune fails within
// milliseconds, before Prometheus has scraped the new target at all: each
// failures series is first seen at 1 (a new pod is a new series; a
// container restart under the same labels goes 1 to 1), so increase() over
// it is 0 and the old third arm stayed quiet while the database grew. A
// failure counted by a process that has no completed prune is the
// condition, whenever it was first scraped.
func TestRetentionStaleFiresWhenEverySeriesIsFirstSeenAtOne(t *testing.T) {
	expr, forDuration := renderedRule(t, "UpgradescopeRetentionStale")
	if forDuration != "15m" {
		t.Fatalf("for = %q, want 15m: the cases below hold the alert for 15 minutes", forDuration)
	}
	pods := []series{
		{instance: "a", failures: "1x719", start: 0, samples: 720},
		{instance: "b", failures: "1x719", start: 43200, samples: 720, gap: 720},
		{instance: "c", failures: "1x719", start: 86400, samples: 720, gap: 1440},
	}
	var load strings.Builder
	for _, p := range pods {
		load.WriteString(p.load())
	}
	// 35h: pod c is 11h old, as young as the first two were. Held for the
	// 15 minutes before, so the alert is firing; so it is while each of the
	// others is the live one, and the old pods' series are stale.
	run(t, "load 1m\n"+load.String()+fires(expr, every5m(35*60)...)+fires(expr, every5m(23*60)...)+fires(expr, every5m(11*60)...))
}

// TestRetentionStaleFiresWhenTheFailureLandsAfterTheFirstScrape is the shape
// the first version of the test modelled: the gauge absent, the counter
// 0 at the first scrape and 1 at the next.
func TestRetentionStaleFiresWhenTheFailureLandsAfterTheFirstScrape(t *testing.T) {
	expr, _ := renderedRule(t, "UpgradescopeRetentionStale")
	p := series{instance: "a", failures: "0 1x718", start: 86400, samples: 720, gap: 1440}
	run(t, "load 1m\n"+p.load()+fires(expr, every5m(35*60)...))
}

// TestRetentionStaleStaysQuiet: no failure; a recent success; a failure
// since followed by a success; a pod that restarted after its prune
// succeeded (its old series are gone, the new process has counted nothing
// and its first prune is still running); a counter reset; nothing scraped.
func TestRetentionStaleStaysQuiet(t *testing.T) {
	expr, _ := renderedRule(t, "UpgradescopeRetentionStale")
	for name, c := range map[string]struct {
		load string
		at   []string
	}{
		"no failure, no success yet, a young process": {
			series{instance: "a", failures: "0x719", start: 0, samples: 720}.load(), every5m(11 * 60),
		},
		"a recent success": {
			// Pruned at 1000s; at 30h that is 27h old.
			series{instance: "a", failures: "0x1799", start: 0, samples: 1800, success: "1000+0x1799"}.load(), every5m(30 * 60),
		},
		"a failure, then a success within the 2 days": {
			// Failed at startup, pruned at 86400s (24h) when the daily run came.
			series{instance: "a", failures: "1x2999", start: 0, samples: 3000, success: "_x1440 86400+0x1559"}.load(), every5m(49 * 60),
		},
		"a restart after a successful prune: the old pod's series end, the new process has counted nothing": {
			series{instance: "old", failures: "0x599", start: 0, samples: 600, success: "100+0x599"}.load() +
				series{instance: "new", failures: "0x119", start: 36000, samples: 120, gap: 600}.load(),
			every5m(11 * 60),
		},
		"a counter reset under the same labels, the new process has counted nothing": {
			// Failed at startup (1), restarted: the same series, now 0,
			// and the new process's startup prune has not finished.
			series{instance: "a", failures: "1x359 0x359", start: 0, samples: 720}.load(), every5m(11 * 60),
		},
		"nothing scraped": {"", every5m(60 * 60)},
	} {
		t.Run(name, func(t *testing.T) {
			load := "load 1m\n  unrelated_metric{job=\"x\"} 1x1000\n" + c.load
			run(t, load+quiet(expr, c.at...))
		})
	}
}

// TestRetentionStaleOtherArms: the first two arms still hold. A prune
// that last completed over 2 days ago fires (and one 47 hours ago does
// not); a process over 2 days old with no completed prune fires though it
// counted no failure (a prune that hangs, or has not finished a big
// backlog).
func TestRetentionStaleOtherArms(t *testing.T) {
	expr, _ := renderedRule(t, "UpgradescopeRetentionStale")
	t.Run("last success over 2 days old", func(t *testing.T) {
		p := series{instance: "a", failures: "0x3100", start: 0, samples: 3101, success: "0+0x3100"}
		// The arm keeps the gauge's own labels and its value, the age.
		run(t, "load 1m\n"+p.load()+quiet(expr, "47h")+"\neval instant at 49h "+expr+"\n  {instance=\"a\", job=\""+job+"\"} 176400\n")
	})
	t.Run("no prune completed in a process over 2 days old", func(t *testing.T) {
		p := series{instance: "a", failures: "0x3100", start: 0, samples: 3101}
		run(t, "load 1m\n"+p.load()+quiet(expr, "47h")+fires(expr, "49h"))
	})
}
