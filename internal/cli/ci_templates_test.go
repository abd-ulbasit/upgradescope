package cli

import (
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The CI templates under ci/ (#73) are copied as they are, and CI runs
// neither Jenkins nor Azure Pipelines, so these keep their shape: a pinned
// release (the one the docs pin) checked against checksums.txt, an
// unpiped gate that writes the JUnit report, and a publishing step that
// runs when the gate fails and reads that file. GitLab's template is
// TestDocsGitLabJob's.

// pinnedVersion is the release docs/guides/other-ci.md installs.
func pinnedVersion(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^VERSION=(v\d+\.\d+\.\d+)$`).FindStringSubmatch(readDoc(t, "docs/guides/other-ci.md"))
	if m == nil {
		t.Fatal("docs/guides/other-ci.md: no VERSION=vX.Y.Z in the install section")
	}
	return m[1]
}

// checkInstall checks the shell lines that install the release.
func checkInstall(t *testing.T, file, script string) {
	t.Helper()
	for _, want := range []string{"releases/download/", "checksums.txt", "sha256sum -c", "tar -xzf upgradescope_linux_amd64.tar.gz upgradescope"} {
		if !strings.Contains(script, want) {
			t.Errorf("%s: the install does not contain %q", file, want)
		}
	}
	if strings.Contains(script, "--ignore-missing") {
		t.Errorf("%s: --ignore-missing is GNU-only; check the one archive's line instead", file)
	}
}

// gateFile returns the file a gate line writes JUnit to, failing t when
// the line is not an unpiped `upgradescope scan --output junit`.
func gateFile(t *testing.T, file, line string) string {
	t.Helper()
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "./upgradescope scan ") || !strings.Contains(line, "--output junit") || strings.Contains(line, "|") {
		t.Errorf("%s: the gate %q should be an unpiped ./upgradescope scan --output junit", file, line)
	}
	_, out, ok := strings.Cut(line, " > ")
	if !ok {
		t.Errorf("%s: the gate %q does not write its report to a file", file, line)
	}
	return strings.Fields(out + " ")[0]
}

func TestCITemplateJenkins(t *testing.T) {
	const file = "ci/jenkins/Jenkinsfile"
	src := readDoc(t, file)
	if !strings.Contains(src, "UPGRADESCOPE_VERSION = '"+pinnedVersion(t)+"'") {
		t.Errorf("%s does not pin %s, the release docs/guides/other-ci.md installs", file, pinnedVersion(t))
	}
	install := regexp.MustCompile(`(?s)sh '''(.*?)'''`).FindStringSubmatch(src)
	if install == nil {
		t.Fatalf("%s: no sh ''' block installing the release", file)
	}
	checkInstall(t, file, install[1])
	gate := regexp.MustCompile(`sh '(\./upgradescope scan [^']*)'`).FindStringSubmatch(src)
	if gate == nil {
		t.Fatalf("%s: no sh step running the gate", file)
	}
	report := gateFile(t, file, gate[1])
	// The junit step must run when the gate fails: in a post { always }.
	_, post, ok := strings.Cut(src, "post {")
	if _, always, ok2 := strings.Cut(post, "always {"); !ok || !ok2 || !strings.Contains(always, "junit testResults: '"+report+"'") {
		t.Errorf("%s: no junit testResults: '%s' step under post { always }", file, report)
	}
}

func TestCITemplateAzure(t *testing.T) {
	const file = "ci/azure/azure-pipelines.yml"
	var p struct {
		Variables map[string]string `json:"variables"`
		Steps     []struct {
			Script    string            `json:"script"`
			Task      string            `json:"task"`
			Condition string            `json:"condition"`
			Inputs    map[string]string `json:"inputs"`
		} `json:"steps"`
	}
	if err := yaml.Unmarshal([]byte(readDoc(t, file)), &p); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	if v := p.Variables["UPGRADESCOPE_VERSION"]; v != pinnedVersion(t) {
		t.Errorf("%s pins %q, docs/guides/other-ci.md installs %s", file, v, pinnedVersion(t))
	}
	var report string
	published := false
	for _, s := range p.Steps {
		switch {
		case strings.Contains(s.Script, "checksums.txt"):
			// A multi-line script step runs without -e: a failed check
			// would not stop it.
			if !strings.HasPrefix(s.Script, "set -euo pipefail\n") {
				t.Errorf("%s: the install step does not start with set -euo pipefail", file)
			}
			checkInstall(t, file, s.Script)
		case strings.Contains(s.Script, "upgradescope scan"):
			report = gateFile(t, file, s.Script)
		case s.Task == "PublishTestResults@2":
			published = true
			if report == "" || s.Inputs["testResultsFiles"] != report || s.Inputs["testResultsFormat"] != "JUnit" {
				t.Errorf("%s: PublishTestResults reads %q as %q, want the gate's report %q as JUnit, after the gate", file,
					s.Inputs["testResultsFiles"], s.Inputs["testResultsFormat"], report)
			}
			if s.Condition != "succeededOrFailed()" && s.Condition != "always()" {
				t.Errorf("%s: PublishTestResults has condition %q; it must run when the gate fails", file, s.Condition)
			}
		}
	}
	if !published {
		t.Errorf("%s: no PublishTestResults@2 step", file)
	}
}
