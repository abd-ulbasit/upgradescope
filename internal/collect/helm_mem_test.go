package collect

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// helmAPIServer is a minimal apiserver holding releases × revisions Helm
// release Secrets that all carry payload (Helm's base64(gzip(JSON))). It
// streams every response from the one precomputed payload, so the server
// adds next to nothing to the heap the collector is measured by. It serves
// what collectHelm asks for: cluster-wide Secret and ConfigMap lists (full
// objects, or metadata-only when the Accept header asks for
// PartialObjectMetadataList), paged by limit/continue, and single Secret
// GETs. Revision rev of release r is "rel-<r>" in namespace
// "team-<r mod 10>"; the newest revision is deployed, older ones superseded.
func helmAPIServer(t testing.TB, releases, revisions int, payload string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	data := base64.StdEncoding.EncodeToString([]byte(payload)) // Secret.data is base64 on the wire
	var gets atomic.Int64
	ident := func(i int) (ns, rel string, rev int, status string) {
		r := i / revisions
		rev = i%revisions + 1
		status = "superseded"
		if rev == revisions {
			status = "deployed"
		}
		return fmt.Sprintf("team-%d", r%10), fmt.Sprintf("rel-%d", r), rev, status
	}
	meta := func(w *bufio.Writer, i int) {
		ns, rel, rev, status := ident(i)
		fmt.Fprintf(w, `{"name":"sh.helm.release.v1.%s.v%d","namespace":%q,"labels":{"owner":"helm","name":%q,"status":%q,"version":"%d"}}`,
			rel, rev, ns, rel, status, rev)
	}
	secret := func(w *bufio.Writer, i int) {
		w.WriteString(`{"kind":"Secret","apiVersion":"v1","metadata":`)
		meta(w, i)
		w.WriteString(`,"type":"helm.sh/release.v1","data":{"release":"`)
		w.WriteString(data)
		w.WriteString(`"}}`)
	}
	total := releases * revisions
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		w := bufio.NewWriter(rw)
		defer w.Flush()
		metadataOnly := strings.Contains(r.Header.Get("Accept"), "as=PartialObjectMetadataList")
		switch {
		case r.URL.Path == "/api/v1/configmaps":
			if metadataOnly {
				w.WriteString(`{"kind":"PartialObjectMetadataList","apiVersion":"meta.k8s.io/v1","metadata":{},"items":[]}`)
			} else {
				w.WriteString(`{"kind":"ConfigMapList","apiVersion":"v1","metadata":{},"items":[]}`)
			}
		case r.URL.Path == "/api/v1/secrets":
			start, _ := strconv.Atoi(r.URL.Query().Get("continue"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			end := total
			if limit > 0 && start+limit < total {
				end = start + limit
			}
			next := ""
			if end < total {
				next = strconv.Itoa(end)
			}
			if metadataOnly {
				fmt.Fprintf(w, `{"kind":"PartialObjectMetadataList","apiVersion":"meta.k8s.io/v1","metadata":{"continue":%q},"items":[`, next)
			} else {
				fmt.Fprintf(w, `{"kind":"SecretList","apiVersion":"v1","metadata":{"continue":%q},"items":[`, next)
			}
			for i := start; i < end; i++ {
				if i > start {
					w.WriteByte(',')
				}
				if metadataOnly {
					w.WriteString(`{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1","metadata":`)
					meta(w, i)
					w.WriteByte('}')
				} else {
					secret(w, i)
				}
			}
			w.WriteString(`]}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/"):
			// /api/v1/namespaces/<ns>/secrets/sh.helm.release.v1.rel-<r>.v<rev>
			parts := strings.Split(r.URL.Path, "/")
			name := parts[len(parts)-1]
			var rel, rev int
			if _, err := fmt.Sscanf(name, "sh.helm.release.v1.rel-%d.v%d", &rel, &rev); err != nil || parts[len(parts)-2] != "secrets" {
				http.NotFound(rw, r)
				return
			}
			gets.Add(1)
			secret(w, rel*revisions+rev-1)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &gets
}

// realisticReleasePayload builds a Helm release payload whose stored
// (gzipped) size is about storedKiB: a compressible rendered manifest of
// about manifestKiB plus chart files of random bytes, which is what keeps
// real payloads large after compression (bundled CRDs, dashboards).
func realisticReleasePayload(t testing.TB, storedKiB, manifestKiB int) string {
	t.Helper()
	var manifest strings.Builder
	for i := 0; manifest.Len() < manifestKiB<<10; i++ {
		fmt.Fprintf(&manifest, "---\n# Source: big/templates/cm.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm-%d\ndata:\n  key: value-%d\n", i, i)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	files := make([]byte, storedKiB<<10)
	for i := range files {
		files[i] = byte(rng.IntN(256))
	}
	doc := fmt.Sprintf(`{"name":"big","info":{"status":"deployed"},"chart":{"metadata":{"name":"big","version":"1.0.0","appVersion":"1.0.0","kubeVersion":">=1.21.0-0"},"files":[{"name":"crds.yaml","data":%q}]},"manifest":%q}`,
		base64.StdEncoding.EncodeToString(files), manifest.String())
	for {
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		zw.Write([]byte(doc))
		zw.Close()
		if gz.Len() <= storedKiB<<10*21/20 { // within 5% of the target
			return base64.StdEncoding.EncodeToString(gz.Bytes())
		}
		// base64 of random bytes gzips to ~0.76 of its size; trim the files.
		cut := (gz.Len() - storedKiB<<10) * 4 / 3
		files = files[:len(files)-cut]
		doc = fmt.Sprintf(`{"name":"big","info":{"status":"deployed"},"chart":{"metadata":{"name":"big","version":"1.0.0","appVersion":"1.0.0","kubeVersion":">=1.21.0-0"},"files":[{"name":"crds.yaml","data":%q}]},"manifest":%q}`,
			base64.StdEncoding.EncodeToString(files), manifest.String())
	}
}

// peakHeap runs f while sampling the live+unswept heap every 200µs and
// returns the peak above the heap measured (after a GC) before f started.
func peakHeap(f func()) (peak uint64) {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	read := func() uint64 {
		metrics.Read(sample)
		return sample[0].Value.Uint64()
	}
	runtime.GC()
	base := read()
	var max atomic.Uint64
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(200 * time.Microsecond)
		defer tick.Stop()
		for {
			if v := read(); v > max.Load() {
				max.Store(v)
			}
			select {
			case <-done:
				return
			case <-tick.C:
			}
		}
	}()
	f()
	close(done)
	<-stopped
	if v := read(); v > max.Load() {
		max.Store(v)
	}
	if max.Load() < base {
		return 0
	}
	return max.Load() - base
}

// TestCollectHelmPeakHeapIsBoundedByOneRelease guards #24: the collector
// decoded every revision's full payload, 500 Secrets per page, so peak heap
// grew with the cluster's Helm history and OOM-killed the agent at its
// 256Mi limit. Only one release's payload may be in flight at a time now.
// Measured on this harness: 300×10×150KiB peaked at 279 MiB and
// 50×10×450KiB at 869–947 MiB before; 17 MiB and 8 MiB after.
//
// The manifest is parsed as in production, with every ConfigMap flagged so
// each release also keeps MaxObjectRefs object refs: the worst case for
// what the inventory retains. Under the race detector, which slows the
// decoding about tenfold, the first shape runs with 60 releases: 600
// Secrets still overflow the old collector's 500-Secret page.
func TestCollectHelmPeakHeapIsBoundedByOneRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("serves ~440 MiB of release data")
	}
	lifecycle := []kb.APILifecycleEntry{{Version: "v1", Kind: "ConfigMap", Deprecated: &inventory.Version{Major: 1, Minor: 99}}}
	cases := []struct {
		releases, revisions, storedKiB int
		maxPeakMiB                     uint64
	}{
		{releases: 300, revisions: 10, storedKiB: 150, maxPeakMiB: 64},
		{releases: 50, revisions: 10, storedKiB: 450, maxPeakMiB: 64}, // #24's acceptance shape
	}
	if raceEnabled {
		cases[0].releases = 60
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%dx%dx%dKiB", tc.releases, tc.revisions, tc.storedKiB), func(t *testing.T) {
			payload := realisticReleasePayload(t, tc.storedKiB, 32)
			srv, gets := helmAPIServer(t, tc.releases, tc.revisions, payload)
			c, err := NewClients(&rest.Config{Host: srv.URL, QPS: -1})
			if err != nil {
				t.Fatal(err)
			}
			var inv inventory.Inventory
			var cerr error
			peak := peakHeap(func() { cerr = collectHelm(context.Background(), c.Kube, c.Metadata, lifecycle, &inv) })
			if cerr != nil && !errors.As(cerr, new(partialError)) {
				t.Fatal(cerr)
			}
			t.Logf("stored payload %d KiB; %d releases found, %d Secret GETs; peak heap above baseline %.1f MiB",
				len(payload)*3/4>>10, len(inv.HelmReleases), gets.Load(), float64(peak)/(1<<20))
			if len(inv.HelmReleases) != tc.releases || gets.Load() != int64(tc.releases) {
				t.Errorf("%d releases from %d Secret GETs, want %d from %d (one per release)", len(inv.HelmReleases), gets.Load(), tc.releases, tc.releases)
			}
			if n := len(inv.HelmReleases[0].ManifestAPIs); n != 1 {
				t.Errorf("first release has %d manifest API rows, want 1 (the flagged ConfigMaps)", n)
			}
			if peak > tc.maxPeakMiB<<20 {
				t.Errorf("peak heap %.1f MiB, want ≤ %d MiB", float64(peak)/(1<<20), tc.maxPeakMiB)
			}
		})
	}
}
