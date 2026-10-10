package chart

import (
	"slices"
	"strings"
	"testing"
)

// The chart passed --allow-anonymous-read whenever server.readToken was
// empty, so a deployment that relied on read tokens minted in the database
// was open again whenever that database was lost or restored (#295). With
// no read token it now passes --require-read-credential, and an open read
// API is an explicit opt-in.
func TestServerReadAPIFlags(t *testing.T) {
	const (
		open   = "--allow-anonymous-read"
		closed = "--require-read-credential"
	)
	for _, tc := range []struct {
		name string
		sets []string
		want []string // flags that must be passed
		not  []string // flags that must not
	}{
		{"nothing set: closed", nil, []string{closed}, []string{open}},
		{"readToken", []string{"server.readToken=r"}, nil, []string{open, closed}},
		{"existingSecret alone: closed", []string{"server.existingSecret=mysec"}, []string{closed}, []string{open}},
		{"existingSecret with readTokenFromSecret", []string{"server.existingSecret=mysec", "server.readTokenFromSecret=true"}, nil, []string{open, closed}},
		{"server.allowAnonymousRead opts into open", []string{"server.allowAnonymousRead=true"}, []string{open}, []string{closed}},
		{"server.ingress.allowAnonymousRead opts into open", []string{"server.ingress.allowAnonymousRead=true"}, []string{open}, []string{closed}},
		{"allowAnonymousRead with a readToken: the token wins", []string{"server.readToken=r", "server.allowAnonymousRead=true"}, nil, []string{open, closed}},
		{"persistence off, still closed", []string{"server.persistence.enabled=false"}, []string{closed}, []string{open}},
		{"external database, still closed", []string{"server.database.existingSecret=pg"}, []string{closed}, []string{open}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := render(t, append([]string{"server.enabled=true", "server.ingestToken=t"}, tc.sets...)...)
			got := args(container(t, objs, "upgradescope-server"))
			for _, f := range tc.want {
				if !slices.Contains(got, f) {
					t.Errorf("args %v lack %s", got, f)
				}
			}
			for _, f := range tc.not {
				if slices.Contains(got, f) {
					t.Errorf("args %v carry %s", got, f)
				}
			}
		})
	}
}

// serve refuses --allow-anonymous-read together with the chart's
// --require-read-credential; an argument in server.extraArgs that would
// crash the pod is a render error naming the value to use instead.
func TestServerExtraArgAllowAnonymousReadIsRefused(t *testing.T) {
	msg := renderErr(t, "server.enabled=true", "server.ingestToken=t", "server.extraArgs[0]=--allow-anonymous-read")
	if !strings.Contains(msg, "server.allowAnonymousRead") {
		t.Errorf("--allow-anonymous-read in extraArgs with no read token: render error = %q, want it to point at server.allowAnonymousRead", msg)
	}
	for _, sets := range [][]string{
		{"server.readToken=r", "server.extraArgs[0]=--allow-anonymous-read"},
		{"server.allowAnonymousRead=true", "server.extraArgs[0]=--allow-anonymous-read"},
	} {
		if msg := renderErr(t, append([]string{"server.enabled=true", "server.ingestToken=t"}, sets...)...); msg != "" {
			t.Errorf("%v: %s", sets, msg)
		}
	}
}

// The Ingress: closed (no read token, no opt-in) publishes a read API that
// needs a credential, so it renders; an open one needs the Ingress's own
// opt-in, which says an authenticating layer fronts it.
func TestIngressAndTheReadAPI(t *testing.T) {
	ing := []string{"server.enabled=true", "server.ingestToken=t", "server.ingress.enabled=true", "server.ingress.host=u.example.com"}
	if msg := renderErr(t, ing...); msg != "" {
		t.Errorf("Ingress with the read API closed (minted tokens): %s", msg)
	}
	if msg := renderErr(t, append(slices.Clone(ing), "server.readToken=r")...); msg != "" {
		t.Errorf("Ingress with a read token: %s", msg)
	}
	msg := renderErr(t, append(slices.Clone(ing), "server.allowAnonymousRead=true")...)
	if !strings.Contains(msg, "server.ingress.allowAnonymousRead") {
		t.Errorf("Ingress with server.allowAnonymousRead only: render error = %q, want it to ask for server.ingress.allowAnonymousRead", msg)
	}
	if msg := renderErr(t, append(slices.Clone(ing), "server.allowAnonymousRead=true", "server.ingress.allowAnonymousRead=true")...); msg != "" {
		t.Errorf("Ingress with both opt-ins: %s", msg)
	}
	if msg := renderErr(t, append(slices.Clone(ing), "server.ingress.allowAnonymousRead=true")...); msg != "" {
		t.Errorf("Ingress with server.ingress.allowAnonymousRead (an auth layer in front): %s", msg)
	}
}

