package chart

import (
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Tests for #255: serve and the agent read their tokens, webhook URLs and
// signing key from files mounted from the Secrets and re-read them, so a
// rotation needs no restart and no pod annotation carries a function of a
// secret.

func containerArgs(t *testing.T, objs []unstructured.Unstructured, deployment string) []string {
	t.Helper()
	args, _, _ := unstructured.NestedStringSlice(container(t, objs, deployment), "args")
	return args
}

// fileFlags maps each --*-file flag the container passes to the file it names.
func fileFlags(args []string) map[string]string {
	out := map[string]string{}
	re := regexp.MustCompile(`^--([a-z-]+)-file=(.+)$`)
	for _, a := range args {
		if m := re.FindStringSubmatch(a); m != nil {
			out[m[1]] = m[2]
		}
	}
	return out
}

// secretVolume is a Secret volume of the pod: its name, optional flag and
// the key -> path items.
type secretVolume struct {
	secret   string
	optional bool
	items    map[string]string // path -> key
}

func secretVolumes(t *testing.T, objs []unstructured.Unstructured, deployment string) map[string]secretVolume {
	t.Helper()
	vols, _, _ := unstructured.NestedSlice(podTemplate(t, objs, deployment), "spec", "volumes")
	out := map[string]secretVolume{}
	for _, v := range vols {
		m := v.(map[string]any)
		sec, ok := m["secret"].(map[string]any)
		if !ok {
			continue
		}
		sv := secretVolume{items: map[string]string{}}
		sv.secret, _ = sec["secretName"].(string)
		sv.optional, _ = sec["optional"].(bool)
		items, _ := sec["items"].([]any)
		for _, it := range items {
			im := it.(map[string]any)
			sv.items[im["path"].(string)] = im["key"].(string)
		}
		out[m["name"].(string)] = sv
	}
	return out
}

// resolveFile finds the Secret key and Secret behind a file a flag names:
// the file must sit in a mount of a Secret volume that lists it, with no
// subPath (a subPath mount never sees the kubelet's update).
func resolveFile(t *testing.T, objs []unstructured.Unstructured, deployment, file string) (secret, key string, optional bool) {
	t.Helper()
	c := container(t, objs, deployment)
	mounts, _, _ := unstructured.NestedSlice(c, "volumeMounts")
	vols := secretVolumes(t, objs, deployment)
	for _, m := range mounts {
		mm := m.(map[string]any)
		dir, _ := mm["mountPath"].(string)
		if path.Dir(file) != dir {
			continue
		}
		if sp, ok := mm["subPath"]; ok {
			t.Errorf("%s mounts %v with subPath %v: a subPath mount never updates", deployment, mm["name"], sp)
		}
		if ro, _ := mm["readOnly"].(bool); !ro {
			t.Errorf("%s mount %v is not readOnly", deployment, mm["name"])
		}
		v, ok := vols[mm["name"].(string)]
		if !ok {
			t.Fatalf("%s: %s is mounted from a volume that is not a Secret", deployment, file)
		}
		k, ok := v.items[path.Base(file)]
		if !ok {
			t.Fatalf("%s: volume %v has no item for %s (items %v)", deployment, mm["name"], file, v.items)
		}
		return v.secret, k, v.optional
	}
	t.Fatalf("%s: no mount holds %s", deployment, file)
	return "", "", false
}

func TestServerTokensAreMountedFilesNotEnvironment(t *testing.T) {
	objs := render(t, "server.enabled=true", "server.ingestToken=i", "server.readToken=r", "server.adminToken=a",
		"server.slackWebhook=https://s.example.com/x", "server.webhook=https://w.example.com/x", "server.webhookSecret=k")
	args := containerArgs(t, objs, "upgradescope-server")
	want := map[string]string{
		"ingest-token": "ingestToken", "read-token": "readToken", "admin-token": "adminToken",
		"slack-webhook": "slackWebhook", "webhook": "webhook", "webhook-secret": "webhookSecret",
	}
	got := fileFlags(args)
	for flag, key := range want {
		file, ok := got[flag]
		if !ok {
			t.Errorf("serve gets no --%s-file (args %v)", flag, args)
			continue
		}
		secret, k, optional := resolveFile(t, objs, "upgradescope-server", file)
		if secret != "upgradescope-server-tokens" || k != key || optional {
			t.Errorf("--%s-file=%s reads key %q of %q (optional %v), want %q of upgradescope-server-tokens", flag, file, k, secret, optional, key)
		}
	}
	// No secret reaches the container as an environment variable or a flag
	// value: only the database URL, which is opened once, still does.
	env, _, _ := unstructured.NestedSlice(container(t, objs, "upgradescope-server"), "env")
	for _, e := range env {
		if n := e.(map[string]any)["name"].(string); strings.HasPrefix(n, "UPGRADESCOPE_") {
			t.Errorf("serve gets the environment variable %s", n)
		}
	}
	raw, _ := json.Marshal(container(t, objs, "upgradescope-server"))
	if strings.Contains(string(raw), "secretKeyRef") {
		t.Errorf("serve has a secretKeyRef: %s", raw)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--optional-secret-file") {
			t.Errorf("a chart-managed Secret holds every key it lists, yet serve gets %s", a)
		}
	}

	// Postgres: the URL stays an environment variable.
	pg := render(t, "server.enabled=true", "server.database.existingSecret=pg")
	if v, _ := envVar(container(t, pg, "upgradescope-server"), "UPGRADESCOPE_DB_URL"); v != "" {
		t.Error("the database URL has a literal value")
	}
	if _, ok := envVar(container(t, pg, "upgradescope-server"), "UPGRADESCOPE_DB_URL"); !ok {
		t.Error("the Postgres URL no longer reaches serve")
	}
}

