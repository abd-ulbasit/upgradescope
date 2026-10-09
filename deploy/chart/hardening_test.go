package chart

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Tests for the chart hardening of #242: the SQLite temp volume, token
// rotation, the stale threshold, ports, long names, the NetworkPolicy
// guards, high availability and chart-to-chart upgrades.

func podTemplate(t *testing.T, objs []unstructured.Unstructured, deployment string) map[string]any {
	t.Helper()
	d := find(objs, "Deployment", deployment)
	if d == nil {
		t.Fatalf("Deployment %s not rendered (have %v)", deployment, kinds(objs))
	}
	tpl, _, _ := unstructured.NestedMap(d.Object, "spec", "template")
	return tpl
}

func podAnnotation(t *testing.T, objs []unstructured.Unstructured, deployment, key string) (string, bool) {
	t.Helper()
	ann, _, _ := unstructured.NestedStringMap(podTemplate(t, objs, deployment), "metadata", "annotations")
	v, ok := ann[key]
	return v, ok
}

func volumeNamed(t *testing.T, objs []unstructured.Unstructured, deployment, name string) map[string]any {
	t.Helper()
	vols, _, _ := unstructured.NestedSlice(podTemplate(t, objs, deployment), "spec", "volumes")
	for _, v := range vols {
		if m := v.(map[string]any); m["name"] == name {
			return m
		}
	}
	return nil
}

func mountPath(c map[string]any, name string) string {
	ms, _, _ := unstructured.NestedSlice(c, "volumeMounts")
	for _, m := range ms {
		if mm := m.(map[string]any); mm["name"] == name {
			s, _ := mm["mountPath"].(string)
			return s
		}
	}
	return ""
}

func writeValues(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return "-f=" + f
}

// The server's root filesystem is read-only. SQLite spills a large delete (the
// retention prune, `clusters delete`) into a temp file, and with no writable
// temp directory that fails with "disk I/O error (6410)" and history grows
// without bound (internal/server/store/sqlite_tempdir_test.go reproduces it).
func TestServerSQLiteHasAWritableTempDir(t *testing.T) {
	objs := render(t, "server.enabled=true")
	c := container(t, objs, "upgradescope-server")
	if v, ok := envVar(c, "SQLITE_TMPDIR"); !ok || v != "/tmp" {
		t.Errorf("SQLITE_TMPDIR = %q (set %v), want /tmp", v, ok)
	}
	if got := mountPath(c, "sqlite-tmp"); got != "/tmp" {
		t.Errorf("tmp volume mounted at %q, want /tmp", got)
	}
	vol := volumeNamed(t, objs, "upgradescope-server", "sqlite-tmp")
	if vol == nil {
		t.Fatal("no sqlite-tmp volume")
	}
	if size, _, _ := unstructured.NestedString(vol, "emptyDir", "sizeLimit"); size != "1Gi" {
		t.Errorf("tmp emptyDir sizeLimit = %q, want 1Gi", size)
	}
	if ro, _, _ := unstructured.NestedBool(c, "securityContext", "readOnlyRootFilesystem"); !ro {
		t.Error("the root filesystem is not read-only: the temp volume is not what makes the delete work")
	}

	objs = render(t, "server.enabled=true", "server.tmp.sizeLimit=4Gi")
	if size, _, _ := unstructured.NestedString(volumeNamed(t, objs, "upgradescope-server", "sqlite-tmp"), "emptyDir", "sizeLimit"); size != "4Gi" {
		t.Errorf("server.tmp.sizeLimit=4Gi renders sizeLimit %q", size)
	}

	// Postgres does no SQLite work: no temp volume, no variable.
	objs = render(t, "server.enabled=true", "server.database.existingSecret=pg")
	c = container(t, objs, "upgradescope-server")
	if _, ok := envVar(c, "SQLITE_TMPDIR"); ok || mountPath(c, "sqlite-tmp") != "" || volumeNamed(t, objs, "upgradescope-server", "sqlite-tmp") != nil {
		t.Error("a Postgres server gets the SQLite temp directory")
	}
}

// A user's own volume at /tmp would be a second mount at one path, which
// makes the Deployment invalid: it becomes SQLite's temp directory instead,
// and the chart adds none. A user volume named like the chart's, sqlite-tmp,
// would silently replace it, so that fails the render while the chart's is
// there; the chart's volume is named sqlite-tmp so an ordinary "tmp" volume
// does not collide.
func TestExtraVolumesDoNotClashWithTheSQLiteTempVolume(t *testing.T) {
	// A user volume called "tmp" mounted elsewhere is fine.
	ok := writeValues(t, "server:\n  extraVolumes:\n    - {name: tmp, emptyDir: {}}\n  extraVolumeMounts:\n    - {name: tmp, mountPath: /scratch}\n")
	if msg := renderErr(t, "server.enabled=true", ok); msg != "" {
		t.Errorf("a volume named tmp mounted at /scratch fails the render: %s", msg)
	}
	if vol := volumeNamed(t, render(t, "server.enabled=true", ok), "upgradescope-server", "sqlite-tmp"); vol == nil {
		t.Error("a user volume mounted elsewhere drops the chart's sqlite-tmp volume")
	}

	// The user's own /tmp is the temp directory: one mount there, theirs,
	// no chart volume, and SQLITE_TMPDIR still /tmp.
	for _, tc := range []struct{ name, values string }{
		{"mine at /tmp", "server:\n  extraVolumes:\n    - {name: mine, emptyDir: {sizeLimit: 8Gi}}\n  extraVolumeMounts:\n    - {name: mine, mountPath: /tmp}\n"},
		{"mine at /tmp/", "server:\n  extraVolumes:\n    - {name: mine, emptyDir: {sizeLimit: 8Gi}}\n  extraVolumeMounts:\n    - {name: mine, mountPath: /tmp/}\n"},
		// Named like the chart's, but it replaces it rather than clashing.
		{"sqlite-tmp at /tmp", "server:\n  extraVolumes:\n    - {name: sqlite-tmp, emptyDir: {sizeLimit: 8Gi}}\n  extraVolumeMounts:\n    - {name: sqlite-tmp, mountPath: /tmp}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := writeValues(t, tc.values)
			if msg := renderErr(t, "server.enabled=true", f); msg != "" {
				t.Fatalf("render failed: %s", msg)
			}
			objs := render(t, "server.enabled=true", f)
			c := container(t, objs, "upgradescope-server")
			if v, ok := envVar(c, "SQLITE_TMPDIR"); !ok || v != "/tmp" {
				t.Errorf("SQLITE_TMPDIR = %q (set %v), want /tmp", v, ok)
			}
			ms, _, _ := unstructured.NestedSlice(c, "volumeMounts")
			var atTmp []string
			for _, m := range ms {
				mm := m.(map[string]any)
				if p, _ := mm["mountPath"].(string); strings.TrimSuffix(p, "/") == "/tmp" {
					atTmp = append(atTmp, fmt.Sprint(mm["name"]))
				}
			}
			if len(atTmp) != 1 {
				t.Errorf("mounts at /tmp = %v, want only the user's", atTmp)
			}
			vols, _, _ := unstructured.NestedSlice(podTemplate(t, objs, "upgradescope-server"), "spec", "volumes")
			var sizes []string
			for _, v := range vols {
				if size, ok, _ := unstructured.NestedString(v.(map[string]any), "emptyDir", "sizeLimit"); ok {
					sizes = append(sizes, size)
				}
			}
			if !slices.Equal(sizes, []string{"8Gi"}) {
				t.Errorf("emptyDir size limits = %v, want only the user's 8Gi (the chart's 1Gi volume skipped)", sizes)
			}
		})
	}

	// A volume named sqlite-tmp next to the chart's is a real clash.
	name := writeValues(t, "server:\n  extraVolumes:\n    - {name: sqlite-tmp, emptyDir: {}}\n")
	if msg := renderErr(t, "server.enabled=true", name); !strings.Contains(msg, "sqlite-tmp") {
		t.Errorf("a volume named sqlite-tmp renders: %q", msg)
	}
	elsewhere := writeValues(t, "server:\n  extraVolumes:\n    - {name: sqlite-tmp, emptyDir: {}}\n  extraVolumeMounts:\n    - {name: sqlite-tmp, mountPath: /scratch}\n")
	if msg := renderErr(t, "server.enabled=true", elsewhere); !strings.Contains(msg, "sqlite-tmp") {
		t.Errorf("a volume named sqlite-tmp mounted at /scratch renders: %q", msg)
	}
	// Postgres has no such volume, so /tmp and the name are the user's.
	mount := writeValues(t, "server:\n  extraVolumes:\n    - {name: sqlite-tmp, emptyDir: {}}\n  extraVolumeMounts:\n    - {name: sqlite-tmp, mountPath: /tmp}\n")
	if msg := renderErr(t, "server.enabled=true", "server.database.existingSecret=pg", mount); msg != "" {
		t.Errorf("a Postgres server refuses a /tmp mount: %s", msg)
	}
	if msg := renderErr(t, "server.enabled=true", "server.database.existingSecret=pg", name); msg != "" {
		t.Errorf("a Postgres server refuses a volume named sqlite-tmp: %s", msg)
	}
}

