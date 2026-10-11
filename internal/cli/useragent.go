package cli

import (
	"fmt"
	"runtime"
	"time"

	"k8s.io/client-go/rest"
)

// kubernetesUserAgent is the User-Agent of every request scan and the agent
// send to the API server: "upgradescope/<release> (<os>/<arch>)". Without it
// client-go sends its own default, "upgradescope/v0.0.0 ... kubernetes/$Format"
// (its unstamped version and an unexpanded git-archive placeholder), and an
// operator reading the audit log cannot tell one release from another. The
// "upgradescope/" prefix is what the e2e audit-log steps classify scan by.
func kubernetesUserAgent() string {
	return fmt.Sprintf("upgradescope/%s (%s/%s)", version, runtime.GOOS, runtime.GOARCH)
}

// applyClientDefaults sets, once, what scan and the agent want of every
// rest.Config they build: the per-request timeout (client-go gives up on a
// single request after it; 0 is none) and the release-stamped user agent.
// The clients built from the config (collect.NewClients, the agent's dynamic
// and apiextensions clients) copy it, so they carry both.
func applyClientDefaults(cfg *rest.Config, requestTimeout time.Duration) {
	cfg.Timeout = requestTimeout
	cfg.UserAgent = kubernetesUserAgent()
}
