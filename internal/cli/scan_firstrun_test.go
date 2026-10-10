package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// chartTree is a chart directory scanned by mistake instead of its render:
// two templates that cannot be decoded, one of them with an apiVersion
// that is itself a template.
var chartTree = map[string]string{
	"Chart.yaml":            "apiVersion: v2\nname: demo\nversion: 0.1.0\n",
	"values.yaml":           "replicaCount: 1\n",
	"templates/deploy.yaml": "{{- if .Values.on }}\napiVersion: apps/v1\nkind: Deployment\n{{- end }}\n",
	"templates/hpa.yaml":    "apiVersion: {{ include \"hpa.apiVersion\" . }}\nkind: HorizontalPodAutoscaler\n",
	"templates/cm.yaml":     "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n",
}

// Scanning an unrendered chart directory never reads READY (#335): one Helm
// hint replaces the per-file warnings, the report says how many templates
// were not read, and the gate fails as it does for any unknown verdict.
func TestScanUnrenderedChartIsUnknownWithOneHint(t *testing.T) {
	files := map[string]string{"rendered/ok.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: ok}\n"}
	for name, content := range chartTree {
		files["chart/"+name] = content
	}
	dir := writeFiles(t, files)
	chartDir := filepath.ToSlash(filepath.Join(dir, "chart"))

	out, stderr, err := execScanFiles(t, "--files", dir)
	if !errors.Is(err, ErrIncomplete) || ExitCode(err) != 2 {
		t.Fatalf("err = %v (exit %d), want ErrIncomplete, exit 2: the unknown verdict", err, ExitCode(err))
	}
	if strings.Contains(out, "READY  yes") || !strings.Contains(out, "READY  unknown") {
		t.Errorf("report does not read unknown:\n%s", out)
	}
	if want := "NOT ASSESSED\n  api-usage (required): 3 Helm template files under "; !strings.Contains(out, want) {
		t.Errorf("NOT ASSESSED lacks the template count (%q):\n%s", want, out)
	}
	hint := "warning: " + chartDir + " looks like an unrendered Helm chart (Chart.yaml found, 3 templates contain {{ }}); render it first: helm template NAME " + chartDir + " --output-dir rendered\n"
	if stderr != hint {
		t.Errorf("stderr = %q, want exactly the one hint %q", stderr, hint)
	}
}