func secretData(t *testing.T, objs []unstructured.Unstructured, name string) map[string]any {
	t.Helper()
	s := find(objs, "Secret", name)
	if s == nil {
		t.Fatalf("Secret %s not rendered (have %v)", name, kinds(objs))
	}
	if _, ok := s.Object["stringData"]; ok {
		t.Errorf("Secret %s has stringData: the API server turns it into data and never removes a key an upgrade drops", name)
	}
	d, _, _ := unstructured.NestedMap(s.Object, "data")
	return d
}

// The containers read their tokens and webhook URLs from environment
// variables at start, so rotating one needs a restart. The chart must not
// try to do that through the pod template: any function of a secret value in
// pod or Deployment metadata, salted or not, is readable by everyone who can
// get pods or Deployments (a wider set than Secret readers), and a short
// operator-chosen token can be tested against it offline.
//
// The test renders every secret twice with different values and requires the
// two renders to differ in the Secrets alone: no other object, annotation
// or label carries anything derived from a secret.
func TestNoObjectCarriesAFunctionOfASecret(t *testing.T) {
	cases := map[string]func(v string) []string{
		"in-chart server": func(v string) []string {
			return []string{"server.enabled=true", "server.ingestToken=ingest-" + v, "server.readToken=read-" + v, "server.adminToken=admin-" + v,
				"server.slackWebhook=https://hooks.example.com/slack-" + v, "server.webhook=https://hooks.example.com/hook-" + v, "server.webhookSecret=sign-" + v,
				"server.teamMap[0].pattern=a-*", "server.teamMap[0].team=a"}
		},
		"remote agent": func(v string) []string {
			return []string{"agent.serverUrl=https://hub.example.com", "agent.serverToken=push-" + v}
		},
		"server, agent token of its own": func(v string) []string {
			return []string{"server.enabled=true", "server.sharedIngestToken=false", "agent.serverToken=push-" + v, "server.readToken=read-" + v}
		},
	}
	for name, sets := range cases {
		t.Run(name, func(t *testing.T) {
			a, b := render(t, sets("one")...), render(t, sets("two")...)
			if len(a) != len(b) {
				t.Fatalf("the renders hold %d and %d objects", len(a), len(b))
			}
			secrets := 0
			for i := range a {
				if a[i].GetKind() == "Secret" {
					secrets++
					continue
				}
				ja, _ := a[i].MarshalJSON()
				jb, _ := b[i].MarshalJSON()
				if !bytes.Equal(ja, jb) {
					t.Errorf("%s changes when only a secret value does:\n%s\n%s", a[i].GetKind()+"/"+a[i].GetName(), ja, jb)
				}
			}
			if secrets == 0 {
				t.Fatal("no Secret rendered: the test compares nothing")
			}
			// What the pod templates and Deployments do carry is the two
			// checksums of non-secret config, and nothing that looks like
			// a digest besides them.
			digest := regexp.MustCompile(`^[0-9a-f]{64}$`)
			for _, o := range a {
				if o.GetKind() != "Deployment" {
					continue
				}
				tpl, _, _ := unstructured.NestedStringMap(o.Object, "spec", "template", "metadata", "annotations")
				for k, v := range tpl {
					if digest.MatchString(v) && k != "checksum/team-map" && k != "checksum/registry" {
						t.Errorf("%s pod annotation %s = %s looks like a digest of something other than the team map or the registry", o.GetName(), k, v)
					}
					if strings.HasPrefix(k, "checksum/") && k != "checksum/team-map" && k != "checksum/registry" {
						t.Errorf("%s has the pod annotation %s: only checksum/team-map and checksum/registry are allowed", o.GetName(), k)
					}
				}
				if own := o.GetAnnotations(); len(own) != 0 {
					t.Errorf("Deployment %s has annotations %v", o.GetName(), own)
				}
			}
		})
	}
	// The team map is not a secret: its checksum is how the pods roll.
	if _, ok := podAnnotation(t, render(t, "server.enabled=true", "server.teamMap[0].pattern=a-*", "server.teamMap[0].team=a"), "upgradescope-server", "checksum/team-map"); !ok {
		t.Error("the server pod lost checksum/team-map")
	}
}

// renderNotes renders the chart's NOTES.txt for the given values (helm
// template does not print it, and helm install would need a cluster): the
// chart is copied with NOTES.txt wrapped in a define and a ConfigMap that
// prints it.
func renderNotes(t *testing.T, release string, sets ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(".")); err != nil {
		t.Fatal(err)
	}
	notes, err := os.ReadFile(filepath.Join(dir, "templates", "NOTES.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "templates", "NOTES.txt")); err != nil {
		t.Fatal(err)
	}
	wrapped := "{{- define \"test.notes\" -}}\n" + string(notes) + "\n{{- end -}}\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "_notes.tpl"), []byte(wrapped), 0o600); err != nil {
		t.Fatal(err)
	}
	cm := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: notes\ndata:\n  notes: |\n{{ include \"test.notes\" . | indent 4 }}\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "notes-test.yaml"), []byte(cm), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"template", release, dir, "--namespace", "upgradescope", "--show-only", "templates/notes-test.yaml"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	cmd := exec.Command(helmBin(t), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	var o unstructured.Unstructured
	if err := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096).Decode(&o.Object); err != nil {
		t.Fatal(err)
	}
	s, _, _ := unstructured.NestedString(o.Object, "data", "notes")
	return s
}

