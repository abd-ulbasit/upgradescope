package chart

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const extraRegistryValues = `agent:
  extraRegistry:
    acme-gateway.yaml: |
      schema_version: 2
      id: acme-gateway
      display_name: Acme Gateway
      matchers:
        images:
          - acme/gateway
      support:
        status: eol
        citations:
          - https://acme.dev/gateway/lifecycle
`

// #49: agent.extraRegistry renders a ConfigMap of registry entries, mounts
// it, and points --registry-dir at it; the pod rolls when it changes. Unset,
// none of it renders.
func TestAgentExtraRegistry(t *testing.T) {
	values := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(values, []byte(extraRegistryValues), 0o600); err != nil {
		t.Fatal(err)
	}
	objs := render(t, "-f="+values)

	cm := find(objs, "ConfigMap", "upgradescope-agent-registry")
	if cm == nil {
		t.Fatalf("no registry ConfigMap rendered (have %v)", kinds(objs))
	}
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	if !strings.Contains(data["acme-gateway.yaml"], "id: acme-gateway") {
		t.Errorf("ConfigMap data = %v, want the entry under its file name", data)
	}

	c := container(t, objs, "upgradescope-agent")
	if !slices.Contains(args(c), "--registry-dir=/etc/upgradescope/registry") {
		t.Errorf("args = %v, want --registry-dir=/etc/upgradescope/registry", args(c))
	}
	mounts, _, _ := unstructured.NestedSlice(c, "volumeMounts")
	var mounted bool
	for _, m := range mounts {
		mm := m.(map[string]any)
		mounted = mounted || mm["mountPath"] == "/etc/upgradescope/registry" && mm["readOnly"] == true
	}
	if !mounted {
		t.Errorf("volumeMounts = %v, want the ConfigMap mounted read-only at /etc/upgradescope/registry", mounts)
	}
	d := find(objs, "Deployment", "upgradescope-agent")
	vols, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "volumes")
	var hasVolume bool
	for _, v := range vols {
		name, _, _ := unstructured.NestedString(v.(map[string]any), "configMap", "name")
		hasVolume = hasVolume || name == "upgradescope-agent-registry"
	}
	if !hasVolume {
		t.Errorf("volumes = %v, want the registry ConfigMap", vols)
	}
	if sum, _, _ := unstructured.NestedString(d.Object, "spec", "template", "metadata", "annotations", "checksum/registry"); sum == "" {
		t.Error("no checksum/registry annotation: the agent reads --registry-dir at startup, so the pod must roll when it changes")
	}

	// Unset: nothing renders and the flag is not passed.
	plain := render(t)
	if find(plain, "ConfigMap", "upgradescope-agent-registry") != nil || slices.ContainsFunc(args(container(t, plain, "upgradescope-agent")), func(a string) bool { return strings.HasPrefix(a, "--registry-dir") }) {
		t.Error("extraRegistry unset still renders the registry ConfigMap or flag")
	}
}

// A key that is not <id>.yaml could never load (the agent would fail to
// start), so the schema refuses it at install time.
func TestAgentExtraRegistryKeysMustBeYAMLFiles(t *testing.T) {
	if out := renderErr(t, "agent.extraRegistry.acme-gateway\\.yaml=schema_version: 2"); out != "" {
		t.Errorf("a <id>.yaml key must render: %s", out)
	}
	for _, key := range []string{"acme-gateway.yml", "Acme.yaml", "notes.txt"} {
		if out := renderErr(t, "agent.extraRegistry."+strings.ReplaceAll(key, ".", "\\.")+"=x"); out == "" {
			t.Errorf("key %q rendered, want a schema error", key)
		}
	}
}
