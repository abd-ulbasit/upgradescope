package crd

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"

	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/suppress"
)

func TestReadSpecIgnore(t *testing.T) {
	cr := newCRObject(DefaultName)
	cr.Object["spec"] = map[string]interface{}{"ignore": []interface{}{
		map[string]interface{}{"key": "eol-addon/ingress-nginx", "reason": "migrating in Q1", "expires": "2026-12-31"},
		map[string]interface{}{"category": "removed-api", "namespace": "legacy-*", "reason": "retired"},
	}}
	got, _, _, err := ReadSpec(context.Background(), newDynFake(cr), DefaultName)
	if err != nil {
		t.Fatal(err)
	}
	want := []suppress.Rule{
		{Key: "eol-addon/ingress-nginx", Reason: "migrating in Q1", Expires: "2026-12-31"},
		{Category: "removed-api", Namespace: "legacy-*", Reason: "retired"},
	}
	if !reflect.DeepEqual(got.Ignore, want) {
		t.Errorf("Ignore = %+v, want %+v", got.Ignore, want)
	}
}

// The CRD schema rejects what the apiserver can check without CEL: a rule
// without a reason, or an expires that is not a date.
func TestManifestIgnoreSchema(t *testing.T) {
	def := parseManifest(t)
	raw, err := json.Marshal(def.Spec.Versions[0].Schema.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}
	var crSchema spec.Schema
	if err := json.Unmarshal(raw, &crSchema); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		rule  map[string]interface{}
		valid bool
	}{
		"valid":       {map[string]interface{}{"key": "eol-addon/ingress-nginx", "reason": "r", "expires": "2026-12-31"}, true},
		"no reason":   {map[string]interface{}{"key": "eol-addon/ingress-nginx"}, false},
		"empty":       {map[string]interface{}{"key": "eol-addon/ingress-nginx", "reason": ""}, false},
		"bad expires": {map[string]interface{}{"key": "eol-addon/ingress-nginx", "reason": "r", "expires": "Dec 31"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			obj := newCRObject(DefaultName).Object
			obj["spec"] = map[string]interface{}{"ignore": []interface{}{tc.rule}}
			err := validate.AgainstSchema(&crSchema, obj, strfmt.Default)
			if (err == nil) != tc.valid {
				t.Errorf("validation err = %v, want valid=%v", err, tc.valid)
			}
		})
	}
}

func TestTargetStatusFromReportCountsSuppressed(t *testing.T) {
	r := engine.Report{Suppressed: []engine.SuppressedFinding{{Finding: engine.Finding{Category: engine.CatEOLAddon}, Reason: "r"}}}
	if got := TargetStatusFromReport(r).Suppressed; got != 1 {
		t.Errorf("Suppressed = %d, want 1", got)
	}
}