// The NOTES say that a rotation needs no restart and how long it takes, and
// still print the restart for what cannot rotate, by the Deployments' real
// names.
func TestNotesDescribeRotationWithoutARestart(t *testing.T) {
	both := strings.Join(strings.Fields(renderNotes(t, "upgradescope", "server.enabled=true")), " ")
	for _, want := range []string{
		"Rotating a token or a webhook URL needs no restart",
		"re-read a file when it changes",
		"at most every 5s",
		"up to about 60 to 90s",
		"existingSecret",
		"keeps the old value",
		"naming the file",
		"the database URL",
		"server.extraEnv",
		"kubectl -n upgradescope rollout restart deploy/upgradescope-server deploy/upgradescope-agent",
	} {
		if !strings.Contains(both, want) {
			t.Errorf("NOTES lack %q:\n%s", want, both)
		}
	}
	for _, bad := range []string{"environment variables, which", "restart the pods that read it"} {
		if strings.Contains(both, bad) {
			t.Errorf("NOTES still say %q", bad)
		}
	}
	// A long release: the command names the Deployments that are rendered.
	long := strings.Repeat("r", 50)
	notes := renderNotes(t, long, "server.enabled=true")
	lobjs := renderRelease(t, long, "server.enabled=true")
	var deps []string
	for _, o := range lobjs {
		if o.GetKind() == "Deployment" {
			deps = append(deps, "deploy/"+o.GetName())
		}
	}
	slices.Sort(deps)
	if len(deps) != 2 {
		t.Fatalf("deployments = %v", deps)
	}
	if !strings.Contains(notes, "rollout restart "+strings.Join([]string{deps[1], deps[0]}, " ")) { // server, then agent
		t.Errorf("NOTES name no restart of %v:\n%s", deps, notes)
	}
	// Only what is installed.
	agentOnly := renderNotes(t, "upgradescope", "agent.serverUrl=https://hub.example.com", "agent.serverToken=t")
	if !strings.Contains(agentOnly, "rollout restart deploy/upgradescope-agent\n") || strings.Contains(agentOnly, "rollout restart deploy/upgradescope-server") {
		t.Errorf("agent-only NOTES:\n%s", agentOnly)
	}
}

// A stale threshold above agent.interval renders, but one below the longest
// gap between an unchanged cluster's pushes (its next tick after the hourly
// force-sync: max(interval, 1h) plus one interval) flags healthy clusters
// stale between pushes, or holds the alert's condition there. The NOTES say
// so; the defaults, and values at or above the gap, are not warned about.
func TestNotesWarnOfAFlappingStaleThreshold(t *testing.T) {
	const staleWarn, alertWarn = "WARNING: server.staleAfter", "WARNING: metrics.prometheusRule.clusterStaleAfterSeconds"
	rule := []string{"server.enabled=true", "metrics.prometheusRule.enabled=true"}
	for _, tc := range []struct {
		name  string
		sets  []string
		stale string // the warning's text after staleWarn, "" for none
		alert string // the warning's text after alertWarn, "" for none
	}{
		{name: "defaults", sets: rule},
		{name: "default at 1h", sets: append(slices.Clone(rule), "agent.interval=1h")},
		{name: "staleAfter 1h at 10m", sets: append(slices.Clone(rule), "server.staleAfter=1h"),
			stale: "(1h) is below 4200s, the larger of agent.interval (10m) and 1h, plus one interval."},
		{name: "staleAfter 69m at 10m", sets: append(slices.Clone(rule), "server.staleAfter=69m"), stale: "(69m) is below 4200s"},
		{name: "staleAfter 70m at 10m", sets: append(slices.Clone(rule), "server.staleAfter=70m")},
		{name: "staleAfter 2h at 10m", sets: append(slices.Clone(rule), "server.staleAfter=2h")},
		{name: "staleAfter 90m at 1h", sets: append(slices.Clone(rule), "agent.interval=1h", "server.staleAfter=90m"),
			stale: "(90m) is below 7200s, the larger of agent.interval (1h) and 1h"},
		{name: "staleAfter 5h at 3h", sets: append(slices.Clone(rule), "agent.interval=3h", "server.staleAfter=5h"), stale: "(5h) is below 21600s"},
		{name: "alert 3601 at 10m", sets: append(slices.Clone(rule), "metrics.prometheusRule.clusterStaleAfterSeconds=3601"),
			alert: "(3601) is below 4200s, the larger of agent.interval (10m) and 1h, plus one interval."},
		{name: "alert 4200 at 10m", sets: append(slices.Clone(rule), "metrics.prometheusRule.clusterStaleAfterSeconds=4200")},
		{name: "both", sets: append(slices.Clone(rule), "server.staleAfter=1h", "metrics.prometheusRule.clusterStaleAfterSeconds=3700"),
			stale: "(1h) is below 4200s", alert: "(3700) is below 4200s"},
		// The force-sync period the agent really runs: --force-sync-every in
		// agent.extraArgs, in either spelling, the last one winning.
		{name: "force-sync 2h, staleAfter 2h", sets: append(slices.Clone(rule), "server.staleAfter=2h", "agent.extraArgs={--force-sync-every=2h}"),
			stale: "(2h) is below 7800s, the larger of agent.interval (10m) and the force-sync period (7200s, agent.extraArgs --force-sync-every), plus one interval."},
		{name: "force-sync 2h, staleAfter 3h", sets: append(slices.Clone(rule), "server.staleAfter=3h", "agent.extraArgs={--force-sync-every=2h}")},
		{name: "force-sync 2h as two arguments", sets: append(slices.Clone(rule), "server.staleAfter=2h", "agent.extraArgs={--force-sync-every,2h}"),
			stale: "(2h) is below 7800s"},
		{name: "force-sync 30m is below the interval's hour", sets: append(slices.Clone(rule), "server.staleAfter=70m", "agent.extraArgs={--force-sync-every=30m}")},
		{name: "last force-sync wins", sets: append(slices.Clone(rule), "server.staleAfter=2h", "agent.extraArgs={--force-sync-every=4h,--force-sync-every=1h}")},
		{name: "force-sync 2h, alert 4200", sets: append(slices.Clone(rule), "agent.extraArgs={--force-sync-every=2h}", "metrics.prometheusRule.clusterStaleAfterSeconds=4200"),
			alert: "(4200) is below 7800s, the larger of agent.interval (10m) and the force-sync period (7200s, agent.extraArgs --force-sync-every), plus one interval."},
		// The alert threshold is unused without the rule.
		{name: "alert without the rule", sets: []string{"server.enabled=true", "metrics.prometheusRule.clusterStaleAfterSeconds=3601"}},
		// A hub with no in-chart agent does not know its agents' intervals.
		{name: "hub", sets: []string{"server.enabled=true", "agent.enabled=false", "server.staleAfter=30m", "metrics.prometheusRule.enabled=true",
			"metrics.prometheusRule.clusterStaleAfterSeconds=1800"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notes := strings.Join(strings.Fields(renderNotes(t, "upgradescope", tc.sets...)), " ")
			for _, w := range []struct{ prefix, want string }{{staleWarn, tc.stale}, {alertWarn, tc.alert}} {
				switch has := strings.Contains(notes, w.prefix); {
				case w.want == "" && has:
					t.Errorf("NOTES warn %q:\n%s", w.prefix, notes)
				case w.want != "" && !strings.Contains(notes, w.prefix+" "+w.want):
					t.Errorf("NOTES lack %q:\n%s", w.prefix+" "+w.want, notes)
				}
			}
			if tc.stale != "" && !strings.Contains(notes, "healthy clusters will read stale between pushes") {
				t.Errorf("the staleAfter warning does not say what happens:\n%s", notes)
			}
			if tc.alert != "" && !strings.Contains(notes, "UpgradescopeClusterStale alert's condition will hold between the pushes of healthy clusters") {
				t.Errorf("the alert warning does not say what happens:\n%s", notes)
			}
		})
	}
}