func TestServerExistingSecretKeysThatMayBeAbsentAreOptional(t *testing.T) {
	// Hub: no in-chart agent, so the ingest token may be absent too.
	objs := render(t, "server.enabled=true", "agent.enabled=false", "server.existingSecret=ex",
		"server.readTokenFromSecret=true", "server.adminTokenFromSecret=true")
	args := containerArgs(t, objs, "upgradescope-server")
	got := fileFlags(args)
	wantOptional := map[string]bool{
		"ingest-token": true, "slack-webhook": true, "webhook": true, "webhook-secret": true,
		"read-token": false, "admin-token": false,
	}
	for flag, optional := range wantOptional {
		file, ok := got[flag]
		if !ok {
			t.Errorf("no --%s-file in %v", flag, args)
			continue
		}
		secret, _, isOptional := resolveFile(t, objs, "upgradescope-server", file)
		if secret != "ex" || isOptional != optional {
			t.Errorf("--%s-file: Secret %q optional %v, want ex and %v", flag, secret, isOptional, optional)
		}
	}
	if !slices.Contains(args, "--optional-secret-file=ingest-token,slack-webhook,webhook,webhook-secret") {
		t.Errorf("serve is not told which files may be absent: %v", args)
	}

	// With the in-chart agent the ingest token is required, as it was.
	objs = render(t, "server.enabled=true", "server.existingSecret=ex")
	file := fileFlags(containerArgs(t, objs, "upgradescope-server"))["ingest-token"]
	if _, _, optional := resolveFile(t, objs, "upgradescope-server", file); optional {
		t.Error("the ingest token is optional though the in-chart agent pushes with it")
	}
	if a := containerArgs(t, objs, "upgradescope-server"); slices.Contains(a, "--optional-secret-file=ingest-token,slack-webhook,webhook,webhook-secret") {
		t.Errorf("ingest-token marked optional: %v", a)
	}
}

