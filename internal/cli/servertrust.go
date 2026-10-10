package cli

import (
	"crypto/x509"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/upgradescope/internal/agent"
)

// serverCAUsage is the help of --server-ca-file on the commands that call a
// server's API (clusters, mcp --server-url). The agent's own flag says the
// same for pushes.
const serverCAUsage = "PEM bundle of a private CA that issued the server's certificate, trusted on top of the system roots " +
	"(for an https server behind a private CA; $SSL_CERT_FILE also adds roots, for the whole process); needs an https server URL. " +
	"Verification is never skipped"

// registerServerCAFlag adds --server-ca-file to cmd.
func registerServerCAFlag(cmd *cobra.Command, file *string) {
	cmd.Flags().StringVar(file, "server-ca-file", "", serverCAUsage)
}

// validateServerCA checks a --server-ca-file against the server URL flag
// (urlFlag, "--server-url"), as the agent does: the CA only verifies the
// server, so it needs one, and there is no certificate to verify over plain
// http. An empty file is always fine.
func validateServerCA(file, urlFlag, serverURL string) error {
	if file == "" {
		return nil
	}
	if serverURL == "" {
		return fmt.Errorf("--server-ca-file needs %s: it only verifies the server the commands call", urlFlag)
	}
	if !strings.HasPrefix(strings.ToLower(serverURL), "https://") {
		return fmt.Errorf("--server-ca-file needs an https %s: over plain http there is no certificate to verify", urlFlag)
	}
	return nil
}

// loadServerCA is the system roots plus the bundle in file, or nil (the
// system roots alone) when no file is named.
func loadServerCA(file string) (*x509.CertPool, error) {
	if file == "" {
		return nil, nil
	}
	roots, err := agent.LoadServerCAs(file)
	if err != nil {
		return nil, fmt.Errorf("--server-ca-file: %w", err)
	}
	return roots, nil
}