// One template under a Chart.yaml is enough, and a JSON report carries the
// required api-usage gap.
func TestScanUnrenderedChartOneTemplateJSON(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"Chart.yaml":            chartTree["Chart.yaml"],
		"templates/deploy.yaml": chartTree["templates/deploy.yaml"],
	})
	out, stderr, err := execScanFiles(t, "--files", dir, "-o", "json")
	// Nothing was read at all: the scan refuses, with the hint twice
	// over (stderr, and the error).
	if err == nil || !strings.Contains(err.Error(), "unrendered Helm templates") || !strings.Contains(err.Error(), "helm template NAME") || ExitCode(err) != 1 {
		t.Fatalf("err = %v, want exit 1 naming Helm; out %q", err, out)
	}
	if strings.Count(stderr, "looks like an unrendered Helm chart") != 1 || !strings.Contains(stderr, "1 template contains {{ }}") {
		t.Errorf("stderr = %q, want the one hint", stderr)
	}

	dir = writeFiles(t, map[string]string{
		"Chart.yaml":            chartTree["Chart.yaml"],
		"templates/deploy.yaml": chartTree["templates/deploy.yaml"],
		"plain.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: ok}\n",
	})
	out, _, err = execScanFiles(t, "--files", dir, "-o", "json")
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
	var rep struct {
		Verdict     string `json:"verdict"`
		Ready       bool   `json:"ready"`
		NotAssessed []struct {
			Capability string `json:"capability"`
			Reason     string `json:"reason"`
			Required   bool   `json:"required"`
		} `json:"notAssessed"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	found := false
	for _, g := range rep.NotAssessed {
		found = found || g.Capability == "api-usage" && g.Required && strings.Contains(g.Reason, "1 Helm template file under ")
	}
	if rep.Verdict != "unknown" || rep.Ready || !found {
		t.Errorf("verdict %q ready %v notAssessed %+v; want unknown with the required api-usage gap", rep.Verdict, rep.Ready, rep.NotAssessed)
	}
}

// Templated files outside any chart are one count line and change nothing:
// the verdict is what the rest of the scan says.
func TestScanTemplatedOutsideChartIsOneCountLine(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"ok.yaml":      "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: ok}\n",
		"t/one.yaml":   "{{- if .Values.on }}\nkind: X\n{{- end }}\n",
		"t/two.yaml":   "apiVersion: v1\nkind: Secret\nmetadata:\n  name: {{ .Values.name }}\n",
		"ci/flow.yaml": "on: push\nenv: ${{ secrets.X }}\n",
	})
	out, stderr, err := execScanFiles(t, "--files", dir)
	if err != nil {
		t.Fatalf("err = %v, want a passing scan: templates outside a chart do not change the verdict", err)
	}
	if !strings.Contains(out, "READY  yes") {
		t.Errorf("report does not read ready:\n%s", out)
	}
	if want := "warning: skipped 2 files containing {{ }}, outside any Helm chart (Chart.yaml): unrendered templates?\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

// A file that is not a template and cannot be parsed is still warned about
// by name, in a chart or out of it.
func TestScanBadYAMLStillWarnedPerFile(t *testing.T) {
	files := map[string]string{"ci/broken.yaml": "key: [unclosed\nother: value\n", "ci/bad.yaml": "a: b: c\n",
		"ok.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: ok}\n"}
	for name, content := range chartTree {
		files["chart/"+name] = content
	}
	dir := writeFiles(t, files)
	_, stderr, err := execScanFiles(t, "--files", dir)
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
	base := filepath.ToSlash(dir)
	for _, f := range []string{"ci/broken.yaml", "ci/bad.yaml"} {
		if !strings.Contains(stderr, "warning: skipped "+base+"/"+f+":1: ") {
			t.Errorf("stderr lacks the warning for %s:\n%s", f, stderr)
		}
	}
	if strings.Count(stderr, "warning: skipped ") != 2 {
		t.Errorf("stderr has a warning per template too:\n%s", stderr)
	}
}

// A missing --target says what to pass and where to look it up (#336), and
// the flag help gives the KB horizon as its example.
func TestScanMissingTargetNamesAnExample(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	horizon := k.MaxKnownK8s.String()

	cmd := newScanCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--files", t.TempDir()})
	err = cmd.Execute()
	want := "--target is required: the Kubernetes minor to upgrade to, e.g. --target " + horizon + " (the newest this build knows; see upgradescope version)"
	if err == nil || ErrorText(err) != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if ExitCode(err) != 1 {
		t.Errorf("exit = %d, want 1", ExitCode(err))
	}
	if usage := newScanCmd().Flags().Lookup("target").Usage; !strings.Contains(usage, "e.g. "+horizon+" ") {
		t.Errorf("--target help = %q, want the KB horizon %s as its example", usage, horizon)
	}

	// An empty --target is the same mistake.
	cmd = newScanCmd()
	cmd.SetArgs([]string{"--target", "", "--files", t.TempDir()})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err == nil || ErrorText(err) != want {
		t.Errorf("--target \"\": err = %v, want %q", err, want)
	}
}

// With no kubeconfig the live scan says where it looked and offers --files
// (#337), and never mentions KUBERNETES_MASTER.
func TestScanNoKubeconfigNamesPathsAndFilesAlternative(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	// client-go reads the home directory once, at start-up: point its
	// default kubeconfig at the temp dir, never at the real one.
	home := filepath.Join(dir, "home")
	oldHome := clientcmd.RecommendedHomeFile
	clientcmd.RecommendedHomeFile = filepath.Join(home, ".kube", "config")
	t.Cleanup(func() { clientcmd.RecommendedHomeFile = oldHome })

	for name, tc := range map[string]struct {
		env, flag, want string
	}{
		"$KUBECONFIG file missing": {env: missing, want: "$KUBECONFIG: " + missing},
		"$KUBECONFIG empty config": {env: empty, want: "$KUBECONFIG: " + empty},
		"--kubeconfig missing":     {flag: missing, want: "--kubeconfig " + missing},
		"default location":         {want: filepath.Join(home, ".kube", "config")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("KUBECONFIG", tc.env)
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			_, _, err := scanRESTConfig(tc.flag, "", 0)
			if err == nil {
				t.Fatal("want an error")
			}
			got := ErrorText(err)
			for _, want := range []string{tc.want, "--kubeconfig <file>", "--context <name>", "upgradescope scan --files <dir> --target "} {
				if !strings.Contains(got, want) {
					t.Errorf("error %q lacks %q", got, want)
				}
			}
			if strings.Contains(got, "KUBERNETES_MASTER") || strings.Contains(got, "\n") {
				t.Errorf("error %q must be one line without KUBERNETES_MASTER", got)
			}
			if ExitCode(err) != 1 {
				t.Errorf("exit = %d, want 1", ExitCode(err))
			}
		})
	}
}

// A kubeconfig that has contexts but no current-context was found: the scan
// does not say "no kubeconfig found", it lists the contexts to pick from.
func TestScanKubeconfigWithoutCurrentContextListsContexts(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config")
	const kc = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://127.0.0.1:1"}
users:
- name: u
  user: {token: t}
contexts:
- name: prod
  context: {cluster: c, user: u}
- name: dev
  context: {cluster: c, user: u}
`
	if err := os.WriteFile(cfg, []byte(kc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", "")
	_, _, err := scanRESTConfig(cfg, "", 0)
	if err == nil {
		t.Fatal("want an error")
	}
	got := ErrorText(err)
	for _, want := range []string{"--kubeconfig " + cfg, "sets no current-context", "--context <name> (dev, prod)", "kubectl config use-context"} {
		if !strings.Contains(got, want) {
			t.Errorf("error %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "no kubeconfig found") || strings.Contains(got, "KUBERNETES_MASTER") {
		t.Errorf("error %q says no kubeconfig was found, or names KUBERNETES_MASTER", got)
	}
	if ExitCode(err) != 1 {
		t.Errorf("exit = %d, want 1", ExitCode(err))
	}
	// --context picks one, and the config loads.
	if _, ctx, err := scanRESTConfig(cfg, "dev", 0); err != nil || ctx != "dev" {
		t.Errorf("--context dev: ctx %q, err %v; want dev, nil", ctx, err)
	}
}

// A mistyped subcommand prints its suggestion on lines of its own (#338)
// with a typed error, and the typed name is still escaped (#245).
func TestUnknownCommandLayout(t *testing.T) {
	run := func(args ...string) error {
		root := Root()
		root.SetArgs(args)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		return Execute(root)
	}
	err := run("scann")
	want := "unknown command \"scann\" for \"upgradescope\"\n\nDid you mean this?\n\tscan"
	if got := ErrorText(err); got != want {
		t.Errorf("ErrorText = %q, want %q", got, want)
	}
	if ExitCode(err) != 1 {
		t.Errorf("exit = %d, want 1", ExitCode(err))
	}
	// Tests and callers that read the error itself still find cobra's text.
	if !strings.Contains(err.Error(), "unknown command \"scann\" for \"upgradescope\"") {
		t.Errorf("Error() = %q", err.Error())
	}

	// The usual way to mistype a subcommand is with its flags after it: the
	// root has none of them, yet the refusal is still the unknown command
	// with its suggestion, not an unknown flag (#338).
	for _, args := range [][]string{
		{"scann", "--target", "1.36", "--files", "x"},
		{"scann", "--target=1.36"},
		{"scann", "-o", "json"},
		{"--target", "1.36", "scann"},
		{"scann", "--files", ".", "--bogus"},
	} {
		if got := ErrorText(run(args...)); got != want {
			t.Errorf("%q: ErrorText = %q, want %q", args, got, want)
		}
		if ExitCode(run(args...)) != 1 {
			t.Errorf("%q: exit = %d, want 1", args, ExitCode(run(args...)))
		}
	}

	// A mistyped flag of the root itself is still a flag error, with the
	// usage pointer, not an unknown command.
	for _, args := range [][]string{{"--bogus"}, {"-o", "json"}} {
		got := ErrorText(run(args...))
		if strings.Contains(got, "unknown command") || !strings.Contains(got, "unknown") || !strings.Contains(got, "run 'upgradescope --help' for usage") {
			t.Errorf("%q: ErrorText = %q, want an unknown flag error", args, got)
		}
	}

	// No suggestion, no suggestion block.
	if got := ErrorText(run("zzzzzzzzzz")); got != "unknown command \"zzzzzzzzzz\" for \"upgradescope\"" {
		t.Errorf("ErrorText = %q", got)
	}

	// The typed name is attacker-influenced: a newline or ESC in it never
	// reaches the terminal as itself, only the layout's own newlines do.
	for _, name := range []string{"sc\nan", "scan\x1b[2J", "sc\ran", "scan\u202e"} {
		got := ErrorText(run(name))
		for _, r := range strings.ReplaceAll(strings.ReplaceAll(got, "\n", ""), "\t", "") {
			if r < 0x20 || r == 0x7f || (r >= 0x202a && r <= 0x202e) {
				t.Errorf("ErrorText(%q) = %q holds the control character %U", name, got, r)
			}
		}
		if !strings.HasPrefix(got, "unknown command \"") || strings.Count(got, "\n") > 4 {
			t.Errorf("ErrorText(%q) = %q: the name's newline must not add lines", name, got)
		}
	}
	if got := ErrorText(run("sc\nan")); !strings.HasPrefix(got, "unknown command \"sc\\nan\" for \"upgradescope\"\n\nDid you mean this?\n\tscan") {
		t.Errorf("ErrorText = %q, want the name escaped", got)
	}
}

// A root command with no argument still prints its help, as before.
func TestRootWithoutArgumentsPrintsHelp(t *testing.T) {
	root := Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(nil)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Available Commands:") {
		t.Errorf("output = %q, want the help", out.String())
	}
}

// -o is the shorthand of --output, the output format, on every command that
// has one (#339), and nothing else uses -o.
func TestOutputShorthandIsFormatEverywhere(t *testing.T) {
	root := Root()
	root.InitDefaultCompletionCmd()
	seen := 0
	walk(root, func(c *cobra.Command) {
		for _, fs := range []*pflag.FlagSet{c.Flags(), c.PersistentFlags()} {
			fs.VisitAll(func(f *pflag.Flag) {
				if f.Name == "output" {
					seen++
					if f.Shorthand != "o" {
						t.Errorf("%s --output has shorthand %q, want o", c.CommandPath(), f.Shorthand)
					}
				} else if f.Shorthand == "o" {
					t.Errorf("%s --%s uses -o, which is for --output", c.CommandPath(), f.Name)
				}
			})
		}
	})
	if seen < 2 {
		t.Errorf("found %d --output flags, want at least scan and version", seen)
	}

	dir := writeFiles(t, map[string]string{"ok.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: ok}\n"})
	out, _, err := execScanFiles(t, "--files", dir, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("scan -o json is not JSON: %v\n%s", err, out)
	}
	cmd := newScanCmd()
	var help bytes.Buffer
	cmd.SetOut(&help)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(help.String(), "-o, --output string") {
		t.Errorf("scan --help lacks -o, --output:\n%s", help.String())
	}
	vc := newVersionCmd()
	var vout bytes.Buffer
	vc.SetOut(&vout)
	vc.SetArgs([]string{"-o", "json"})
	if err := vc.Execute(); err != nil || !json.Valid(vout.Bytes()) {
		t.Errorf("version -o json: err %v, output %q", err, vout.String())
	}
}

// The root command's examples target the knowledge base's horizon, never a
// minor written into the help text.
func TestRootExamplesTargetTheHorizon(t *testing.T) {
	ex := Root().Example
	if want := "--target " + targetExample(); strings.Count(ex, want) != 2 || strings.Contains(ex, "HORIZON") {
		t.Errorf("root examples %q: want %q twice", ex, want)
	}
}
