package inventory

import (
	"errors"
	"strings"
	"testing"
)

// validInventory carries every kind of identifier ValidateIdentifiers
// checks, each valid, the empty ones included.
func validInventory() Inventory {
	usage := func() APIUsage {
		return APIUsage{
			Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy", Count: 3,
			Namespaces: map[string]int{"": 1, "team-a": 2},
			Objects: []ObjectRef{
				{Name: "system:node:worker-1"}, // RBAC names take ':'
				{Namespace: "team-a", Name: "web.example.com", Manager: "m <&'\">"},
				{Namespace: "team-a"}, // no name known
			},
		}
	}
	return Inventory{
		SchemaVersion:        1,
		APIUsage:             []APIUsage{usage()},
		APIAuthorshipUnknown: []APIUsage{usage()},
		HelmReleases:         []HelmRelease{{Name: "ingress-nginx", Namespace: "ingress", ManifestAPIs: []APIUsage{usage()}}},
		AddOns:               []AddOnInstance{{ID: "ingress-nginx", Namespaces: []string{"ingress"}}, {ID: "x"}},
		Nodes:                []NodeInfo{{Name: "ip-10-0-0-1.ec2.internal"}},
		ControlPlane:         []ComponentVersion{{Component: "kube-proxy", Version: "v1.33.0", Node: "ip-10-0-0-1.ec2.internal"}, {Component: "kube-apiserver", Version: "v1.33.0"}},
		Namespaces:           []NamespaceInfo{{Name: "team-a", Team: "Payments_Team.1"}, {Name: "kube-system"}},
		CRDs:                 []CRD{{Group: "example.com", Kind: "Widget", Usage: []APIUsage{usage()}}},
	}
}

func TestValidateIdentifiersAcceptsValid(t *testing.T) {
	if err := validInventory().ValidateIdentifiers(); err != nil {
		t.Fatalf("ValidateIdentifiers() = %v, want nil", err)
	}
	if err := (Inventory{}).ValidateIdentifiers(); err != nil {
		t.Fatalf("empty inventory: %v", err)
	}
	long := Inventory{
		APIUsage: []APIUsage{{
			Namespaces: map[string]int{strings.Repeat("a", 63): 1},
			Objects:    []ObjectRef{{Namespace: strings.Repeat("a", 63), Name: strings.Repeat("n", 253)}},
		}},
		Nodes: []NodeInfo{{Name: strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)}},
	}
	if err := long.ValidateIdentifiers(); err != nil {
		t.Fatalf("identifiers at their length limits: %v", err)
	}
}

