package crd

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestConstantsMatchManifest(t *testing.T) {
	c := parseManifest(t)
	if c.Spec.Group != Group {
		t.Errorf("Group const %q != manifest group %q", Group, c.Spec.Group)
	}
	if c.Spec.Names.Plural != Plural || c.Spec.Names.Singular != Singular || c.Spec.Names.Kind != Kind {
		t.Errorf("names consts (%s/%s/%s) != manifest names %+v", Plural, Singular, Kind, c.Spec.Names)
	}
	if c.Spec.Versions[0].Name != Version {
		t.Errorf("Version const %q != manifest version %q", Version, c.Spec.Versions[0].Name)
	}
	if DefaultName != "cluster" {
		t.Errorf("DefaultName = %q, want cluster", DefaultName)
	}
}

func TestGVR(t *testing.T) {
	want := schema.GroupVersionResource{Group: "upgradescope.dev", Version: "v1alpha1", Resource: "clusterreadinesses"}
	if got := GVR(); got != want {
		t.Errorf("GVR() = %v, want %v", got, want)
	}
}

func TestStatusJSONRoundTrip(t *testing.T) {
	st := Status{
		ObservedServerVersion: "v1.35.2",
		KBVersion:             "k8s-1.36+registry-2026-06-10",
		LastEvaluated:         metav1.NewTime(time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)),
		Targets: []TargetStatus{{
			Target:     "1.36",
			Score:      87,
			Ready:      false,
			Blockers:   1,
			Warnings:   2,
			Infos:      3,
			ByCategory: map[string]int{"removed-api": 1, "eol-approaching": 2},
			TopFindings: []TopFinding{{
				Category:    "removed-api",
				Severity:    "blocker",
				Title:       "flowcontrol.apiserver.k8s.io/v1beta3 FlowSchema removed in 1.32",
				Remediation: "migrate to flowcontrol.apiserver.k8s.io/v1",
			}},
		}},
		NotAssessed:  []string{"helm: secrets list forbidden"},
		AgentVersion: "v0.2.0",
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"observedServerVersion"`, `"kbVersion"`, `"lastEvaluated"`, `"targets"`,
		`"target"`, `"score"`, `"ready"`, `"blockers"`, `"warnings"`, `"infos"`,
		`"byCategory"`, `"topFindings"`, `"category"`, `"severity"`, `"title"`,
		`"remediation"`, `"notAssessed"`, `"agentVersion"`,
	} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("marshaled status missing key %s in %s", key, raw)
		}
	}
	var back Status
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// metav1.Time.UnmarshalJSON converts to time.Local; compare the instant,
	// then normalize the location for the struct-wide DeepEqual.
	if !back.LastEvaluated.Time.Equal(st.LastEvaluated.Time) {
		t.Errorf("LastEvaluated round trip = %v, want %v", back.LastEvaluated, st.LastEvaluated)
	}
	back.LastEvaluated = st.LastEvaluated
	if !reflect.DeepEqual(st, back) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", back, st)
	}
}

func TestSpecJSONOmitsEmptyTargets(t *testing.T) {
	raw, err := json.Marshal(Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{}" {
		t.Errorf("empty Spec marshals to %s, want {}", raw)
	}
}
