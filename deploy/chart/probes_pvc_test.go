package chart

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Tests for the server's startupProbe (#345) and the PVC options (#346).

// The server migrates its database before it opens the port, so a large
// database or slow storage must not let liveness restart it mid-migration:
// a startupProbe on /healthz holds liveness off for periodSeconds x
// failureThreshold (5s x 60 = 5 minutes by default), and both are values.
func TestServerStartupProbe(t *testing.T) {
	probe := func(t *testing.T, sets ...string) map[string]any {
		t.Helper()
		c := container(t, render(t, append([]string{"server.enabled=true"}, sets...)...), "upgradescope-server")
		p, ok, _ := unstructured.NestedMap(c, "startupProbe")
		if !ok {
			t.Fatalf("server has no startupProbe (sets %v)", sets)
		}
		return p
	}
	num := func(p map[string]any, k string) int64 {
		switch v := p[k].(type) { // JSON numbers decode as float64 or int64
		case float64:
			return int64(v)
		case int64:
			return v
		}
		return -1
	}

	p := probe(t)
	if path, _, _ := unstructured.NestedString(p, "httpGet", "path"); path != "/healthz" {
		t.Errorf("startupProbe path = %q, want /healthz", path)
	}
	if port, _, _ := unstructured.NestedString(p, "httpGet", "port"); port != "http" {
		t.Errorf("startupProbe port = %q, want http (the liveness port)", port)
	}
	if scheme, _, _ := unstructured.NestedString(p, "httpGet", "scheme"); scheme != "" {
		t.Errorf("startupProbe scheme = %q without TLS, want none (HTTP)", scheme)
	}
	if got := num(p, "periodSeconds"); got != 5 {
		t.Errorf("startupProbe periodSeconds = %d, want 5", got)
	}
	if got := num(p, "failureThreshold"); got != 60 {
		t.Errorf("startupProbe failureThreshold = %d, want 60 (5 minutes)", got)
	}

	p = probe(t, "server.startupProbe.failureThreshold=120", "server.startupProbe.periodSeconds=10")
	if got := num(p, "failureThreshold"); got != 120 {
		t.Errorf("server.startupProbe.failureThreshold=120 renders %d", got)
	}
	if got := num(p, "periodSeconds"); got != 10 {
		t.Errorf("server.startupProbe.periodSeconds=10 renders %d", got)
	}

	p = probe(t, "server.ingestToken=t", "server.readToken=r", "server.tls.secretName=srv-tls")
	if scheme, _, _ := unstructured.NestedString(p, "httpGet", "scheme"); scheme != "HTTPS" {
		t.Errorf("startupProbe scheme = %q with server.tls, want HTTPS as liveness", scheme)
	}

	// Liveness and readiness are what they were.
	c := container(t, render(t, "server.enabled=true"), "upgradescope-server")
	for _, tc := range []struct {
		probe, path   string
		delay, period int64
	}{{"livenessProbe", "/healthz", 5, 15}, {"readinessProbe", "/readyz", 2, 5}} {
		lp, _, _ := unstructured.NestedMap(c, tc.probe)
		if path, _, _ := unstructured.NestedString(lp, "httpGet", "path"); path != tc.path || num(lp, "initialDelaySeconds") != tc.delay || num(lp, "periodSeconds") != tc.period {
			t.Errorf("%s = %v, want %s, initialDelaySeconds %d, periodSeconds %d", tc.probe, lp, tc.path, tc.delay, tc.period)
		}
	}

	// The agent runs no migrations: no startupProbe there.
	if _, ok, _ := unstructured.NestedMap(container(t, render(t), "upgradescope-agent"), "startupProbe"); ok {
		t.Error("the agent got a startupProbe")
	}
}

func TestSchemaServerStartupProbe(t *testing.T) {
	for _, bad := range []string{"server.startupProbe.failureThreshold=0", "server.startupProbe.periodSeconds=0", "server.startupProbe.failureThreshold=lots", "server.startupProbe.timeoutSeconds=3"} {
		if renderErr(t, "server.enabled=true", bad) == "" {
			t.Errorf("%s rendered, want a schema error", bad)
		}
	}
}

