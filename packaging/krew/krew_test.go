// Package krew_test renders the repository's .krew.yaml template the way
// rajatjindal/krew-release-bot does (text/template with its indent and
// addURIAndSha functions) and checks that the result is the krew plugin
// manifest krew-index expects. The bot downloads each archive to compute
// its sha256; here a fixture checksums table stands in, so the test is
// offline.
package krew_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"text/template"

	"sigs.k8s.io/yaml"
)

const tag = "v0.2.0"

type plugin struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Version          string `json:"version"`
		Homepage         string `json:"homepage"`
		ShortDescription string `json:"shortDescription"`
		Description      string `json:"description"`
		Caveats          string `json:"caveats"`
		Platforms        []struct {
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
			URI    string `json:"uri"`
			Sha256 string `json:"sha256"`
			Bin    string `json:"bin"`
		} `json:"platforms"`
	} `json:"spec"`
}

// render is krew-release-bot's RenderTemplate (pkg/source/template.go at
// v0.0.51) with the download replaced by sums.
func render(t *testing.T, sums map[string]string) []byte {
	t.Helper()
	indent := func(spaces int, v string) string {
		v = strings.ReplaceAll(v, "    sha256:", "sha256:")
		pad := strings.Repeat(" ", spaces)
		return strings.TrimSpace(pad + strings.ReplaceAll(v, "\n", "\n"+pad))
	}
	addURIAndSha := func(url, tag string) (string, error) {
		var buf bytes.Buffer
		if err := template.Must(template.New("url").Parse(url)).Execute(&buf, struct{ TagName string }{tag}); err != nil {
			return "", err
		}
		sum, ok := sums[buf.String()]
		if !ok {
			return "", fmt.Errorf("no release asset %s", buf.String())
		}
		return fmt.Sprintf("uri: %s\n    sha256: %s", buf.String(), sum), nil
	}
	tmpl, err := template.New(".krew.yaml").Funcs(template.FuncMap{"indent": indent, "addURIAndSha": addURIAndSha}).ParseFiles("../../.krew.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, struct{ TagName string }{tag}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestKrewManifest(t *testing.T) {
	want := map[string]string{} // os/arch -> asset
	sums := map[string]string{}
	for _, p := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"} {
		os, arch, _ := strings.Cut(p, "/")
		ext := ".tar.gz"
		if os == "windows" {
			ext = ".zip"
		}
		// The release's version-independent archive names (.goreleaser.yml).
		asset := fmt.Sprintf("https://github.com/abd-ulbasit/upgradescope/releases/download/%s/upgradescope_%s_%s%s", tag, os, arch, ext)
		want[p] = asset
		sums[asset] = fmt.Sprintf("%x", sha256.Sum256([]byte(asset)))
	}

	raw := render(t, sums)
	if bytes.Contains(raw, []byte("TEMPLATE")) {
		t.Errorf("the header comment leaked into the manifest:\n%s", raw)
	}
	var p plugin
	if err := yaml.UnmarshalStrict(raw, &p); err != nil {
		t.Fatalf("rendered manifest is not a valid plugin: %v\n%s", err, raw)
	}
	if p.APIVersion != "krew.googlecontainertools.github.com/v1alpha2" || p.Kind != "Plugin" || p.Metadata.Name != "upgradescope" {
		t.Errorf("apiVersion/kind/name = %s/%s/%s", p.APIVersion, p.Kind, p.Metadata.Name)
	}
	if p.Spec.Version != tag {
		t.Errorf("version = %q, want %q", p.Spec.Version, tag)
	}
	// krew-index: shortDescription is one short line.
	if n := len(p.Spec.ShortDescription); n == 0 || n > 50 || strings.Contains(p.Spec.ShortDescription, "\n") {
		t.Errorf("shortDescription %q: want one line of at most 50 characters", p.Spec.ShortDescription)
	}
	if !strings.Contains(p.Spec.Description, "kubectl upgradescope scan") {
		t.Error("description has no kubectl upgradescope example")
	}
	for _, needle := range []string{"/metrics", "nodes", "NOT ASSESSED", "--allow-incomplete"} {
		if !strings.Contains(p.Spec.Caveats, needle) {
			t.Errorf("caveats do not mention %q (the RBAC the scan needs)", needle)
		}
	}

	got := map[string]bool{}
	for _, pl := range p.Spec.Platforms {
		key := pl.Selector.MatchLabels["os"] + "/" + pl.Selector.MatchLabels["arch"]
		got[key] = true
		if pl.URI != want[key] {
			t.Errorf("%s: uri = %q, want %q", key, pl.URI, want[key])
		}
		if pl.Sha256 != sums[want[key]] {
			t.Errorf("%s: sha256 = %q, want the asset's %q", key, pl.Sha256, sums[want[key]])
		}
		wantBin := "upgradescope"
		if strings.HasPrefix(key, "windows/") {
			wantBin += ".exe"
		}
		if pl.Bin != wantBin {
			t.Errorf("%s: bin = %q, want %q", key, pl.Bin, wantBin)
		}
	}
	for k := range want {
		if !got[k] {
			t.Errorf("no platform for %s", k)
		}
	}
	if len(p.Spec.Platforms) != len(want) {
		t.Errorf("%d platforms, want %d", len(p.Spec.Platforms), len(want))
	}
}

// The template names exactly the archives GoReleaser builds.
func TestKrewArchivesMatchGoReleaser(t *testing.T) {
	gr, err := os.ReadFile("../../.goreleaser.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(gr, []byte(`name_template: '{{ .ProjectName }}_{{ .Os }}_{{ .Arch }}'`)) {
		t.Error(".goreleaser.yml archive name_template changed; update .krew.yaml (and packaging/homebrew-tap) with it")
	}
}