// A value dropped on upgrade must leave the live Secret: stringData is
// merged into data and never removed, so every key is written under data.
func TestSecretKeysAreRemovedWithTheirValues(t *testing.T) {
	name := "upgradescope-server-tokens"
	all := render(t, "server.enabled=true", "server.ingestToken=i", "server.readToken=r", "server.adminToken=a",
		"server.slackWebhook=https://s.example.com", "server.webhook=https://w.example.com", "server.webhookSecret=ws")
	d := secretData(t, all, name)
	for _, k := range []string{"ingestToken", "readToken", "adminToken", "slackWebhook", "webhook", "webhookSecret"} {
		if _, ok := d[k]; !ok {
			t.Errorf("Secret data has no %s", k)
		}
	}
	dropped := secretData(t, render(t, "server.enabled=true", "server.ingestToken=i"), name)
	for _, k := range []string{"readToken", "adminToken", "slackWebhook", "webhook", "webhookSecret"} {
		if _, ok := dropped[k]; ok {
			t.Errorf("removing server.%s leaves %s in the rendered Secret", k, k)
		}
	}
	if _, ok := dropped["ingestToken"]; !ok {
		t.Error("the ingest token is gone")
	}
	// No shared ingest token and nothing else: no key at all.
	if d := secretData(t, render(t, "server.enabled=true", "server.sharedIngestToken=false", "agent.serverToken=t"), name); len(d) != 0 {
		t.Errorf("Secret data = %v, want none", d)
	}
	agent := secretData(t, render(t, "agent.serverUrl=https://hub.example.com", "agent.serverToken=t"), "upgradescope-agent-token")
	if _, ok := agent["serverToken"]; !ok {
		t.Error("agent token Secret has no serverToken under data")
	}
}

func serverArg(t *testing.T, objs []unstructured.Unstructured, prefix string) string {
	t.Helper()
	for _, a := range args(container(t, objs, "upgradescope-server")) {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			return v
		}
	}
	return ""
}

func staleRule(t *testing.T, objs []unstructured.Unstructured) string {
	t.Helper()
	expr, _ := alerts(t, objs)["UpgradescopeClusterStale"]["expr"].(string)
	m := regexp.MustCompile(`> (\d+)$`).FindStringSubmatch(expr)
	if m == nil {
		t.Fatalf("UpgradescopeClusterStale expr = %q, want to end in a number of seconds", expr)
	}
	return m[1]
}

// An unchanged cluster pushes at its next tick after the hourly force-sync,
// so the stale threshold has to follow the agent interval: with the default
// 2h and an interval of 3h, every cluster flagged stale part of every cycle.
func TestStaleThresholdFollowsTheAgentInterval(t *testing.T) {
	for _, tc := range []struct {
		interval  string
		wantFlag  string
		wantAlert string
	}{
		{"10m", "2h", "7200"}, // the default: unchanged output
		{"40m", "2h", "7200"},
		{"1h", "10800s", "10800"},
		{"3h", "32400s", "32400"},
		{"1.5h", "16200s", "16200"},
		{"1h30m", "16200s", "16200"},
	} {
		t.Run(tc.interval, func(t *testing.T) {
			objs := render(t, "server.enabled=true", "agent.interval="+tc.interval, "metrics.prometheusRule.enabled=true")
			if got := serverArg(t, objs, "--stale-after="); got != tc.wantFlag {
				t.Errorf("--stale-after = %q, want %q", got, tc.wantFlag)
			}
			if got := staleRule(t, objs); got != tc.wantAlert {
				t.Errorf("alert threshold = %s seconds, want %s", got, tc.wantAlert)
			}
		})
	}

	// Set explicitly: used as written, and the alert follows it.
	objs := render(t, "server.enabled=true", "server.staleAfter=5h", "metrics.prometheusRule.enabled=true")
	if got := serverArg(t, objs, "--stale-after="); got != "5h" {
		t.Errorf("--stale-after = %q, want 5h", got)
	}
	if got := staleRule(t, objs); got != "18000" {
		t.Errorf("alert threshold = %s, want 18000 (follows server.staleAfter)", got)
	}
	// A separate alert threshold wins for the alert only.
	objs = render(t, "server.enabled=true", "metrics.prometheusRule.enabled=true", "metrics.prometheusRule.clusterStaleAfterSeconds=9000")
	if got := staleRule(t, objs); got != "9000" {
		t.Errorf("alert threshold = %s, want 9000", got)
	}

	// Not above the interval: every cluster would read stale between pushes.
	for _, sets := range [][]string{
		{"server.staleAfter=1h", "agent.interval=1h"},
		{"server.staleAfter=30m", "agent.interval=1h"},
		{"metrics.prometheusRule.enabled=true", "metrics.prometheusRule.clusterStaleAfterSeconds=3600", "agent.interval=1h"},
	} {
		if msg := renderErr(t, append([]string{"server.enabled=true"}, sets...)...); !strings.Contains(msg, "must be above agent.interval") {
			t.Errorf("%v: render error = %q, want one naming agent.interval", sets, msg)
		}
	}
	// A hub with no in-chart agent does not know its agents' intervals.
	if msg := renderErr(t, "server.enabled=true", "agent.enabled=false", "server.staleAfter=30m", "agent.interval=1h"); msg != "" {
		t.Errorf("server-only render failed: %s", msg)
	}
}

// The Prometheus guide gives the stale alert's threshold as the chart
// renders it: 0 by default, following the server's, not a fixed 7200.
func TestDocsGiveTheStaleAlertThreshold(t *testing.T) {
	raw, err := os.ReadFile("../../docs/guides/prometheus-grafana.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	for _, bad := range []string{"(default 7200)", "upgradescope_cluster_last_push_age_seconds > 7200`", "above an hour"} {
		if strings.Contains(text, bad) {
			t.Errorf("the Prometheus guide still says %q", bad)
		}
	}
	for _, want := range []string{
		"upgradescope_cluster_last_push_age_seconds > <threshold>`",
		"`metrics.prometheusRule.clusterStaleAfterSeconds`, which defaults to 0",
		"at or below `agent.interval` fails the render",
		"(../operations.md#stale-clusters)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the Prometheus guide lacks %q", want)
		}
	}
}

// The Service port may be 80 or 443; the non-root pod without capabilities
// cannot bind either, so serve listens on its own container port.
func TestServicePortIsNotTheListenPort(t *testing.T) {
	for _, port := range []string{"80", "443", "8080"} {
		objs := render(t, "server.enabled=true", "server.service.port="+port)
		c := container(t, objs, "upgradescope-server")
		if !slices.Contains(args(c), "--listen=:8080") {
			t.Errorf("service.port=%s: args %v, want --listen=:8080", port, args(c))
		}
		ports, _, _ := unstructured.NestedSlice(c, "ports")
		if cp := fmt.Sprint(ports[0].(map[string]any)["containerPort"]); cp != "8080" {
			t.Errorf("service.port=%s: containerPort %s, want 8080", port, cp)
		}
		svc := find(objs, "Service", "upgradescope-server")
		sp, _, _ := unstructured.NestedSlice(svc.Object, "spec", "ports")
		if got := fmt.Sprint(sp[0].(map[string]any)["port"]); got != port {
			t.Errorf("service.port=%s: Service port %s", port, got)
		}
		if got := sp[0].(map[string]any)["targetPort"]; got != "http" {
			t.Errorf("service.port=%s: targetPort %v, want the named port http", port, got)
		}
		agent := container(t, objs, "upgradescope-agent")
		if want := fmt.Sprintf("--server-url=http://upgradescope-server.upgradescope.svc:%s", port); !slices.Contains(args(agent), want) {
			t.Errorf("service.port=%s: agent args %v, want %s (the agent pushes to the Service)", port, args(agent), want)
		}
	}
	objs := render(t, "server.enabled=true", "server.containerPort=9090", "server.service.port=443")
	c := container(t, objs, "upgradescope-server")
	if !slices.Contains(args(c), "--listen=:9090") {
		t.Errorf("containerPort=9090: args %v", args(c))
	}
	if path, port := probe(t, c, "readinessProbe"); path != "/readyz" || port != "http" {
		t.Errorf("readiness probe %s %s, want /readyz on the named port", path, port)
	}
	if msg := renderErr(t, "server.enabled=true", "server.containerPort=443"); msg == "" {
		t.Error("server.containerPort=443 renders: the non-root pod cannot bind it")
	}
}

