package collect

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

const metricsBody = `# HELP apiserver_request_total Counter of apiserver requests broken out for each verb.
# TYPE apiserver_request_total counter
apiserver_request_total{code="200",resource="pods",verb="LIST"} 1042
# HELP apiserver_requested_deprecated_apis Gauge of deprecated APIs that have been requested, broken out by API group, version, resource, subresource, and removed_release.
# TYPE apiserver_requested_deprecated_apis gauge
apiserver_requested_deprecated_apis{group="flowcontrol.apiserver.k8s.io",removed_release="1.32",resource="flowschemas",subresource="",version="v1beta3"} 1
apiserver_requested_deprecated_apis{group="",removed_release="",resource="componentstatuses",subresource="",version="v1"} 1
`

func metricsRESTClient(t *testing.T, body string) rest.Interface {
	t.Helper()
	return metricsRESTClientFunc(t, func() string { return body })
}

// metricsRESTClientFunc serves body() as /metrics on every scrape.
func metricsRESTClientFunc(t *testing.T, body func() string) rest.Interface {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(body()))
	}))
	t.Cleanup(srv.Close)

	cfg := &rest.Config{
		Host: srv.URL,
		ContentConfig: rest.ContentConfig{
			GroupVersion:         &schema.GroupVersion{},
			NegotiatedSerializer: clientgoscheme.Codecs.WithoutConversion(),
		},
	}
	rc, err := rest.UnversionedRESTClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

func TestCollectDeprecatedCalls(t *testing.T) {
	rc := metricsRESTClient(t, metricsBody)

	var inv inventory.Inventory
	if err := collectDeprecatedCalls(context.Background(), rc, selfCalls{}, &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.DeprecatedCall{
		{Group: "", Version: "v1", Resource: "componentstatuses"},
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Resource: "flowschemas", RemovedRelease: "1.32"},
	}
	if !reflect.DeepEqual(inv.DeprecatedCalls, want) {
		t.Errorf("calls = %#v\nwant  %#v", inv.DeprecatedCalls, want)
	}
}

// The apiserver's process start time, from the same scrape, says since
// when apiserver_requested_deprecated_apis has counted: it is recorded
// whether or not any deprecated API was requested, in whole seconds. A
// value that is not a time (none, several series, zero, NaN, beyond any
// date) is not recorded; whether a time is plausible for the scrape is the
// server's to judge (apiServerStart, internal/server).
func TestCollectDeprecatedCallsRecordsAPIServerStart(t *testing.T) {
	const started = `# HELP process_start_time_seconds Start time of the process since unix epoch in seconds.
# TYPE process_start_time_seconds gauge
process_start_time_seconds 1.78592040037e+09
`
	want := time.Unix(1785920400, 0).UTC()
	for name, body := range map[string]string{"with calls": metricsBody + started, "without calls": started} {
		var inv inventory.Inventory
		if err := collectDeprecatedCalls(context.Background(), metricsRESTClient(t, body), selfCalls{}, &inv); err != nil {
			t.Fatal(err)
		}
		if !inv.APIServerStartTime.Equal(want) || inv.APIServerStartTime.Location() != time.UTC {
			t.Errorf("%s: start time = %v, want %v", name, inv.APIServerStartTime, want)
		}
	}

	const gauge = "# TYPE process_start_time_seconds gauge\n"
	for name, body := range map[string]string{
		"absent":   metricsBody,
		"zero":     gauge + "process_start_time_seconds 0\n",
		"negative": gauge + "process_start_time_seconds -5\n",
		"NaN":      gauge + "process_start_time_seconds NaN\n",
		"+Inf":     gauge + "process_start_time_seconds +Inf\n",
		"huge":     gauge + "process_start_time_seconds 1e300\n",
		"two":      gauge + "process_start_time_seconds{a=\"1\"} 1.7e+09\nprocess_start_time_seconds{a=\"2\"} 1.7e+09\n",
	} {
		var inv inventory.Inventory
		if err := collectDeprecatedCalls(context.Background(), metricsRESTClient(t, body), selfCalls{}, &inv); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !inv.APIServerStartTime.IsZero() {
			t.Errorf("%s: start time = %v, want none recorded", name, inv.APIServerStartTime)
		}
	}
}

// The scanner's own deprecated LISTs (selfListed, from api-usage) are
// rows the metric cannot tell apart from other clients': the capability is
// partial, naming them, and the rows stay in the inventory for the engine
// to discount.
func TestCollectDeprecatedCallsMarksSelfListedResources(t *testing.T) {
	rc := metricsRESTClient(t, metricsBody)
	self := []string{"v1 componentstatuses"}

	var inv inventory.Inventory
	err := collectDeprecatedCalls(context.Background(), rc, selfCalls{listed: self}, &inv)
	var pe partialError
	if !errors.As(err, &pe) || !pe.incomplete || !reflect.DeepEqual(pe.skipped, self) {
		t.Fatalf("err = %#v, want an incomplete partialError skipping %q", err, self)
	}
	if !strings.Contains(pe.msg, "upgradescope lists v1 componentstatuses itself") {
		t.Errorf("reason = %q, must say the scanner lists it itself", pe.msg)
	}
	if len(inv.DeprecatedCalls) != 2 {
		t.Errorf("calls = %#v, want both rows kept", inv.DeprecatedCalls)
	}
}

// metricsDenyRESTClient returns the given HTTP status for /metrics —
// what managed control planes (EKS/GKE/AKS) do when they block the
// nonResourceURL even though RBAC grants it.
func metricsDenyRESTClient(t *testing.T, status int) rest.Interface {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, http.StatusText(status), status)
	}))
	t.Cleanup(srv.Close)

	cfg := &rest.Config{
		Host: srv.URL,
		ContentConfig: rest.ContentConfig{
			GroupVersion:         &schema.GroupVersion{},
			NegotiatedSerializer: clientgoscheme.Codecs.WithoutConversion(),
		},
	}
	rc, err := rest.UnversionedRESTClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

