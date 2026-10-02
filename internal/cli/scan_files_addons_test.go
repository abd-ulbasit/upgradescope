package cli

import (
	"strings"
	"testing"
)

// ingressNginxDeployment is a rendered ingress-nginx v1.8.1 controller
// Deployment with an init container, as `helm template` writes it.
const ingressNginxDeployment = `# Source: ingress-nginx/templates/controller-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ingress-nginx-controller
  namespace: ingress-nginx
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: ingress-nginx
    spec:
      initContainers:
        - name: init
          image: busybox:1.36
      containers:
        - name: controller
          image: registry.k8s.io/ingress-nginx/controller:v1.8.1
`

// #47: `scan --files` (the CI gate) judges the add-ons a render runs, with
// the embedded registry: an EOL ingress-nginx controller blocks, add-ons
// are no longer listed as not assessed, and the image no registry entry
// knows is listed.
func TestScanFilesAssessesAddOns(t *testing.T) {
	dir := writeFiles(t, map[string]string{"rendered/controller.yaml": ingressNginxDeployment})
	out, _, err := execScanFiles(t, "--files", dir)
	if ExitCode(err) != 2 {
		t.Fatalf("err = %v, want exit 2 (blocked)\n%s", err, out)
	}
	for _, want := range []string{
		"READY  no\n",
		"[eol-addon] Ingress NGINX Controller is end-of-life since 2026-03-24\n",
		"Detected Ingress NGINX Controller version 1.8.1 via image in namespace(s): ingress-nginx.",
		"UNRECOGNIZED IMAGES (1)\n",
		"  - docker.io/library/busybox\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "addons: files mode") {
		t.Errorf("add-ons still listed as not assessed:\n%s", out)
	}
	for _, want := range []string{"deprecated-calls: files mode", "helm: files mode", "versions: files mode"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks gap %q:\n%s", want, out)
		}
	}
}