// upgrade.md states the startup window and how to widen it.
func TestDocsGiveTheStartupWindow(t *testing.T) {
	b, err := os.ReadFile("../../docs/operations/upgrade.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Join(strings.Fields(string(b)), " ")
	for _, want := range []string{"server.startupProbe.failureThreshold", "5 minutes", "before it opens its port"} {
		if !strings.Contains(doc, want) {
			t.Errorf("upgrade.md does not say %q", want)
		}
	}
}

func serverPVC(objs []unstructured.Unstructured) *unstructured.Unstructured {
	return find(objs, "PersistentVolumeClaim", "upgradescope-server-data")
}

func dataClaim(t *testing.T, objs []unstructured.Unstructured) string {
	t.Helper()
	v := volumeNamed(t, objs, "upgradescope-server", "data")
	if v == nil {
		t.Fatal("server has no data volume")
	}
	s, _, _ := unstructured.NestedString(v, "persistentVolumeClaim", "claimName")
	return s
}

func TestServerPersistenceDefault(t *testing.T) {
	objs := render(t, "server.enabled=true")
	pvc := serverPVC(objs)
	if pvc == nil {
		t.Fatal("default renders no server PVC")
	}
	modes, _, _ := unstructured.NestedStringSlice(pvc.Object, "spec", "accessModes")
	if !reflect.DeepEqual(modes, []string{"ReadWriteOnce"}) {
		t.Errorf("accessModes = %v, want [ReadWriteOnce]", modes)
	}
	if ann := pvc.GetAnnotations(); len(ann) != 0 {
		t.Errorf("default PVC annotations = %v, want none (no resource-policy keep)", ann)
	}
	if got := dataClaim(t, objs); got != "upgradescope-server-data" {
		t.Errorf("data volume claim = %q, want the chart's PVC", got)
	}
}

// server.persistence.existingClaim: no PVC of the chart's; the server
// mounts the named claim, which outlives the release.
func TestServerPersistenceExistingClaim(t *testing.T) {
	objs := render(t, "server.enabled=true", "server.persistence.existingClaim=my-history")
	if serverPVC(objs) != nil {
		t.Error("existingClaim still renders the chart's PVC")
	}
	for _, o := range objs {
		if o.GetKind() == "PersistentVolumeClaim" {
			t.Errorf("existingClaim renders a PVC %s", o.GetName())
		}
	}
	if got := dataClaim(t, objs); got != "my-history" {
		t.Errorf("data volume claim = %q, want my-history", got)
	}
	// persistence.enabled=false is still an emptyDir, existingClaim or not.
	objs = render(t, "server.enabled=true", "server.persistence.enabled=false", "server.persistence.existingClaim=my-history")
	if v := volumeNamed(t, objs, "upgradescope-server", "data"); v == nil || v["emptyDir"] == nil || serverPVC(objs) != nil {
		t.Errorf("persistence.enabled=false: data volume %v, want an emptyDir and no PVC", v)
	}
}

// retain renders helm.sh/resource-policy: keep, so helm uninstall leaves
// the PVC (and its history); annotations and accessModes render as set.
func TestServerPersistenceOptions(t *testing.T) {
	objs := render(t, "server.enabled=true", "server.persistence.retain=true",
		"server.persistence.annotations.backup\\.example\\.com/schedule=nightly",
		"server.persistence.accessModes={ReadWriteOncePod}")
	pvc := serverPVC(objs)
	if pvc == nil {
		t.Fatal("no server PVC")
	}
	want := map[string]string{"helm.sh/resource-policy": "keep", "backup.example.com/schedule": "nightly"}
	if ann := pvc.GetAnnotations(); !reflect.DeepEqual(ann, want) {
		t.Errorf("PVC annotations = %v, want %v", ann, want)
	}
	modes, _, _ := unstructured.NestedStringSlice(pvc.Object, "spec", "accessModes")
	if !reflect.DeepEqual(modes, []string{"ReadWriteOncePod"}) {
		t.Errorf("accessModes = %v, want [ReadWriteOncePod]", modes)
	}
}

func TestSchemaServerPersistence(t *testing.T) {
	for _, ok := range []string{"server.persistence.size=1Gi", "server.persistence.size=500Mi", "server.persistence.size=10G", "server.persistence.size=1.5Gi",
		"server.persistence.accessModes={ReadWriteOnce,ReadOnlyMany}", "server.persistence.accessModes={ReadWriteMany}"} {
		if msg := renderErr(t, "server.enabled=true", ok); msg != "" {
			t.Errorf("%s rejected: %s", ok, msg)
		}
	}
	for _, bad := range []string{"server.persistence.size=lots", "server.persistence.size=1 Gi", "server.persistence.size=-1Gi", "server.persistence.size=1gi",
		"server.persistence.accessModes={ReadWriteSometimes}",
		"server.persistence.existingClaim=Bad_Name", "server.persistence.retain=yes"} {
		msg := renderErr(t, "server.enabled=true", bad)
		if msg == "" {
			t.Errorf("%s rendered, want a schema error", bad)
		} else if bad == "server.persistence.size=lots" && !strings.Contains(msg, "schema") {
			t.Errorf("size=lots fails, but not with a schema error: %s", msg)
		}
	}
	if renderErr(t, "server.enabled=true", writeValues(t, "server:\n  persistence:\n    accessModes: []\n")) == "" {
		t.Error("empty accessModes rendered, want a schema error")
	}
}
