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

// TestInventoryWireFormat pins the exact JSON wire form of a fully-populated
// Inventory. This is the agent→server contract: any field rename, tag change,
// or type-level marshaling change must fail this test loudly and be a
// deliberate, versioned decision (bump SchemaVersion).
func TestInventoryWireFormat(t *testing.T) {
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
			Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy",
			Count: 3, Namespaces: map[string]int{"": 1, "legacy": 2},
		}},
		DeprecatedCalls: []DeprecatedCall{{
			Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3",
			Resource: "flowschemas", Subresource: "status", RemovedRelease: "1.32",
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

	want := `{
  "schemaVersion": 1,
  "clusterId": "8f2a1b3c-kube-system-uid",
  "collectedAt": "2026-06-10T12:00:00Z",
  "serverVersion": "v1.34.2",
  "capabilities": {
    "api-usage": {
      "available": true
    },
    "helm": {
      "available": false,
      "reason": "secrets list forbidden"
    }
  },
  "apiUsage": [
    {
      "group": "policy",
      "version": "v1beta1",
      "kind": "PodSecurityPolicy",
      "count": 3,
      "namespaces": {
        "": 1,
        "legacy": 2
      }
    }
  ],
  "deprecatedCalls": [
    {
      "group": "flowcontrol.apiserver.k8s.io",
      "version": "v1beta3",
      "resource": "flowschemas",
      "subresource": "status",
      "removedRelease": "1.32"
    }
  ],
  "helmReleases": [
    {
      "name": "ingress-nginx",
      "namespace": "ingress-nginx",
      "chartName": "ingress-nginx",
      "chartVersion": "4.7.1",
      "appVersion": "1.8.1",
      "status": "deployed"
    }
  ],
  "addOns": [
    {
      "id": "ingress-nginx",
      "version": "1.8.1",
      "namespaces": [
        "ingress-nginx"
      ],
      "source": "image"
    }
  ],
  "nodes": [
    {
      "name": "node-1",
      "kubeletVersion": "v1.33.1"
    }
  ],
  "namespaces": [
    {
      "name": "payments",
      "team": "payments-team"
    }
  ],
  "unrecognizedImages": [
    "registry.example.com/internal/app"
  ]
}`

	got, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	if string(got) != want {
		t.Errorf("Inventory wire format drifted — this is the agent->server contract.\ngot:\n%s\nwant:\n%s", got, want)
	}
}
