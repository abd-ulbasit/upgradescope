package mcp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// fleetTimeout bounds each request to the server.
const fleetTimeout = 30 * time.Second

// maxFleetResponseBytes bounds what is read of one response: a report or
// the fleet summary goes on to a client whole, so the bound is the one on
// a report a tool reads (MaxReportBytes says why).
const maxFleetResponseBytes = MaxReportBytes

// Fleet reads an upgradescope server's REST API with its read token, so the
// server's own read authentication and its limits apply as they do to a
// curl; the tools hold no credential of their own and show no data the token
// would not.
type Fleet struct {
	BaseURL string // e.g. https://upgradescope.example.com
	Token   string // the server's read token; "" for an open read API
	Client  *http.Client
}

// NewFleet returns a client for the server at baseURL.
func NewFleet(baseURL, token string) (*Fleet, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("--server-url %q: want an http(s) URL such as https://upgradescope.example.com", baseURL)
	}
	return &Fleet{BaseURL: strings.TrimSuffix(baseURL, "/"), Token: token, Client: newFleetClient()}, nil
}

// newFleetClient follows no redirect: the server's API answers where it is
// asked, and a 3xx (to a login page, or another host) comes back as the
// error it is rather than as an answer from somewhere else.
func newFleetClient() *http.Client {
	return &http.Client{
		Timeout:       fleetTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// TrustRoots makes the client verify the server against roots (the system
// roots plus a private CA: agent.LoadServerCAs) instead of the system roots
// alone. Verification is never skipped, and the client still follows no
// redirect.
func (f *Fleet) TrustRoots(roots *x509.CertPool) {
	f.Client = newFleetClient()
	if roots == nil {
		return
	}
	tr := http.DefaultTransport.(*http.Transport).Clone() // keeps the proxy, dial and idle settings
	tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	f.Client.Transport = tr
}

// CleartextHost is the host:port of serverURL when a bearer token sent to
// it would cross the network in the clear: the URL is plain http:// to a
// host that is not loopback, and token is not empty. ok is false otherwise.
// 'mcp --server-url' and 'clusters' warn on it.
func CleartextHost(serverURL, token string) (host string, ok bool) {
	u, err := url.Parse(serverURL)
	if err != nil || token == "" || !strings.EqualFold(u.Scheme, "http") {
		return "", false
	}
	h := u.Hostname()
	if ip := net.ParseIP(h); strings.EqualFold(h, "localhost") || ip != nil && ip.IsLoopback() {
		return "", false
	}
	return u.Host, true
}

// CleartextWarning says why reading serverURL with token is unsafe, or
// returns "": a read token sent over plain http:// to a host that is not
// loopback crosses the network in the clear, where anyone on the path can
// replay it (as agent.CleartextPushWarning says of the ingest token).
func CleartextWarning(serverURL, token string) string {
	host, ok := CleartextHost(serverURL, token)
	if !ok {
		return ""
	}
	return fmt.Sprintf("reading %s over plain http: the read token crosses the network unencrypted, "+
		"so anyone on the path can replay it; serve the server over https (--tls-cert-file, the chart's server.tls, or a TLS Ingress)", host)
}

// get returns the 2xx JSON body of GET path. Any other status is an error
// carrying the server's own message, cut to MaxClusterTextBytes, so an
// assistant sees "401 Unauthorized ..." and can tell the user to supply the
// read token.
func (f *Fleet) get(ctx context.Context, path string, query url.Values) (json.RawMessage, error) {
	u := f.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if f.Token != "" {
		req.Header.Set("Authorization", "Bearer "+f.Token)
	}
	client := f.Client
	if client == nil {
		client = newFleetClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &outsideError{fmt.Sprintf("upgradescope server: GET %s: ", path), err}
	}
	defer resp.Body.Close()
	if resp.ContentLength > maxFleetResponseBytes {
		return nil, fmt.Errorf("upgradescope server: GET %s: response is larger than %s, the most a tool reads", path, mib(maxFleetResponseBytes))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFleetResponseBytes+1))
	if err != nil {
		return nil, &outsideError{fmt.Sprintf("upgradescope server: GET %s: reading the response: ", path), err}
	}
	if len(raw) > maxFleetResponseBytes {
		return nil, fmt.Errorf("upgradescope server: GET %s: response is larger than %s, the most a tool reads", path, mib(maxFleetResponseBytes))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The status code and Go's text for it, not resp.Status, whose
		// reason phrase is the server's to choose; and the tool's hint
		// before the server's own message, which is cut.
		msg := fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		if resp.StatusCode == http.StatusUnauthorized && f.Token == "" {
			msg += " (the server requires a read token: start 'upgradescope mcp' with --read-token, --read-token-file or $UPGRADESCOPE_READ_TOKEN)"
		}
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg += ": the server says: " + quoteOutside(e.Error)
		}
		return nil, fmt.Errorf("upgradescope server: GET %s: %s", path, msg)
	}
	return json.RawMessage(raw), nil
}

// clusterID resolves a cluster's name, or its numeric id, to the id.
func (f *Fleet) clusterID(ctx context.Context, cluster string) (int64, string, error) {
	raw, err := f.get(ctx, "/api/v1/clusters", nil)
	if err != nil {
		return 0, "", err
	}
	var rows []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return 0, "", &outsideError{"upgradescope server: decoding the cluster list: ", err}
	}
	for _, r := range rows {
		if r.Name == cluster {
			return r.ID, r.Name, nil
		}
	}
	if id, err := strconv.ParseInt(cluster, 10, 64); err == nil {
		for _, r := range rows {
			if r.ID == id {
				return r.ID, r.Name, nil
			}
		}
	}
	return 0, "", fmt.Errorf("the server has no cluster %q (it knows %d; fleet_summary lists them)", quoteOutside(cluster), len(rows))
}

// Report is the server's report of a cluster at target ("" = the cluster's
// next minor) and the cluster's name.
func (f *Fleet) Report(ctx context.Context, cluster, target string) (json.RawMessage, string, error) {
	id, name, err := f.clusterID(ctx, cluster)
	if err != nil {
		return nil, "", err
	}
	q := url.Values{}
	if target != "" {
		q.Set("target", target)
	}
	doc, err := f.get(ctx, fmt.Sprintf("/api/v1/clusters/%d/report", id), q)
	if err != nil {
		return nil, "", err
	}
	var probe struct {
		SchemaVersion *int `json:"schemaVersion"`
	}
	if json.Unmarshal(doc, &probe) != nil || probe.SchemaVersion == nil {
		return nil, "", errors.New("upgradescope server: the report response is not a report (is --server-url an upgradescope server?)")
	}
	return doc, name, nil
}

// Summary is the server's fleet score matrix.
func (f *Fleet) Summary(ctx context.Context, targets []string) (json.RawMessage, error) {
	q := url.Values{}
	if len(targets) > 0 {
		q.Set("targets", strings.Join(targets, ","))
	}
	return f.get(ctx, "/api/v1/fleet", q)
}