// A Service name is a DNS-1035 label: at most 63 characters, which the API
// server enforces and helm template does not. Everything that names a
// Service follows its cut name; no other resource is renamed (see
// TestLongReleaseKeepsTheNamesOfResourcesThatExist).
func TestServiceNamesFitSixtyThreeCharacters(t *testing.T) {
	values := writeValues(t, `
server:
  enabled: true
  replicas: 2
  readToken: r
  teamMap: [{pattern: "a-*", team: a}]
  database: {existingSecret: pg}
  tls: {certManager: {issuerRef: {name: ca}}}
  ingress: {enabled: true, host: u.example.com}
networkPolicy: {enabled: true, serverIngressFrom: [{podSelector: {}}]}
agent:
  extraRegistry:
    thing.yaml: "id: thing"
metrics:
  serviceMonitor: {enabled: true}
  prometheusRule: {enabled: true}
  grafanaDashboard: {enabled: true}
`)
	// 53 characters, Helm's longest release name.
	for _, release := range []string{
		strings.Repeat("a", 53),
		strings.Repeat("a-", 26) + "a", // hyphens: a cut can land on one
		"upgradescope",
	} {
		if release != "upgradescope" && len(release) != 53 {
			t.Fatalf("release %q is %d characters", release, len(release))
		}
		objs := renderRelease(t, release, values)
		for _, o := range objs {
			if n := o.GetName(); o.GetKind() == "Service" && len(n) > 63 {
				t.Errorf("%s: %s %q is %d characters", release, o.GetKind(), n, len(n))
			}
		}
		// Every Secret and ConfigMap a pod names exists under that name.
		have := map[string]bool{}
		for _, o := range objs {
			have[o.GetKind()+"/"+o.GetName()] = true
		}
		for _, dep := range []string{"-agent", "-server"} {
			var d *unstructured.Unstructured
			for i := range objs {
				if objs[i].GetKind() == "Deployment" && strings.HasSuffix(objs[i].GetName(), dep) {
					d = &objs[i]
				}
			}
			if d == nil {
				t.Fatalf("%s: no %s Deployment", release, dep)
			}
			raw, _ := d.MarshalJSON()
			for _, m := range regexp.MustCompile(`"(?:secretKeyRef|configMap)":\{[^}]*?"name":"([^"]+)"`).FindAllStringSubmatch(string(raw), -1) {
				if m[1] == "pg" {
					continue
				}
				if !have["Secret/"+m[1]] && !have["ConfigMap/"+m[1]] {
					t.Errorf("%s: %s names %q, which is not rendered", release, d.GetName(), m[1])
				}
			}
		}
		// The ServiceMonitor, the rule and the Ingress name the Service.
		svc := ""
		for _, o := range objs {
			if o.GetKind() == "Service" && strings.HasSuffix(o.GetName(), "-server") {
				svc = o.GetName()
			}
		}
		// The certificate names the Service it is for.
		for _, o := range objs {
			if o.GetKind() != "Certificate" {
				continue
			}
			dns, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "dnsNames")
			if !slices.Contains(dns, svc+".upgradescope.svc") {
				t.Errorf("%s: Certificate dnsNames %v lack the Service %q", release, dns, svc)
			}
		}
		var ing *unstructured.Unstructured
		for i := range objs {
			if objs[i].GetKind() == "Ingress" {
				ing = &objs[i]
			}
		}
		if ing == nil {
			t.Fatalf("%s: no Ingress", release)
		}
		// The in-chart agent pushes to the Service.
		if a := strings.Join(args(container(t, objs, find2(objs, "Deployment", "-agent"))), " "); !strings.Contains(a, "://"+svc+".upgradescope.svc:") {
			t.Errorf("%s: agent args %q do not push to the Service %q", release, a, svc)
		}
		rules, _, _ := unstructured.NestedSlice(ing.Object, "spec", "rules")
		paths, _, _ := unstructured.NestedSlice(rules[0].(map[string]any), "http", "paths")
		if name, _, _ := unstructured.NestedString(paths[0].(map[string]any), "backend", "service", "name"); name != svc {
			t.Errorf("%s: Ingress backend %q, Service %q", release, name, svc)
		}
		a := alerts(t, objs)
		expr, _ := a["UpgradescopeClusterStale"]["expr"].(string)
		if !strings.Contains(expr, `job="`+svc+`"`) {
			t.Errorf("%s: stale alert %q does not select the server Service %q", release, expr, svc)
		}
	}
	// A release that fits keeps the names it always had.
	objs := render(t, "server.enabled=true", "metrics.serviceMonitor.enabled=true", "server.teamMap[0].pattern=a-*", "server.teamMap[0].team=a")
	var got []string
	for _, o := range objs {
		if o.GetKind() == "Service" || o.GetKind() == "Secret" || o.GetKind() == "ConfigMap" || o.GetKind() == "PersistentVolumeClaim" {
			got = append(got, o.GetKind()+"/"+o.GetName())
		}
	}
	slices.Sort(got)
	want := []string{
		"ConfigMap/upgradescope-server-team-map", "PersistentVolumeClaim/upgradescope-server-data",
		"Secret/upgradescope-server-tokens", "Service/upgradescope-agent-metrics", "Service/upgradescope-server",
	}
	if !slices.Equal(got, want) {
		t.Errorf("default names = %v, want %v", got, want)
	}
}

// An earlier chart named every resource fullname + suffix, uncut, and the
// API server accepts a PVC, Secret or ConfigMap name of up to 253
// characters. A release whose fullname is 52 to 56 characters has such
// names beyond 63. Renaming the PVC on `helm upgrade` makes Helm delete the
// old one (the SQLite history with it), and a renamed token Secret defeats
// the lookup that keeps the generated ingest token, so every remote agent
// gets a 401. Only a Service name is cut: the Services of such a release
// never installed, so there is nothing to keep.
func TestLongReleaseKeepsTheNamesOfResourcesThatExist(t *testing.T) {
	const release = "platform-observability-readiness-scan-eu" // 40 characters
	if len(release) != 40 {
		t.Fatalf("release is %d characters", len(release))
	}
	full := release + "-upgradescope"
	objs := renderRelease(t, release, "server.enabled=true", "metrics.serviceMonitor.enabled=true", "metrics.prometheusRule.enabled=true",
		"server.teamMap[0].pattern=a-*", "server.teamMap[0].team=a", "server.ingress.enabled=true", "server.ingress.host=u.example.com", "server.readToken=x",
		"server.tls.certManager.issuerRef.name=ca", "agent.extraRegistry.thing\\.yaml=id: thing")
	// What origin/main rendered for these values (fullname + suffix).
	for kind, names := range map[string][]string{
		"PersistentVolumeClaim": {full + "-server-data"},
		"Secret":                {full + "-server-tokens"},
		"ConfigMap":             {full + "-server-team-map", full + "-agent-registry"},
		"Deployment":            {full + "-server", full + "-agent"},
		"ServiceAccount":        {full, full + "-server"},
		"ClusterRole":           {full + "-agent"},
		"ClusterRoleBinding":    {full + "-agent"},
		"Ingress":               {full + "-server"},
		"Certificate":           {full + "-server"},
		"ServiceMonitor":        {full + "-agent", full + "-server"},
		"PrometheusRule":        {full},
	} {
		var have []string
		for _, o := range objs {
			if o.GetKind() == kind {
				have = append(have, o.GetName())
			}
		}
		slices.Sort(have)
		slices.Sort(names)
		if !slices.Equal(have, names) {
			t.Errorf("%s names = %v, want the uncut %v", kind, have, names)
		}
	}
	// The Services are the cut ones, and what points at them follows.
	for _, o := range objs {
		if o.GetKind() == "Service" && len(o.GetName()) > 63 {
			t.Errorf("Service %q is %d characters", o.GetName(), len(o.GetName()))
		}
	}
	// A pod names the PVC and the Secret under the names that exist.
	srv := podTemplate(t, objs, full+"-server")
	if v := volumeNamed(t, objs, full+"-server", "data"); v == nil {
		t.Error("no data volume")
	} else if claim, _, _ := unstructured.NestedString(v, "persistentVolumeClaim", "claimName"); claim != full+"-server-data" {
		t.Errorf("claimName = %q, want %q", claim, full+"-server-data")
	}
	_ = srv
	if v := volumeNamed(t, objs, full+"-server", "secret-files"); v == nil {
		t.Error("no secret-files volume")
	} else if name, _, _ := unstructured.NestedString(v, "secret", "secretName"); name != full+"-server-tokens" {
		t.Errorf("the server reads its tokens from the Secret %q, want %s-server-tokens", name, full)
	}
}