func TestValidateIdentifiersRefusesInvalid(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*Inventory)
		field string
	}{
		{"namespace key of apostrophes", func(inv *Inventory) {
			inv.APIUsage[0].Namespaces[strings.Repeat("'", 190)] = 1
		}, "apiUsage[0].namespaces"},
		{"namespace key too long", func(inv *Inventory) {
			inv.APIUsage[0].Namespaces[strings.Repeat("a", 64)] = 1
		}, "apiUsage[0].namespaces"},
		{"namespace key with a dot", func(inv *Inventory) { inv.APIUsage[0].Namespaces["a.b"] = 1 }, "apiUsage[0].namespaces"},
		{"upper-case namespace", func(inv *Inventory) { inv.APIUsage[0].Objects[1].Namespace = "Team-A" }, "apiUsage[0].objects[1].namespace"},
		{"object name with a slash", func(inv *Inventory) { inv.APIUsage[0].Objects[0].Name = "a/b" }, "apiUsage[0].objects[0].name"},
		{"object name ..", func(inv *Inventory) { inv.APIUsage[0].Objects[0].Name = ".." }, "apiUsage[0].objects[0].name"},
		{"object name with %", func(inv *Inventory) { inv.APIUsage[0].Objects[0].Name = "a%2f" }, "apiUsage[0].objects[0].name"},
		{"object name too long", func(inv *Inventory) { inv.APIUsage[0].Objects[0].Name = strings.Repeat("n", 254) }, "apiUsage[0].objects[0].name"},
		{"authorship unknown object", func(inv *Inventory) { inv.APIAuthorshipUnknown[0].Objects[0].Name = "a/b" }, "apiAuthorshipUnknown[0].objects[0].name"},
		{"helm release name", func(inv *Inventory) { inv.HelmReleases[0].Name = "Ingress" }, "helmReleases[0].name"},
		{"helm release namespace", func(inv *Inventory) { inv.HelmReleases[0].Namespace = "<x>" }, "helmReleases[0].namespace"},
		{"helm manifest object", func(inv *Inventory) {
			inv.HelmReleases[0].ManifestAPIs[0].Objects[1].Namespace = "a_b"
		}, "helmReleases[0].manifestApis[0].objects[1].namespace"},
		{"add-on namespace", func(inv *Inventory) { inv.AddOns[0].Namespaces = append(inv.AddOns[0].Namespaces, "-x") }, "addOns[0].namespaces[1]"},
		{"node name", func(inv *Inventory) { inv.Nodes[0].Name = "node 1" }, "nodes[0].name"},
		{"kube-proxy node name", func(inv *Inventory) { inv.ControlPlane[0].Node = "node 1" }, "controlPlane[0].node"},
		{"namespace list name", func(inv *Inventory) { inv.Namespaces[1].Name = strings.Repeat("k", 64) }, "namespaces[1].name"},
		{"team label value", func(inv *Inventory) { inv.Namespaces[0].Team = "payments team" }, "namespaces[0].team"},
		{"team label value too long", func(inv *Inventory) { inv.Namespaces[0].Team = strings.Repeat("t", 64) }, "namespaces[0].team"},
		{"CRD usage namespace key", func(inv *Inventory) { inv.CRDs[0].Usage[0].Namespaces[" "] = 1 }, "crds[0].usage[0].namespaces"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := validInventory()
			tc.edit(&inv)
			err := inv.ValidateIdentifiers()
			var idErr *IdentifierError
			if !errors.As(err, &idErr) || idErr.Field != tc.field {
				t.Fatalf("ValidateIdentifiers() = %v, want an IdentifierError at %s", err, tc.field)
			}
			if !strings.HasPrefix(err.Error(), tc.field+": ") || len(idErr.Problems) == 0 || idErr.Rule == "" {
				t.Fatalf("message %q: want the field path, the rule and why", err)
			}
		})
	}
}

// A refused identifier can be megabytes: the message quotes 64 bytes of
// it and says how long it is.
func TestIdentifierErrorQuotesAShortPrefix(t *testing.T) {
	inv := Inventory{APIUsage: []APIUsage{{Namespaces: map[string]int{strings.Repeat("é", 1<<20): 1}}}}
	err := inv.ValidateIdentifiers()
	if err == nil || len(err.Error()) > 1024 || !strings.Contains(err.Error(), "(2097152 bytes)") {
		t.Fatalf("message (%d bytes) = %.300q, want a short one naming the length", len(err.Error()), err)
	}
}

// Of several invalid namespace keys the least is named, whatever the map
// order.
func TestValidateIdentifiersIsDeterministic(t *testing.T) {
	keys := map[string]int{"ok": 1}
	for _, k := range []string{"Z", "Y", "B", "X", "C", "D"} {
		keys[k] = 1
	}
	for range 20 {
		err := Inventory{APIUsage: []APIUsage{{Namespaces: keys}}}.ValidateIdentifiers()
		var idErr *IdentifierError
		if !errors.As(err, &idErr) || idErr.Value != "B" {
			t.Fatalf("ValidateIdentifiers() = %v, want the least invalid key \"B\"", err)
		}
	}
}

func TestValidateClusterName(t *testing.T) {
	for _, ok := range []string{"prod-eu-1", "a", "prod.eu.example.com", "3f2a9c1e-0b7d-4c55-9a1e-2f4d6b8c0e11", strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)} {
		if err := ValidateClusterName(ok); err != nil {
			t.Errorf("ValidateClusterName(%.40q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "../<script>x", "Prod", "prod eu", "prod_eu", "-prod", "prod.", strings.Repeat("a", 254)} {
		err := ValidateClusterName(bad)
		var idErr *IdentifierError
		if !errors.As(err, &idErr) || idErr.Field != "clusterName" {
			t.Errorf("ValidateClusterName(%.40q) = %v, want an IdentifierError at clusterName", bad, err)
		}
	}
}
