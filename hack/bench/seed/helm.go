package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Release sizes. A real release stores the chart (its templates, bundled
// files and default values) beside the manifest it applied, so the stored
// payload is far larger than the objects it deploys. The collector's own
// measurements (internal/collect/helm.go): kube-prometheus-stack 2.6 MiB of
// JSON, cert-manager 1.7 MiB, istiod 0.4 MiB, each gzipping 7-9x. The mix
// below is a plausible production cluster: most releases are small
// operators and apps, a few are the big platform charts.
type sizeClass struct {
	name    string
	objects int // rendered objects in the manifest
	files   int // bundled chart files (CRDs, dashboards), as base64 of text
	every   int // class chosen when i%100 < every share
}

var (
	classSmall  = sizeClass{"small", 12, 1, 80}
	classMedium = sizeClass{"medium", 60, 4, 95}
	classLarge  = sizeClass{"large", 300, 20, 100}
)

// classOf picks the size class of release i: 80% small, 15% medium, 5% large.
func classOf(i int) sizeClass {
	switch p := i % 100; {
	case p < classSmall.every:
		return classSmall
	case p < classMedium.every:
		return classMedium
	default:
		return classLarge
	}
}

// helmRelease is one release's Secrets, ready to create.
type helmRelease struct {
	secrets  []*corev1.Secret
	class    string
	jsonSize int // the installed revision's decompressed JSON
	gzSize   int // the installed revision's stored (base64 of gzip) payload
}

// releaseName and releaseNamespace spread releases over the namespaces.
func releaseName(i int) string { return fmt.Sprintf("rel-%04d", i) }

// deprecatedEvery makes 1 release in N hold an object at an API the
// knowledge base flags (PodSecurityPolicy, removed in 1.25), so the
// manifest scan has findings to record.
const deprecatedEvery = 50

func helmReleaseSecrets(i int, cfg config) (helmRelease, error) {
	class := classOf(i)
	ns := nsName(i % max(cfg.Namespaces, 1))
	name := releaseName(i)
	revs := max(cfg.HelmRevisions, 1)
	out := helmRelease{class: class.name}
	for rev := 1; rev <= revs; rev++ {
		rng := rand.New(rand.NewPCG(cfg.Seed, uint64(i)*1000+uint64(rev)))
		status := "deployed"
		if rev < revs {
			status = "superseded"
		}
		doc := releaseDoc(name, ns, rev, status, class, rng, i%deprecatedEvery == 0)
		raw, err := json.Marshal(doc)
		if err != nil {
			return out, err
		}
		payload, err := encodeRelease(raw)
		if err != nil {
			return out, err
		}
		if rev == revs {
			out.jsonSize, out.gzSize = len(raw), len(payload)
		}
		out.secrets = append(out.secrets, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, rev),
				Namespace: ns,
				Labels: map[string]string{
					"owner": "helm", "name": name, "status": status, "version": fmt.Sprint(rev),
					benchLabel: "true",
				},
			},
			Type: corev1.SecretType("helm.sh/release.v1"),
			// Helm stores base64(gzip(JSON)) in the value; the API's own
			// base64 of the field is the client's.
			Data: map[string][]byte{"release": payload},
		})
	}
	return out, nil
}

// encodeRelease is Helm's storage encoding: base64 of gzip of the JSON.
func encodeRelease(raw []byte) ([]byte, error) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return []byte(base64.StdEncoding.EncodeToString(gz.Bytes())), nil
}

type chartFile struct {
	Name string `json:"name"`
	Data string `json:"data"` // base64
}

func releaseDoc(name, ns string, rev int, status string, class sizeClass, rng *rand.Rand, flagged bool) map[string]any {
	var manifest strings.Builder
	var templates []chartFile
	for o := range class.objects {
		obj := renderObject(name, o, rng)
		fmt.Fprintf(&manifest, "---\n# Source: %s/templates/obj-%d.yaml\n%s", name, o, obj)
		// The chart's template is the object with its values templated in.
		tpl := strings.ReplaceAll(obj, name, `{{ include "chart.fullname" . }}`)
		templates = append(templates, chartFile{Name: fmt.Sprintf("templates/obj-%d.yaml", o), Data: base64.StdEncoding.EncodeToString([]byte(tpl))})
	}
	if flagged {
		fmt.Fprintf(&manifest, "---\n# Source: %s/templates/psp.yaml\napiVersion: policy/v1beta1\nkind: PodSecurityPolicy\nmetadata:\n  name: %s-psp\nspec:\n  privileged: false\n  runAsUser:\n    rule: RunAsAny\n", name, name)
	}
	var files []chartFile
	for f := range class.files {
		files = append(files, chartFile{Name: fmt.Sprintf("crds/crd-%d.yaml", f), Data: base64.StdEncoding.EncodeToString([]byte(renderCRD(name, f, rng)))})
	}
	when := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(rng.IntN(30*24)) * time.Hour)
	return map[string]any{
		"name": name,
		"info": map[string]any{
			"first_deployed": when.Format(time.RFC3339Nano), "last_deployed": when.Format(time.RFC3339Nano),
			"deleted": "", "description": "Install complete", "status": status,
		},
		"chart": map[string]any{
			"metadata": map[string]any{
				"name": "chart-" + name, "version": fmt.Sprintf("%d.%d.%d", 1+rng.IntN(5), rng.IntN(30), rng.IntN(10)),
				"appVersion":  fmt.Sprintf("v%d.%d.%d", rng.IntN(3), rng.IntN(20), rng.IntN(10)),
				"kubeVersion": ">=1.21.0-0", "description": "A benchmark chart", "apiVersion": "v2",
			},
			"templates": templates,
			"files":     files,
			"values":    map[string]any{"replicaCount": 2, "image": map[string]any{"repository": "ghcr.io/example/" + name, "tag": "v1"}},
		},
		"manifest":  manifest.String(),
		"version":   rev,
		"namespace": ns,
	}
}