func TestAgentPushTokenIsAMountedFile(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sets         []string
		secret, key  string
		wantNoServer bool
	}{
		{"in-chart server's ingest token", []string{"server.enabled=true"}, "upgradescope-server-tokens", "ingestToken", false},
		{"server with an existing Secret", []string{"server.enabled=true", "server.existingSecret=ex"}, "ex", "ingestToken", false},
		{"inline token", []string{"agent.serverUrl=https://hub.example.com", "agent.serverToken=t"}, "upgradescope-agent-token", "serverToken", true},
		{"existing Secret", []string{"agent.serverUrl=https://hub.example.com", "agent.existingSecret=mine"}, "mine", "serverToken", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := render(t, tc.sets...)
			args := containerArgs(t, objs, "upgradescope-agent")
			file, ok := fileFlags(args)["server-token"]
			if !ok {
				t.Fatalf("the agent gets no --server-token-file: %v", args)
			}
			secret, key, optional := resolveFile(t, objs, "upgradescope-agent", file)
			if secret != tc.secret || key != tc.key || optional {
				t.Errorf("agent token file is key %q of %q (optional %v), want %q of %q", key, secret, optional, tc.key, tc.secret)
			}
			env, _, _ := unstructured.NestedSlice(container(t, objs, "upgradescope-agent"), "env")
			for _, e := range env {
				if n := e.(map[string]any)["name"].(string); strings.HasPrefix(n, "UPGRADESCOPE_") {
					t.Errorf("the agent gets the environment variable %s", n)
				}
			}
			for _, a := range args {
				if strings.HasPrefix(a, "--server-token=") {
					t.Errorf("the agent gets the token as a flag: %s", a)
				}
			}
		})
	}
	// CRD-only: no push, no token, no volume.
	objs := render(t)
	if _, ok := fileFlags(containerArgs(t, objs, "upgradescope-agent"))["server-token"]; ok {
		t.Error("a CRD-only agent gets --server-token-file")
	}
	if len(secretVolumes(t, objs, "upgradescope-agent")) != 0 {
		t.Error("a CRD-only agent mounts a Secret")
	}
}