func TestCollectDeprecatedCallsForbidden(t *testing.T) {
	// 401/403 must yield an actionable capability reason, not a raw
	// client-go error: this is the expected state on managed control
	// planes and the user needs to know it is benign and documented.
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			rc := metricsDenyRESTClient(t, status)

			var inv inventory.Inventory
			err := collectDeprecatedCalls(context.Background(), rc, selfCalls{}, &inv)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			const want = "apiserver /metrics forbidden (managed control planes often block this; see README §Managed clusters)"
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
			if inv.DeprecatedCalls != nil {
				t.Errorf("calls = %#v, want none on denied scrape", inv.DeprecatedCalls)
			}
		})
	}
}

func TestCollectDeprecatedCallsOtherErrorNotRewritten(t *testing.T) {
	// A 500 is not the managed-control-plane case; the reason must not
	// point users at the Managed clusters doc for an unrelated outage.
	rc := metricsDenyRESTClient(t, http.StatusInternalServerError)

	var inv inventory.Inventory
	err := collectDeprecatedCalls(context.Background(), rc, selfCalls{}, &inv)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if strings.Contains(err.Error(), "Managed clusters") {
		t.Errorf("error = %q, must not mention Managed clusters for a 500", err)
	}
}

func TestCollectDeprecatedCallsFamilyAbsent(t *testing.T) {
	rc := metricsRESTClient(t, "# TYPE apiserver_request_total counter\napiserver_request_total{code=\"200\"} 7\n")

	var inv inventory.Inventory
	if err := collectDeprecatedCalls(context.Background(), rc, selfCalls{}, &inv); err != nil {
		t.Fatal(err)
	}
	if inv.DeprecatedCalls != nil {
		t.Errorf("calls = %#v, want none when family is absent", inv.DeprecatedCalls)
	}
}

// #239: rows at a group/version api-usage's discovery did not show (it
// failed, or skipped the group) may be an earlier scan's own LISTs: they
// are withheld and named, the scanner's listed ones kept as they were. A
// subresource row is someone else's, and a row at another group/version
// is attributed as always.
func TestCollectDeprecatedCallsWithholdsRowsDiscoveryCouldNotAttribute(t *testing.T) {
	body := `# TYPE apiserver_requested_deprecated_apis gauge
apiserver_requested_deprecated_apis{group="policy",removed_release="1.25",resource="podsecuritypolicies",subresource="",version="v1beta1"} 1
apiserver_requested_deprecated_apis{group="policy",removed_release="1.25",resource="poddisruptionbudgets",subresource="status",version="v1beta1"} 1
apiserver_requested_deprecated_apis{group="flowcontrol.apiserver.k8s.io",removed_release="1.32",resource="flowschemas",subresource="",version="v1beta3"} 1
`
	self := selfCalls{listed: []string{"coordination.k8s.io/v1beta1 leasecandidates"}, undiscovered: []string{"policy/v1beta1"}}
	var inv inventory.Inventory
	err := collectDeprecatedCalls(context.Background(), metricsRESTClient(t, body), self, &inv)
	var pe partialError
	if !errors.As(err, &pe) || !pe.incomplete {
		t.Fatalf("err = %#v, want an incomplete partialError", err)
	}
	if want := []string{"coordination.k8s.io/v1beta1 leasecandidates", "policy/v1beta1 podsecuritypolicies"}; !reflect.DeepEqual(pe.skipped, want) {
		t.Errorf("skipped %q, want %q", pe.skipped, want)
	}
	for _, want := range []string{"upgradescope lists coordination.k8s.io/v1beta1 leasecandidates itself", "did not show what upgradescope lists at policy/v1beta1", "policy/v1beta1 podsecuritypolicies may be upgradescope's"} {
		if !strings.Contains(pe.msg, want) {
			t.Errorf("reason %q\nmissing %q", pe.msg, want)
		}
	}
	if len(inv.DeprecatedCalls) != 3 {
		t.Errorf("calls %+v: every row is kept for the engine", inv.DeprecatedCalls)
	}

	// Discovery got through and the scanner listed nothing deprecated: no gap.
	if err := collectDeprecatedCalls(context.Background(), metricsRESTClient(t, body), selfCalls{}, &inventory.Inventory{}); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

// What api-usage can say before discovery answers: every group/version at
// which the knowledge base schedules a removal, the only ones the scanner
// lists at a deprecated version.
func TestUnknownSelfCallsAreTheRemovalGroupVersions(t *testing.T) {
	self := unknownSelfCalls(loadKB(t).APILifecycle, nil)
	for _, want := range []string{"policy/v1beta1", "coordination.k8s.io/v1beta1", "flowcontrol.apiserver.k8s.io/v1beta3"} {
		if !slices.Contains(self.undiscovered, want) {
			t.Errorf("undiscovered %q: missing %s", self.undiscovered, want)
		}
	}
	if slices.Contains(self.undiscovered, "v1") || len(self.listed) != 0 {
		t.Errorf("undiscovered %q, listed %q: core v1 (Endpoints, ComponentStatus) is never removed", self.undiscovered, self.listed)
	}
	if got := unknownSelfCalls(loadKB(t).APILifecycle, func(gv schema.GroupVersion) bool { return gv.Group == "policy" }); !reflect.DeepEqual(got.undiscovered, []string{"policy/v1beta1"}) {
		t.Errorf("only policy skipped: undiscovered %q, want [policy/v1beta1]", got.undiscovered)
	}
}