// The ServiceMonitor sends the chart's read token or nothing, so with the
// read API closed by the chart it would be scraping a 401: the render says
// so instead of leaving a target that is down.
func TestServiceMonitorNeedsAReadCredentialItCanSend(t *testing.T) {
	base := []string{"server.enabled=true", "server.ingestToken=t", "metrics.serviceMonitor.enabled=true"}
	msg := renderErr(t, base...)
	if !strings.Contains(msg, "server.readToken") || !strings.Contains(msg, "server.allowAnonymousRead") {
		t.Errorf("ServiceMonitor with a closed read API: render error = %q, want it to name server.readToken and server.allowAnonymousRead", msg)
	}
	for _, extra := range []string{"server.readToken=r", "server.allowAnonymousRead=true"} {
		if msg := renderErr(t, append(slices.Clone(base), extra)...); msg != "" {
			t.Errorf("ServiceMonitor with %s: %s", extra, msg)
		}
	}
	if msg := renderErr(t, "metrics.serviceMonitor.enabled=true"); msg != "" {
		t.Errorf("agent-only ServiceMonitor: %s", msg)
	}
}

// The install notes explain the closed read API and mint the first token
// with a command that works against the chart's database: the SQLite file
// the server holds open, or the Postgres URL the container already has.
// An open read API is warned about, with what a lost database does to it.
func TestReadAPINotes(t *testing.T) {
	sqlite := renderNotes(t, "upgradescope", "server.enabled=true", "server.ingestToken=t")
	for _, want := range []string{
		"closed", "401", "--require-read-credential",
		"/upgradescope tokens create --read --teams '*' --db /data/upgradescope.sqlite",
		"deploy/upgradescope-server",
	} {
		if !strings.Contains(sqlite, want) {
			t.Errorf("NOTES (closed, SQLite) lack %q:\n%s", want, sqlite)
		}
	}
	pg := renderNotes(t, "upgradescope", "server.enabled=true", "server.ingestToken=t", "server.database.existingSecret=pg")
	if !strings.Contains(pg, "/upgradescope tokens create --read --teams '*'\n") || strings.Contains(pg, "tokens create --read --teams '*' --db") {
		t.Errorf("NOTES (closed, Postgres) mint with the wrong command:\n%s", pg)
	}
	for _, bad := range []string{"Fine behind a", "ClusterIP Service; set one"} {
		for _, n := range []string{sqlite, pg} {
			if strings.Contains(n, bad) {
				t.Errorf("NOTES still say %q", bad)
			}
		}
	}
	openNotes := renderNotes(t, "upgradescope", "server.enabled=true", "server.ingestToken=t", "server.allowAnonymousRead=true", "server.persistence.enabled=false")
	for _, want := range []string{"OPEN", "lost or restored database", "every pod restart", "421"} {
		if !strings.Contains(openNotes, want) {
			t.Errorf("NOTES (open) lack %q:\n%s", want, openNotes)
		}
	}
	withToken := renderNotes(t, "upgradescope", "server.enabled=true", "server.ingestToken=t", "server.readToken=r")
	if strings.Contains(withToken, "OPEN") || strings.Contains(withToken, "tokens create --read") {
		t.Errorf("NOTES with a read token talk about an open or empty read API:\n%s", withToken)
	}
}
