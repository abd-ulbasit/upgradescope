package inventory

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestInventoryJSONRoundTrip(t *testing.T) {
	inv := Inventory{
		SchemaVersion: 1,
		ClusterID:     "8f2a1b3c-kube-system-uid",
		CollectedAt:   time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		ServerVersion: "v1.34.2",
		Capabilities: map[Capability]CapabilityStatus{
			CapAPIUsage: {Available: true},
			CapHelm:     {Available: false, Reason: "secrets list forbidden"},
		},
		APIUsage: []APIUsage{{
			Group:      "policy",
			Version:    "v1beta1",
			Kind:       "PodSecurityPolicy",
			Count:      3,
			Namespaces: map[string]int{"": 3},
		}},
		DeprecatedCalls: []DeprecatedCall{{
			Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3",
			Resource: "flowschemas", RemovedRelease: "1.32",
		}},
		HelmReleases: []HelmRelease{{
			Name: "ingress-nginx", Namespace: "ingress-nginx",
			ChartName: "ingress-nginx", ChartVersion: "4.7.1",
			AppVersion: "1.8.1", Status: "deployed",
		}},
		AddOns: []AddOnInstance{{
			ID: "ingress-nginx", Version: "1.8.1",
			Namespaces: []string{"ingress-nginx"}, Source: "image",
		}},
		Nodes:              []NodeInfo{{Name: "node-1", KubeletVersion: "v1.33.1"}},
		Namespaces:         []NamespaceInfo{{Name: "payments", Team: "payments-team"}},
		UnrecognizedImages: []string{"registry.example.com/internal/app"},
	}

	data, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got Inventory
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, inv) {
		t.Errorf("round-trip mismatch:\ngot:  %+v\nwant: %+v", got, inv)
	}
}

func TestInventoryOmitEmpty(t *testing.T) {
	inv := Inventory{
		SchemaVersion: 1,
		ClusterID:     "files",
		CollectedAt:   time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		Capabilities:  map[Capability]CapabilityStatus{},
		APIUsage:      []APIUsage{}, // empty (non-nil) slice must also be omitted
	}

	data, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	absent := []string{
		`"serverVersion"`, `"apiUsage"`, `"deprecatedCalls"`, `"helmReleases"`,
		`"addOns"`, `"nodes"`, `"namespaces"`, `"unrecognizedImages"`,
	}
	for _, key := range absent {
		if bytes.Contains(data, []byte(key)) {
			t.Errorf("empty field %s present in JSON: %s", key, data)
		}
	}

	present := []string{`"schemaVersion"`, `"clusterId"`, `"collectedAt"`, `"capabilities"`}
	for _, key := range present {
		if !bytes.Contains(data, []byte(key)) {
			t.Errorf("required field %s missing from JSON: %s", key, data)
		}
	}
}