// Every value set that carries a secret renders the same pod template
// metadata whatever the secret is, and no object but a Secret holds the
// secret itself or a hash of it (a digest of any of the usual kinds): the
// pods roll on neither, and a pod or Deployment reader learns nothing.
func TestNoSecretDerivedValueInPodMetadata(t *testing.T) {
	registryValues := writeValues(t, extraRegistryValues)
	secrets := func(v string) map[string][]string {
		ingest, read, admin := "ingest-"+v+"zq9", "read-"+v+"zq9", "admin-"+v+"zq9"
		slack, hook, sign := "https://hooks.example.com/slack-"+v, "https://hooks.example.com/hook-"+v, "sign-"+v+"zq9"
		push := "push-" + v + "zq9"
		return map[string][]string{
			"in-chart server, every secret": {"server.enabled=true", "server.ingestToken=" + ingest, "server.readToken=" + read, "server.adminToken=" + admin,
				"server.slackWebhook=" + slack, "server.webhook=" + hook, "server.webhookSecret=" + sign},
			"server with a team map and TLS": {"server.enabled=true", "server.ingestToken=" + ingest, "server.readToken=" + read,
				"server.teamMap[0].pattern=a-*", "server.teamMap[0].team=a", "server.tls.secretName=tls"},
			"remote agent": {"agent.serverUrl=https://hub.example.com", "agent.serverToken=" + push},
			"server and an agent token of its own": {"server.enabled=true", "server.sharedIngestToken=false", "agent.serverToken=" + push, "server.readToken=" + read},
			"existing Secrets":                     {"server.enabled=true", "server.existingSecret=ex-" + v, "server.readTokenFromSecret=true", "server.adminTokenFromSecret=true", "agent.existingSecret=agent-" + v},
			"agent with extra registry":            {"agent.serverUrl=https://hub.example.com", "agent.serverToken=" + push, registryValues},
		}
	}
	a, b := secrets("one"), secrets("two")
	hex := regexp.MustCompile(`[0-9a-fA-F]{32,}`)
	b64 := regexp.MustCompile(`[A-Za-z0-9+/=_-]{40,}`)
	for name, sets := range a {
		t.Run(name, func(t *testing.T) {
			objsA, objsB := render(t, sets...), render(t, b[name]...)
			if len(objsA) != len(objsB) {
				t.Fatalf("%d and %d objects", len(objsA), len(objsB))
			}
			for i := range objsA {
				if objsA[i].GetKind() == "Secret" {
					continue
				}
				// The existing Secrets' names are not secrets, and differ
				// between the two renders only here.
				ja, _ := objsA[i].MarshalJSON()
				jb, _ := objsB[i].MarshalJSON()
				ja, jb = []byte(strings.NewReplacer("-one", "", "-two", "").Replace(string(ja))), []byte(strings.NewReplacer("-one", "", "-two", "").Replace(string(jb)))
				if string(ja) != string(jb) {
					t.Errorf("%s/%s depends on a secret value:\n%s\n%s", objsA[i].GetKind(), objsA[i].GetName(), ja, jb)
				}
				text := string(ja)
				for _, bad := range []string{"zq9", "https://hooks.example.com/"} {
					if strings.Contains(text, bad) {
						t.Errorf("%s/%s carries a secret value (%q)", objsA[i].GetKind(), objsA[i].GetName(), bad)
					}
				}
				if objsA[i].GetKind() != "Deployment" {
					continue
				}
				tpl := podTemplate(t, objsA, objsA[i].GetName())
				ann, _, _ := unstructured.NestedStringMap(tpl, "metadata", "annotations")
				labels, _, _ := unstructured.NestedStringMap(tpl, "metadata", "labels")
				for _, m := range []map[string]string{ann, labels, objsA[i].GetAnnotations()} {
					for k, v := range m {
						if hex.MatchString(v) || b64.MatchString(v) {
							// The two checksums of non-secret config are the exception.
							if k == "checksum/team-map" || k == "checksum/registry" {
								continue
							}
							t.Errorf("%s: %s = %q looks like a digest", objsA[i].GetName(), k, v)
						}
						if strings.Contains(k, "checksum") && k != "checksum/team-map" && k != "checksum/registry" {
							t.Errorf("%s has the checksum annotation %s", objsA[i].GetName(), k)
						}
					}
				}
			}
		})
	}
}

// With the default threshold (server.staleAfter empty: the larger of 2h and
// three intervals), a force-sync period set in agent.extraArgs that pushes
// the gap between an unchanged cluster's pushes past it makes healthy
// clusters read stale: the NOTES say so, and say nothing at the defaults.
func TestNotesWarnOfADefaultThresholdBelowTheForceSync(t *testing.T) {
	notes := strings.Join(strings.Fields(renderNotes(t, "upgradescope", "server.enabled=true", "agent.extraArgs={--force-sync-every=4h}")), " ")
	want := "WARNING: the default stale threshold (7200s) is below 15000s, the larger of agent.interval (10m) and the force-sync period (14400s, agent.extraArgs --force-sync-every), plus one interval."
	if !strings.Contains(notes, want) {
		t.Errorf("NOTES lack %q:\n%s", want, notes)
	}
	if plain := renderNotes(t, "upgradescope", "server.enabled=true"); strings.Contains(plain, "default stale threshold") {
		t.Errorf("the defaults are warned about:\n%s", plain)
	}
	// An unreadable value counts as the agent's default of 1h.
	if odd := renderNotes(t, "upgradescope", "server.enabled=true", "agent.extraArgs={--force-sync-every=soon}"); strings.Contains(odd, "default stale threshold") {
		t.Errorf("an unreadable force-sync is warned about:\n%s", odd)
	}
}
