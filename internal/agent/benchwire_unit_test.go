package agent

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestClassifyRequest(t *testing.T) {
	const meta = "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1"
	for _, tc := range []struct {
		method, path, query, accept string
		verb, resource              string
	}{
		{"GET", "/api/v1/nodes", "limit=500", "", "LIST", "nodes"},
		{"GET", "/api/v1/nodes", "continue=abc&limit=500", "", "LIST", "nodes"},
		{"GET", "/api/v1/namespaces", "limit=500", "", "LIST", "namespaces"},
		{"GET", "/api/v1/namespaces/kube-system", "", "", "GET", "namespaces"},
		{"GET", "/api/v1/namespaces/kube-system/pods", "limit=500", "", "LIST", "pods"},
		{"GET", "/api/v1/namespaces/ns-1/secrets/sh.helm.release.v1.rel-0001.v1", "", "", "GET", "secrets"},
		{"GET", "/api/v1/secrets", "labelSelector=owner%3Dhelm", meta, "LIST", "secrets (metadata)"},
		{"GET", "/api/v1/pods", "watch=true", "", "WATCH", "pods"},
		{"GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", "", "", "LIST", "customresourcedefinitions"},
		{"GET", "/apis/apps/v1/namespaces/x/deployments", "", meta, "LIST", "deployments (metadata)"},
		{"GET", "/apis/upgradescope.dev/v1alpha1/clusterreadinesses/bench", "", "", "GET", "clusterreadinesses"},
		{"PUT", "/apis/upgradescope.dev/v1alpha1/clusterreadinesses/bench/status", "", "", "UPDATE", "clusterreadinesses/status"},
		{"POST", "/apis/upgradescope.dev/v1alpha1/clusterreadinesses", "", "", "CREATE", "clusterreadinesses"},
		{"PATCH", "/apis/upgradescope.dev/v1alpha1/clusterreadinesses/bench", "", "", "PATCH", "clusterreadinesses"},
		{"GET", "/api", "", "", "GET", "discovery"},
		{"GET", "/apis", "", "", "GET", "discovery"},
		{"GET", "/api/v1", "", "", "GET", "discovery"},
		{"GET", "/apis/apps/v1", "", "", "GET", "discovery"},
		{"GET", "/version", "", "", "GET", "/version"},
		{"GET", "/metrics", "", "", "GET", "/metrics"},
	} {
		verb, resource := classifyRequest(tc.method, tc.path, tc.query, tc.accept)
		if verb != tc.verb || resource != tc.resource {
			t.Errorf("%s %s?%s: got %s %q, want %s %q", tc.method, tc.path, tc.query, verb, resource, tc.verb, tc.resource)
		}
	}
}

func TestRequestRecorderCountsRequestsAndBodyBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 100))
	}))
	defer srv.Close()
	rec := newRequestRecorder()
	cl := &http.Client{Transport: rec.transport(http.DefaultTransport)}
	for _, p := range []string{"/api/v1/nodes", "/api/v1/nodes", "/api/v1/namespaces/a/secrets/b", "/version"} {
		resp, err := cl.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	stats, total := rec.stats()
	if total != 4 || rec.bodyBytes.Load() != 400 {
		t.Errorf("total = %d, body bytes = %d; want 4 requests and 400 bytes", total, rec.bodyBytes.Load())
	}
	if len(stats) != 3 || stats[0] != (requestStat{"LIST", "nodes", 2}) {
		t.Errorf("stats = %+v, want LIST nodes x2 first, then GET secrets and GET /version", stats)
	}
	rec.reset()
	if _, total := rec.stats(); total != 0 || rec.bodyBytes.Load() != 0 {
		t.Errorf("after reset: total %d, bytes %d, want zero", total, rec.bodyBytes.Load())
	}
}

func TestWireProxyCountsBytesBothWays(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() { // answers "ping" with 1000 bytes
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 4)
				if _, err := io.ReadFull(c, buf); err == nil {
					_, _ = c.Write([]byte(strings.Repeat("y", 1000)))
				}
			}()
		}
	}()
	p, err := newWireProxy(up.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	c, err := net.Dial("tcp", p.addr())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(io.LimitReader(c, 1000))
	if err != nil || len(got) != 1000 {
		t.Fatalf("read %d bytes, %v; want 1000", len(got), err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for p.down.Load() < 1000 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.up.Load() != 4 || p.down.Load() != 1000 || p.conns.Load() != 1 {
		t.Errorf("up %d, down %d, conns %d; want 4, 1000, 1", p.up.Load(), p.down.Load(), p.conns.Load())
	}
	p.resetBytes()
	if p.up.Load() != 0 || p.down.Load() != 0 {
		t.Error("resetBytes left counts")
	}
	c.Close()
}

func TestBenchRESTConfigCountsRequestsAndWireBytesThroughTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"kind":"NodeList","apiVersion":"v1","metadata":{},"items":[]}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	kc := filepath.Join(dir, "kubeconfig")
	ca := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	if err := os.WriteFile(kc, []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters: [{name: lab, cluster: {server: %s, certificate-authority-data: %s}}]
users: [{name: u, user: {}}]
contexts: [{name: lab, context: {cluster: lab, user: u}}]
current-context: lab
`, srv.URL, ca)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, proxy, rec, err := benchRESTConfig(kc)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.close()
	if strings.Contains(cfg.Host, strings.TrimPrefix(srv.URL, "https://")) {
		t.Fatalf("host = %s, want the forwarder's address, not the apiserver's", cfg.Host)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{Limit: 500}); err != nil {
		t.Fatalf("list through the forwarder, certificate checked against the original host: %v", err)
	}
	stats, total := rec.stats()
	if total != 1 || stats[0].Verb != "LIST" || stats[0].Resource != "nodes" {
		t.Errorf("requests = %+v, want one LIST nodes", stats)
	}
	if proxy.down.Load() == 0 || proxy.up.Load() == 0 || rec.bodyBytes.Load() == 0 {
		t.Errorf("wire down %d up %d, body %d: want all counted", proxy.down.Load(), proxy.up.Load(), rec.bodyBytes.Load())
	}
	if proxy.down.Load() < rec.bodyBytes.Load() {
		t.Errorf("wire bytes %d under the body bytes %d: the wire carries at least the body (TLS adds to it)", proxy.down.Load(), rec.bodyBytes.Load())
	}
}

func TestEnvIntAndBenchSampler(t *testing.T) {
	t.Setenv("UPGRADESCOPE_BENCH_X", "7")
	if got := envInt(t, "UPGRADESCOPE_BENCH_X", 1); got != 7 {
		t.Errorf("envInt = %d, want 7", got)
	}
	if got := envInt(t, "UPGRADESCOPE_BENCH_UNSET", 3); got != 3 {
		t.Errorf("envInt default = %d, want 3", got)
	}
	s := startBenchSampler()
	keep := make([]byte, 64<<20)
	keep[len(keep)-1] = 1
	time.Sleep(30 * time.Millisecond)
	heap, total := s.finish()
	if heap < 64<<20 || total < heap {
		t.Errorf("peak heap %d, runtime total %d: want the 64 MiB held counted, total at least the heap", heap, total)
	}
	runtime.KeepAlive(keep)
	if maxRSSBytes() == 0 && runtime.GOOS != "windows" {
		t.Error("maxRSSBytes = 0 on a platform that reports it")
	}
}
