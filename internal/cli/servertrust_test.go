package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Go reads $SSL_CERT_FILE and $SSL_CERT_DIR only on Linux and other Unix
// systems; on macOS and Windows it uses the platform verifier and ignores
// them, so the help must not offer them as a way to trust a private CA
// everywhere (#372). Release builds cover all three, and CI runs on Linux
// only, so the text is what is pinned.
func TestServerCAUsageQualifiesSSLCertFile(t *testing.T) {
	for _, env := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if !strings.Contains(serverCAUsage, env) {
			t.Errorf("serverCAUsage does not mention %s", env)
		}
	}
	for _, platform := range []string{"Linux", "macOS", "Windows"} {
		if !strings.Contains(serverCAUsage, platform) {
			t.Errorf("serverCAUsage mentions SSL_CERT_FILE without naming %s:\n%s", platform, serverCAUsage)
		}
	}
	if strings.Contains(serverCAUsage, "also adds roots") {
		t.Errorf("serverCAUsage still says $SSL_CERT_FILE adds roots, which is Unix-only:\n%s", serverCAUsage)
	}
}

// No hand-written page offers $SSL_CERT_FILE or $SSL_CERT_DIR without the
// platforms: the paragraph (or table row) that mentions either also names
// Linux, macOS and Windows (#372). CHANGELOG.md is read too, though it is
// not in docPages, and a list item is a unit of its own there, since a
// release's entries form one paragraph.
func TestDocsQualifySSLCertFile(t *testing.T) {
	for _, page := range append(docPages(t), "CHANGELOG.md") {
		if !strings.HasSuffix(page, ".md") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(repoRoot, page))
		if err != nil {
			t.Fatal(err)
		}
		var units []string // a paragraph, or one row of a table
		for _, para := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n\n") {
			if strings.HasPrefix(strings.TrimSpace(para), "|") {
				units = append(units, strings.Split(para, "\n")...)
			} else if strings.HasPrefix(para, "- ") {
				units = append(units, strings.Split(para, "\n- ")...)
			} else {
				units = append(units, para)
			}
		}
		for _, para := range units {
			if !strings.Contains(para, "SSL_CERT_FILE") && !strings.Contains(para, "SSL_CERT_DIR") {
				continue
			}
			for _, platform := range []string{"Linux", "macOS", "Windows"} {
				if !strings.Contains(para, platform) {
					t.Errorf("%s mentions SSL_CERT_FILE or SSL_CERT_DIR without naming %s: %.120s...", page, platform, strings.TrimSpace(para))
				}
			}
		}
	}
}
