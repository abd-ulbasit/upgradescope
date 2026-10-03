package crd

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// The Argo CD health check the GitOps guide ships is run here, under
// gopher-lua (the Lua engine Argo CD embeds), over ClusterReadiness objects
// the agent's own WriteStatus produced. The script is read out of the
// guide, not copied, so the test cannot drift from what users paste.

const (
	argoGuide     = "../../docs/guides/gitops-argo-flux.md"
	argoHealthKey = "resource.customizations.health.upgradescope.dev_ClusterReadiness"
	argoLibsKey   = "resource.customizations.useOpenLibs.upgradescope.dev_ClusterReadiness"
)

// argoConfig returns the argocd-cm data the guide documents: the YAML
// fence that holds the health script's key.
func argoConfig(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(argoGuide)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	key := -1
	for i, l := range lines {
		if strings.Contains(l, argoHealthKey) {
			key = i
			break
		}
	}
	if key < 0 {
		t.Fatalf("%s does not document %s", argoGuide, argoHealthKey)
	}
	start, end := key, key
	for start > 0 && !strings.HasPrefix(lines[start], "```") {
		start--
	}
	for end < len(lines) && !strings.HasPrefix(lines[end], "```") {
		end++
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := yaml.Unmarshal([]byte(strings.Join(lines[start+1:end], "\n")), &cm); err != nil {
		t.Fatalf("the guide's argocd-cm snippet is not YAML: %v", err)
	}
	return cm.Data
}

// luaValue converts decoded JSON the way Argo CD converts the resource it
// hands a health script: objects and arrays become tables.
func luaValue(l *lua.LState, v any) lua.LValue {
	switch v := v.(type) {
	case map[string]any:
		t := l.NewTable()
		for k, e := range v {
			t.RawSetString(k, luaValue(l, e))
		}
		return t
	case []any:
		t := l.NewTable()
		for _, e := range v {
			t.Append(luaValue(l, e))
		}
		return t
	case string:
		return lua.LString(v)
	case float64:
		return lua.LNumber(v)
	case bool:
		return lua.LBool(v)
	}
	return lua.LNil
}

// argoHealth runs the script as Argo CD does: the open libraries on (the
// guide sets useOpenLibs), os reduced to time, the object as the global
// obj, and the script's returned table as the health.
func argoHealth(t *testing.T, script string, now time.Time, obj map[string]any) (status, message string) {
	t.Helper()
	l := lua.NewState(lua.Options{SkipOpenLibs: false})
	defer l.Close()
	osLib := l.NewTable()
	osLib.RawSetString("time", l.NewFunction(func(l *lua.LState) int {
		l.Push(lua.LNumber(now.Unix()))
		return 1
	}))
	l.SetGlobal("os", osLib)
	l.SetGlobal("obj", luaValue(l, obj))
	fn, err := l.LoadString(script)
	if err != nil {
		t.Fatalf("health script does not compile: %v", err)
	}
	l.Push(fn)
	if err := l.PCall(0, 1, nil); err != nil {
		t.Fatalf("health script failed: %v", err)
	}
	hs, ok := l.Get(-1).(*lua.LTable)
	if !ok {
		t.Fatalf("health script returned %s, want a table", l.Get(-1).Type())
	}
	return hs.RawGetString("status").String(), hs.RawGetString("message").String()
}

func TestArgoHealthScriptAgainstWrittenStatus(t *testing.T) {
	cfg := argoConfig(t)
	if cfg[argoLibsKey] != "true" {
		t.Fatalf("the guide must set %s to \"true\": the script needs string, math and os", argoLibsKey)
	}
	script := cfg[argoHealthKey]
	if strings.TrimSpace(script) == "" {
		t.Fatalf("the guide's %s is empty", argoHealthKey)
	}

	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	ready := []TargetStatus{{Target: "1.36", Score: 100, Ready: true, Verdict: "ready"}}
	blocked := []TargetStatus{{Target: "1.36", Score: 40, Verdict: "blocked", Blockers: 1}}
	unknown := []TargetStatus{{Target: "1.36", Score: 90, Verdict: "unknown"}}
	cases := []struct {
		name         string
		targets      []TargetStatus
		notAssessed  []string
		age          time.Duration
		observedGen  int64 // 0: the object's generation
		noStatus     bool
		wantStatus   string
		wantContains string
	}{
		{name: "ready", targets: ready, age: time.Minute, wantStatus: "Healthy", wantContains: "1.36"},
		{name: "ready, just inside the stale window", targets: ready, age: 29 * time.Minute, wantStatus: "Healthy"},
		{name: "ready, stale", targets: ready, age: 31 * time.Minute, wantStatus: "Degraded", wantContains: "status is stale"},
		{name: "blocked", targets: blocked, age: time.Minute, wantStatus: "Degraded", wantContains: "Blocked: 1.36: 1 blocker(s)"},
		{name: "blocked, stale", targets: blocked, age: time.Hour, wantStatus: "Degraded", wantContains: "status is stale"},
		{name: "unknown", targets: unknown, age: time.Minute, wantStatus: "Degraded", wantContains: ReasonNotAssessed + ": 1.36"},
		{name: "no target resolved", notAssessed: []string{"targets: no spec targets and server version unknown"}, age: time.Minute, wantStatus: "Degraded", wantContains: ReasonNotAssessed + ": no target evaluated"},
		{name: "spec edited since the last tick", targets: ready, age: time.Minute, observedGen: 2, wantStatus: "Progressing", wantContains: "spec changed"},
		{name: "no status yet", noStatus: true, wantStatus: "Progressing", wantContains: "first evaluation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cr := newCRObject(DefaultName, "1.36")
			cr.SetGeneration(3)
			dyn := newDynFake(cr)
			if !tc.noStatus {
				st := Status{
					LastEvaluated:      metav1.NewTime(now.Add(-tc.age)),
					Targets:            tc.targets,
					NotAssessed:        tc.notAssessed,
					ObservedGeneration: tc.observedGen,
				}
				if err := WriteStatus(context.Background(), dyn, DefaultName, st); err != nil {
					t.Fatal(err)
				}
			}
			got, err := dyn.Resource(GVR()).Get(context.Background(), DefaultName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			// Through JSON, as the API server serves it.
			raw, err := json.Marshal(got.Object)
			if err != nil {
				t.Fatal(err)
			}
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatal(err)
			}
			status, message := argoHealth(t, script, now, obj)
			if status != tc.wantStatus || !strings.Contains(message, tc.wantContains) {
				t.Errorf("health = %s %q, want %s containing %q", status, message, tc.wantStatus, tc.wantContains)
			}
		})
	}
}