// networkPolicy admits the in-chart agent only; behind it, the Ingress
// controller and Prometheus get a 504 or a failed scrape with no error
// anywhere in the chart.
func TestNetworkPolicyGuards(t *testing.T) {
	ingress := []string{"server.enabled=true", "networkPolicy.enabled=true", "server.ingress.enabled=true", "server.ingress.host=h.example.com", "server.readToken=x"}
	if msg := renderErr(t, ingress...); !strings.Contains(msg, "networkPolicy.serverIngressFrom") || !strings.Contains(msg, "Ingress controller") {
		t.Errorf("Ingress with a NetworkPolicy and no peers: render error = %q, want one naming networkPolicy.serverIngressFrom", msg)
	}
	withPeer := append(slices.Clone(ingress), "networkPolicy.serverIngressFrom[0].namespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=ingress-nginx")
	objs := render(t, withPeer...)
	np := find(objs, "NetworkPolicy", "upgradescope-server")
	if np == nil {
		t.Fatal("no NetworkPolicy")
	}
	from, _, _ := unstructured.NestedSlice(np.Object, "spec", "ingress")
	peers, _ := from[0].(map[string]any)["from"].([]any)
	if len(peers) != 2 {
		t.Errorf("NetworkPolicy peers = %v, want the agent and the ingress controller", peers)
	}

	monitor := []string{"server.enabled=true", "networkPolicy.enabled=true", "metrics.serviceMonitor.enabled=true"}
	if msg := renderErr(t, monitor...); !strings.Contains(msg, "networkPolicy.serverIngressFrom") || !strings.Contains(msg, "Prometheus") {
		t.Errorf("ServiceMonitor with a NetworkPolicy and no peers: render error = %q", msg)
	}
	if msg := renderErr(t, append(slices.Clone(monitor), "networkPolicy.serverIngressFrom[0].namespaceSelector.matchLabels.team=obs")...); msg != "" {
		t.Errorf("ServiceMonitor with a peer: %s", msg)
	}
	// Neither exposure: the agent-only policy still renders.
	if msg := renderErr(t, "server.enabled=true", "networkPolicy.enabled=true"); msg != "" {
		t.Errorf("plain NetworkPolicy: %s", msg)
	}
	// Without the policy nothing is guarded.
	if msg := renderErr(t, "server.enabled=true", "server.ingress.enabled=true", "server.ingress.host=h.example.com", "server.readToken=x"); msg != "" {
		t.Errorf("Ingress without a policy: %s", msg)
	}
}

// replicas above one: a PodDisruptionBudget so a node drain keeps a pod up,
// and a soft spread across nodes.
func TestServerHighAvailability(t *testing.T) {
	ha := []string{"server.enabled=true", "server.replicas=2", "server.database.existingSecret=pg"}
	objs := render(t, ha...)
	pdb := find(objs, "PodDisruptionBudget", "upgradescope-server")
	if pdb == nil {
		t.Fatalf("no PodDisruptionBudget (have %v)", kinds(objs))
	}
	if v, _, _ := unstructured.NestedFieldNoCopy(pdb.Object, "spec", "minAvailable"); fmt.Sprint(v) != "1" {
		t.Errorf("minAvailable = %v, want 1", v)
	}
	sel, _, _ := unstructured.NestedStringMap(pdb.Object, "spec", "selector", "matchLabels")
	if sel["app.kubernetes.io/component"] != "server" || sel["app.kubernetes.io/instance"] != "upgradescope" {
		t.Errorf("PDB selector = %v, want the server pods", sel)
	}
	tsc, _, _ := unstructured.NestedSlice(podTemplate(t, objs, "upgradescope-server"), "spec", "topologySpreadConstraints")
	if len(tsc) != 1 {
		t.Fatalf("topologySpreadConstraints = %v, want the default", tsc)
	}
	c := tsc[0].(map[string]any)
	if c["topologyKey"] != "kubernetes.io/hostname" || c["whenUnsatisfiable"] != "ScheduleAnyway" {
		t.Errorf("default spread = %v, want a soft spread over hostnames", c)
	}
	lsel, _, _ := unstructured.NestedStringMap(c, "labelSelector", "matchLabels")
	if !mapsEqual(lsel, sel) {
		t.Errorf("spread selector %v differs from the PDB's %v", lsel, sel)
	}

	// One replica: nothing that could block a drain.
	one := render(t, "server.enabled=true")
	if find(one, "PodDisruptionBudget", "upgradescope-server") != nil {
		t.Error("a PodDisruptionBudget with one replica blocks every node drain")
	}
	if tsc, _, _ := unstructured.NestedSlice(podTemplate(t, one, "upgradescope-server"), "spec", "topologySpreadConstraints"); len(tsc) != 0 {
		t.Errorf("one replica gets topologySpreadConstraints %v", tsc)
	}

	// Opt-outs and overrides.
	off := render(t, append(slices.Clone(ha), "server.podDisruptionBudget.enabled=false", "server.defaultTopologySpread=false")...)
	if find(off, "PodDisruptionBudget", "upgradescope-server") != nil {
		t.Error("podDisruptionBudget.enabled=false still renders one")
	}
	if tsc, _, _ := unstructured.NestedSlice(podTemplate(t, off, "upgradescope-server"), "spec", "topologySpreadConstraints"); len(tsc) != 0 {
		t.Error("defaultTopologySpread=false still spreads")
	}
	mu := render(t, append(slices.Clone(ha), "server.podDisruptionBudget.maxUnavailable=50%")...)
	pdb = find(mu, "PodDisruptionBudget", "upgradescope-server")
	if v, _, _ := unstructured.NestedString(pdb.Object, "spec", "maxUnavailable"); v != "50%" {
		t.Errorf("maxUnavailable = %q, want 50%%", v)
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(pdb.Object, "spec", "minAvailable"); ok {
		t.Error("minAvailable rendered next to maxUnavailable")
	}
	own := render(t, append(slices.Clone(ha), "server.topologySpreadConstraints[0].maxSkew=1", "server.topologySpreadConstraints[0].topologyKey=topology.kubernetes.io/zone",
		"server.topologySpreadConstraints[0].whenUnsatisfiable=DoNotSchedule")...)
	tsc, _, _ = unstructured.NestedSlice(podTemplate(t, own, "upgradescope-server"), "spec", "topologySpreadConstraints")
	if len(tsc) != 1 || tsc[0].(map[string]any)["topologyKey"] != "topology.kubernetes.io/zone" {
		t.Errorf("own topologySpreadConstraints = %v, want only the zone constraint", tsc)
	}
	// SQLite with replicas is refused, as before.
	if msg := renderErr(t, "server.enabled=true", "server.replicas=2"); !strings.Contains(msg, "Postgres") {
		t.Errorf("SQLite with 2 replicas: %q", msg)
	}
}

// find2 returns the name of the one object of kind whose name ends in suffix.
func find2(objs []unstructured.Unstructured, kind, suffix string) string {
	for _, o := range objs {
		if o.GetKind() == kind && strings.HasSuffix(o.GetName(), suffix) {
			return o.GetName()
		}
	}
	return ""
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// renderChartDir renders the chart at dir with the given values files.
func renderChartDir(t *testing.T, dir string, files ...string) ([]unstructured.Unstructured, error) {
	t.Helper()
	a := []string{"template", "upgradescope", dir, "--namespace", "upgradescope"}
	for _, f := range files {
		a = append(a, "-f", f)
	}
	cmd := exec.Command(helmBin(t), a...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, stderr.String())
	}
	var objs []unstructured.Unstructured
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var o unstructured.Unstructured
		if err := dec.Decode(&o.Object); err != nil {
			if errors.Is(err, io.EOF) {
				return objs, nil
			}
			return nil, fmt.Errorf("decode rendered manifests: %w", err)
		}
		if o.Object != nil {
			objs = append(objs, o)
		}
	}
}