// renderObject is one rendered manifest object: a Deployment, Service,
// ConfigMap, ServiceAccount or Role, with the per-release names, hashes and
// numbers a real render has.
func renderObject(release string, o int, rng *rand.Rand) string {
	n := fmt.Sprintf("%s-%d", release, o)
	switch o % 5 {
	case 0:
		port := strconv.Itoa(8000 + rng.IntN(1000))
		return strings.NewReplacer(
			"@NAME@", n, "@RELEASE@", release, "@REPLICAS@", strconv.Itoa(1+rng.IntN(5)),
			"@SUM@", fmt.Sprintf("%016x%016x", rng.Uint64(), rng.Uint64()),
			"@IMAGE@", fmt.Sprintf("v%d.%d.%d", rng.IntN(3), rng.IntN(20), rng.IntN(10)), "@PORT@", port,
			"@CPUREQ@", strconv.Itoa(50+rng.IntN(200)), "@MEMREQ@", strconv.Itoa(64+rng.IntN(512)),
			"@CPULIM@", strconv.Itoa(250+rng.IntN(500)), "@MEMLIM@", strconv.Itoa(256+rng.IntN(1024)),
		).Replace(deploymentTemplate)
	case 1:
		return fmt.Sprintf("apiVersion: v1\nkind: Service\nmetadata:\n  name: %s\nspec:\n  type: ClusterIP\n  ports:\n  - port: %d\n    targetPort: http\n  selector:\n    app.kubernetes.io/name: %s\n", n, 80+rng.IntN(9000), n)
	case 2:
		return fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\ndata:\n  config.yaml: |\n    server:\n      port: %d\n      timeout: %ds\n    upstream: https://svc-%d.internal:%d\n    cache: {ttl: %ds, size: %d}\n", n, 8000+rng.IntN(1000), 1+rng.IntN(60), rng.IntN(500), 8000+rng.IntN(1000), 10+rng.IntN(600), 100+rng.IntN(10000))
	case 3:
		return fmt.Sprintf("apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: %s\nautomountServiceAccountToken: true\n", n)
	default:
		return fmt.Sprintf("apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: %s\nrules:\n- apiGroups: [\"\"]\n  resources: [configmaps, secrets, pods]\n  verbs: [get, list, watch]\n", n)
	}
}

// renderCRD is a bundled CRD-sized file: a long schema of repetitive but
// not identical properties, what keeps real payloads large after gzip.
func renderCRD(release string, f int, rng *rand.Rand) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: kind%d.%s.example.com\nspec:\n  group: %s.example.com\n  names: {kind: Kind%d, plural: kind%ds}\n  scope: Namespaced\n  versions:\n  - name: v1\n    served: true\n    storage: true\n    schema:\n      openAPIV3Schema:\n        type: object\n        properties:\n          spec:\n            type: object\n            properties:\n", f, release, release, f, f)
	for p := range 120 {
		fmt.Fprintf(&b, "              field%d:\n                type: string\n                description: %016x%016x controls how the controller treats item %d\n                maxLength: %d\n", p, rng.Uint64(), rng.Uint64(), p, 16+rng.IntN(240))
	}
	return b.String()
}

// deploymentTemplate is the Deployment a rendered chart typically holds.
const deploymentTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: @NAME@
  labels:
    app.kubernetes.io/name: @NAME@
    app.kubernetes.io/instance: @RELEASE@
    app.kubernetes.io/managed-by: Helm
spec:
  replicas: @REPLICAS@
  selector:
    matchLabels:
      app.kubernetes.io/name: @NAME@
  template:
    metadata:
      annotations:
        checksum/config: @SUM@
      labels:
        app.kubernetes.io/name: @NAME@
    spec:
      serviceAccountName: @NAME@
      containers:
      - name: main
        image: ghcr.io/example/@NAME@:@IMAGE@
        args: ["--listen=:@PORT@", "--log-level=info", "--config=/etc/@NAME@/config.yaml"]
        ports:
        - containerPort: @PORT@
          name: http
        readinessProbe:
          httpGet: {path: /ready, port: http}
          periodSeconds: 10
        resources:
          requests: {cpu: @CPUREQ@m, memory: @MEMREQ@Mi}
          limits: {cpu: @CPULIM@m, memory: @MEMLIM@Mi}
`
