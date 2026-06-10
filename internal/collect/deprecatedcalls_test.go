package collect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(body))
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
	if err := collectDeprecatedCalls(context.Background(), rc, &inv); err != nil {
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

func TestCollectDeprecatedCallsFamilyAbsent(t *testing.T) {
	rc := metricsRESTClient(t, "# TYPE apiserver_request_total counter\napiserver_request_total{code=\"200\"} 7\n")

	var inv inventory.Inventory
	if err := collectDeprecatedCalls(context.Background(), rc, &inv); err != nil {
		t.Fatal(err)
	}
	if inv.DeprecatedCalls != nil {
		t.Errorf("calls = %#v, want none when family is absent", inv.DeprecatedCalls)
	}
}