// `helm upgrade --reset-then-reuse-values` (Helm 3.14+) renders the NEW
// chart with its own values.yaml as the defaults and the OLD release's user
// values (what `helm get values` prints) on top. `--reuse-values` would
// instead make the old release's defaults the new chart's, which pins the
// old image digest and every default that changed since. This renders the
// current chart as the first does and checks that the new defaults win.
func TestUpgradeWithResetThenReuseValues(t *testing.T) {
	const newDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(".")); err != nil {
		t.Fatal(err)
	}
	// Stamp the way the release workflow does (hack/chart-release-annotations.sh).
	vals, err := os.ReadFile(filepath.Join(dir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	stamped := strings.Replace(string(vals), "  digest: \"\"\n", "  digest: \""+newDigest+"\"\n", 1)
	if stamped == string(vals) {
		t.Fatal("values.yaml has no digest line to stamp")
	}
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), []byte(stamped), 0o600); err != nil {
		t.Fatal(err)
	}
	chartYAML, err := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	appVersion := regexp.MustCompile(`(?m)^appVersion: "?([^"\n]+)"?$`).FindSubmatch(chartYAML)
	if appVersion == nil {
		t.Fatal("no appVersion in Chart.yaml")
	}

	for _, old := range []string{"user-values-v0.1.x.yaml", "user-values-v0.2.0-rc.2.yaml"} {
		t.Run(old, func(t *testing.T) {
			objs, err := renderChartDir(t, dir, filepath.Join("testdata", "upgrade", old))
			if err != nil {
				t.Fatalf("the current chart does not render on %s: %v", old, err)
			}
			want := "ghcr.io/abd-ulbasit/upgradescope:" + string(appVersion[1]) + "@" + newDigest
			for _, dep := range []string{"upgradescope-agent", "upgradescope-server"} {
				c := container(t, objs, dep)
				if c["image"] != want {
					t.Errorf("%s image = %v, want %s (the new chart's digest, never an old one)", dep, c["image"], want)
				}
			}
			// Defaults that changed since v0.1.x apply unless the user set them.
			srv := container(t, objs, "upgradescope-server")
			if mem, _, _ := unstructured.NestedString(srv, "resources", "limits", "memory"); mem != "1Gi" {
				t.Errorf("server memory limit = %q, want the new default 1Gi", mem)
			}
			if _, ok := envVar(srv, "SQLITE_TMPDIR"); !ok {
				t.Error("an upgraded SQLite server has no SQLITE_TMPDIR")
			}
		})
	}
}

var fence = regexp.MustCompile("^\\s*(```|~~~)")

// The upgrade commands the docs give must not use --reuse-values, which
// carries the old chart's defaults (its image digest, resource limits,
// security contexts) onto the new chart and fails to render when the new
// chart adds a value. A page may warn against it in prose.
var inlineCode = regexp.MustCompile("`[^`]+`")

func TestDocsDoNotRecommendReuseValues(t *testing.T) {
	roots := []string{"../../README.md", "README.md", "../../docs"}
	var files []string
	for _, r := range roots {
		err := filepath.WalkDir(r, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(p, ".md") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	bare := regexp.MustCompile(`--reuse-values`)
	var withReset int
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		inFence, n := false, 0
		for sc.Scan() {
			n++
			line := sc.Text()
			if fence.MatchString(line) {
				inFence = !inFence
			}
			if strings.Contains(line, "--reset-then-reuse-values") {
				withReset++
			}
			clean := strings.ReplaceAll(line, "--reset-then-reuse-values", "")
			if bare.MatchString(clean) && inFence {
				t.Errorf("%s:%d recommends --reuse-values in a command: %s", f, n, strings.TrimSpace(line))
			}
			if !inFence {
				for _, span := range inlineCode.FindAllString(clean, -1) {
					if strings.Contains(span, "helm upgrade") && bare.MatchString(span) {
						t.Errorf("%s:%d recommends --reuse-values in an inline command: %s", f, n, span)
					}
				}
			}
		}
		_ = fh.Close()
	}
	if withReset == 0 {
		t.Error("no doc gives the --reset-then-reuse-values command")
	}
}

// unknownKeys lists, as dotted paths, the keys of user values that a chart's
// values.yaml (defaults) does not have. Below a key whose default is a map
// with keys, the user's keys must be among them; below an empty map (free
// form, such as podAnnotations), a list or a scalar, any value goes.
func unknownKeys(user, defaults map[string]any, path string) []string {
	var out []string
	for k, v := range user {
		d, ok := defaults[k]
		if !ok {
			out = append(out, path+k)
			continue
		}
		um, uok := v.(map[string]any)
		dm, dok := d.(map[string]any)
		if uok && dok && len(dm) > 0 {
			out = append(out, unknownKeys(um, dm, path+k+".")...)
		}
	}
	slices.Sort(out)
	return out
}

// The files in testdata/upgrade claim to be what `helm get values` printed
// for a release of an older chart. Each is checked against that chart, taken
// from its tag (skipped where the tag is not in the clone), in two ways: it
// must render on it, which from v0.2.0-rc.1 on means passing its schema,
// which refuses a key it did not have; and every key must be one its
// values.yaml has, which is the only guard for v0.1.1, whose chart has no
// values.schema.json and renders whatever keys it is given.
func TestUpgradeValuesFilesAreRealUserValues(t *testing.T) {
	for file, tag := range map[string]string{
		"user-values-v0.1.x.yaml":      "v0.1.1",
		"user-values-v0.2.0-rc.2.yaml": "v0.2.0-rc.2",
	} {
		t.Run(tag, func(t *testing.T) {
			if err := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/tags/"+tag).Run(); err != nil {
				t.Skipf("tag %s is not in this clone", tag)
			}
			dir := t.TempDir()
			tarball, err := exec.Command("git", "-C", "../..", "archive", tag, "--", "deploy/chart").Output()
			if err != nil {
				t.Fatalf("git archive %s: %v", tag, err)
			}
			untar := exec.Command("tar", "-x", "-C", dir)
			untar.Stdin = bytes.NewReader(tarball)
			if out, err := untar.CombinedOutput(); err != nil {
				t.Fatalf("tar: %v\n%s", err, out)
			}
			chart := filepath.Join(dir, "deploy", "chart")
			userFile := filepath.Join("testdata", "upgrade", file)
			if _, err := renderChartDir(t, chart, userFile); err != nil {
				t.Errorf("the %s chart refuses %s, so it is not what a %s release's user values were: %v", tag, file, tag, err)
			}

			var user, defaults map[string]any
			for f, into := range map[string]*map[string]any{userFile: &user, filepath.Join(chart, "values.yaml"): &defaults} {
				raw, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				if err := yaml.Unmarshal(raw, into); err != nil {
					t.Fatalf("%s: %v", f, err)
				}
			}
			if bad := unknownKeys(user, defaults, ""); len(bad) > 0 {
				t.Errorf("%s sets %v, which the %s chart's values.yaml does not have: no %s release's user values held them", file, bad, tag, tag)
			}
			if _, err := os.Stat(filepath.Join(chart, "values.schema.json")); errors.Is(err, os.ErrNotExist) {
				// No schema: the key check is the guard, so it must catch a
				// key the chart lacked (server.retention came with v0.2.0).
				if bad := unknownKeys(map[string]any{"server": map[string]any{"retention": "30d", "enabled": true}}, defaults, ""); !slices.Equal(bad, []string{"server.retention"}) {
					t.Errorf("the key check against %s's values.yaml finds %v in {server: {retention, enabled}}, want [server.retention]", tag, bad)
				}
			}
		})
	}
}

// A budget of 0 is a value, not "unset" (a number 0 is falsy in a template),
// and a budget with neither bound is not a budget.
func TestPodDisruptionBudgetBounds(t *testing.T) {
	ha := []string{"server.enabled=true", "server.replicas=2", "server.database.existingSecret=pg"}
	spec := func(extra ...string) map[string]any {
		t.Helper()
		pdb := find(render(t, append(slices.Clone(ha), extra...)...), "PodDisruptionBudget", "upgradescope-server")
		if pdb == nil {
			t.Fatal("no PodDisruptionBudget")
		}
		s, _, _ := unstructured.NestedMap(pdb.Object, "spec")
		return s
	}
	if s := spec("server.podDisruptionBudget.maxUnavailable=0"); fmt.Sprint(s["maxUnavailable"]) != "0" || s["minAvailable"] != nil {
		t.Errorf("maxUnavailable=0 renders %v, want maxUnavailable: 0 alone", s)
	}
	if s := spec("server.podDisruptionBudget.minAvailable=0"); fmt.Sprint(s["minAvailable"]) != "0" || s["maxUnavailable"] != nil {
		t.Errorf("minAvailable=0 renders %v, want minAvailable: 0 alone", s)
	}
	if s := spec("server.podDisruptionBudget.minAvailable=50%"); s["minAvailable"] != "50%" {
		t.Errorf("minAvailable=50%% renders %v", s)
	}
	if s := spec("server.podDisruptionBudget.maxUnavailable=1", "server.podDisruptionBudget.minAvailable=2"); s["minAvailable"] != nil || fmt.Sprint(s["maxUnavailable"]) != "1" {
		t.Errorf("both set renders %v, want maxUnavailable alone", s)
	}
	for _, bad := range [][]string{
		{"server.podDisruptionBudget.minAvailable=", "server.podDisruptionBudget.maxUnavailable="},
		{"server.podDisruptionBudget.minAvailable=null"},
	} {
		if msg := renderErr(t, append(slices.Clone(ha), bad...)...); !strings.Contains(msg, "minAvailable or maxUnavailable") {
			t.Errorf("%v: render error = %q, want one naming both bounds", bad, msg)
		}
	}
	for _, bad := range []string{"-1", "abc", "5%%%"} {
		if msg := renderErr(t, append(slices.Clone(ha), "server.podDisruptionBudget.maxUnavailable="+bad)...); msg == "" {
			t.Errorf("maxUnavailable=%s renders", bad)
		}
	}
	// The budget is only rendered for replicas above one, so an empty one
	// with a single replica is not an error.
	if msg := renderErr(t, "server.enabled=true", "server.podDisruptionBudget.minAvailable="); msg != "" {
		t.Errorf("an empty budget with one replica: %s", msg)
	}
}

// The upgrade and security pages (and the chart README, generated from its
// template) say that a rotation needs no restart, how long it takes and what
// still needs one, and none describes a pod-template checksum of a secret.
func TestDocsDescribeRotationWithoutARestart(t *testing.T) {
	for _, f := range []string{
		"../../docs/operations/upgrade.md",
		"../../docs/operations/security-model-and-rbac.md",
		"../../docs/operations/tenancy.md",
		"../../hack/docs/chart-README.md.gotmpl",
		"README.md",
	} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(raw)), " ")
		// What still needs a restart keeps its command.
		if !strings.Contains(text, "rollout restart") {
			t.Errorf("%s does not say what still needs kubectl rollout restart", f)
		}
		if !strings.Contains(text, "no restart") && !strings.Contains(text, "without a restart") {
			t.Errorf("%s does not say a rotation needs no restart", f)
		}
		if !strings.Contains(text, "60 to 90 s") {
			t.Errorf("%s does not give the rotation latency (60 to 90 seconds)", f)
		}
		for _, bad := range []string{"salted", "carry a checksum of the values", "carries a checksum of the chart-managed", "restarts the server, since the pod template",
			"A follow-up will make", "re-read mounted token files, so that no restart", "read once, when they start, and the chart does not restart"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s still says %q", f, bad)
			}
		}
	}
	// The Deployments are <fullname>-server and <fullname>-agent: a release
	// named foo has foo-upgradescope-server, so deploy/<release>-server names
	// nothing.
	for _, f := range []string{
		"../../docs/operations/upgrade.md",
		"../../docs/operations/security-model-and-rbac.md",
		"../../docs/operations/tenancy.md",
		"../../docs/operations/auth.md",
		"../../hack/docs/chart-README.md.gotmpl",
		"README.md",
	} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "deploy/<release>-") {
			t.Errorf("%s names a Deployment deploy/<release>-...; the chart's are deploy/<fullname>-server and -agent", f)
		}
	}
	// The security page and the upgrade page both give the reason the chart
	// does not hash tokens.
	for _, f := range []string{"../../docs/operations/upgrade.md", "../../docs/operations/security-model-and-rbac.md"} {
		raw, _ := os.ReadFile(f)
		text := strings.Join(strings.Fields(string(raw)), " ")
		if !strings.Contains(text, "tested against it offline") {
			t.Errorf("%s lacks the reason the chart does not hash tokens", f)
		}
	}
}

// The first upgrade to this chart is the exception to "removing a value
// removes its key": an earlier chart wrote the keys as stringData, which Helm
// cannot remove once the API server has turned them into data. The ledger,
// the upgrade page and the chart README must say so and give the cleanup.
func TestDocsQualifyKeyRemovalOnTheFirstUpgrade(t *testing.T) {
	for _, f := range []string{
		"../../docs/claims.md",
		"../../docs/operations/upgrade.md",
		"../../hack/docs/chart-README.md.gotmpl",
		"README.md",
	} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(raw)), " ")
		if f == "../../docs/claims.md" {
			i := strings.Index(text, "| RB-19 |")
			j := strings.Index(text[i:], "| RB-20 |")
			if i < 0 || j < 0 {
				t.Fatal("RB-19 or RB-20 not found in the claims ledger")
			}
			text = text[i : i+j]
		}
		for _, want := range []string{"from this chart version on", "stringData", `"op":"remove"`, "/data/<key>"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not carry the qualification on key removal (missing %q)", f, want)
			}
		}
	}
}
