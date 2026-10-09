{{/* Chart name */}}
{{- define "upgradescope.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified app name */}}
{{- define "upgradescope.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
A resource name made of the fullname plus a suffix, e.g. "-server" or
"-agent-metrics" (call with (dict "root" $ "suffix" "-server")). The
fullname gives way to the suffix, so the whole name is at most 63
characters: a Service name is a DNS-1035 label, which the API server
refuses past 63, though helm template and lint do not. A name that already
fits (every one of a default release name) is unchanged. A fullname cut
short may leave two releases' names equal: Helm releases are at most 53
characters and are unique per namespace, so only a release name sharing its
first characters with another in the namespace is affected.
*/}}
{{- define "upgradescope.derived" -}}
{{- printf "%s%s" (include "upgradescope.fullname" .root | trunc (int (sub 63 (len .suffix))) | trimSuffix "-") .suffix -}}
{{- end -}}

{{/* Common labels */}}
{{- define "upgradescope.labels" -}}
app.kubernetes.io/name: {{ include "upgradescope.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/*
The container image of both Deployments: repository:tag (tag defaults to the
chart's appVersion), plus @digest when image.digest is set and image.tag is
not. The release workflow sets image.digest when it packages the chart, so
the published chart runs exactly the image released with it. The digest is
that appVersion image's: applied to an overridden tag it would run the old
bytes under the new tag's name, so a set image.tag runs as written (pin it
with tag@digest in image.tag).
*/}}
{{- define "upgradescope.image" -}}
{{- $ref := printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- if and .Values.image.digest (not .Values.image.tag) -}}
{{- $ref = printf "%s@%s" $ref .Values.image.digest -}}
{{- end -}}
{{- $ref -}}
{{- end -}}

{{/* Agent ServiceAccount name */}}
{{- define "upgradescope.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "upgradescope.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Pod-level settings shared by both Deployments. Call with
(dict "root" $ "c" .Values.<component>).
*/}}
{{- define "upgradescope.podOptions" -}}
{{- with .c.podSecurityContext }}
securityContext: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .root.Values.imagePullSecrets }}
imagePullSecrets: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .c.priorityClassName }}
priorityClassName: {{ . | quote }}
{{- end }}
{{- with .c.nodeSelector }}
nodeSelector: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .c.tolerations }}
tolerations: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .c.affinity }}
affinity: {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{/* Server ServiceAccount name ("" = namespace default) */}}
{{- define "upgradescope.serverServiceAccountName" -}}
{{- if .Values.server.serviceAccount.create -}}
{{- default (include "upgradescope.serverFullname" .) .Values.server.serviceAccount.name -}}
{{- else -}}
{{- .Values.server.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Server resource name */}}
{{- define "upgradescope.serverFullname" -}}
{{- include "upgradescope.derived" (dict "root" . "suffix" "-server") -}}
{{- end -}}

{{/* The server Service's fully qualified DNS name:
<fullname>-server.<namespace>.svc.<clusterDomain>, the domain lowercased
and without a trailing dot. */}}
{{- define "upgradescope.serverFQDN" -}}
{{- printf "%s.%s.svc.%s" (include "upgradescope.serverFullname" .) .Release.Namespace (.Values.clusterDomain | lower | trimSuffix ".") -}}
{{- end -}}

{{/* Is the agent pushing to a server at all? Non-empty string = yes. */}}
{{- define "upgradescope.pushEnabled" -}}
{{- if or .Values.agent.serverUrl .Values.server.enabled -}}true{{- end -}}
{{- end -}}

{{/* Effective server URL for the agent: the in-chart server's Service,
over HTTPS when it serves TLS. */}}
{{- define "upgradescope.serverUrl" -}}
{{- if .Values.agent.serverUrl -}}
{{- .Values.agent.serverUrl -}}
{{- else -}}
{{- printf "%s://%s.%s.svc:%d" (ternary "https" "http" (ne (include "upgradescope.serverTLSSecret" .) "")) (include "upgradescope.serverFullname" .) .Release.Namespace (int .Values.server.service.port) -}}
{{- end -}}
{{- end -}}

{{/* kubernetes.io/tls Secret the server serves HTTPS with, "" = plain
HTTP: server.tls.secretName, or the one the cert-manager Certificate
issues into, <fullname>-server-https: not <fullname>-server-tls, the
Ingress's default, which holds the public host's certificate. */}}
{{- define "upgradescope.serverTLSSecret" -}}
{{- if .Values.server.tls.secretName -}}
{{- .Values.server.tls.secretName -}}
{{- else if .Values.server.tls.certManager.issuerRef.name -}}
{{- include "upgradescope.derived" (dict "root" . "suffix" "-server-https") -}}
{{- end -}}
{{- end -}}

{{/* Does the in-chart agent push to the in-chart server over HTTPS, and
so trust its Secret's CA (server.tls.caKey)? Non-empty string = yes.
agent.serverCA, when set, is trusted instead. */}}
{{- define "upgradescope.agentTrustsServerCA" -}}
{{- if and .Values.server.enabled (not .Values.agent.serverUrl) (include "upgradescope.serverTLSSecret" .) .Values.server.tls.caKey (not (include "upgradescope.agentServerCA" .)) -}}true{{- end -}}
{{- end -}}

{{/* Does agent.serverCA name a CA bundle for --server-ca-file? Non-empty
string = yes; a half or contradictory setting fails the render. */}}
{{- define "upgradescope.agentServerCA" -}}
{{- $ca := .Values.agent.serverCA -}}
{{- if or $ca.configMap $ca.secret -}}
{{- if and $ca.configMap $ca.secret -}}
{{- fail "agent.serverCA: set configMap or secret, not both" -}}
{{- end -}}
{{- if not $ca.key -}}
{{- fail "agent.serverCA.key is empty: name the key that holds the PEM bundle" -}}
{{- end -}}
{{- if not (include "upgradescope.pushEnabled" .) -}}
{{- fail "agent.serverCA verifies the server snapshots are pushed to: set agent.serverUrl (or server.enabled)" -}}
{{- end -}}
{{- if not (hasPrefix "https://" (lower (include "upgradescope.serverUrl" .))) -}}
{{- fail "agent.serverCA needs an https server: set an https agent.serverUrl, or server.tls for the in-chart server" -}}
{{- end -}}
true
{{- end -}}
{{- end -}}

{{/* Does env (a container env list) set name? Non-empty string = yes.
Call with (dict "env" <list> "name" <string>). */}}
{{- define "upgradescope.setsEnv" -}}
{{- range .env -}}{{- if eq .name $.name -}}true{{- end -}}{{- end -}}
{{- end -}}

{{/*
GOMEMLIMIT for a container: 90% of its memory limit, in bytes, or "" when
it has no limit or its extraEnv sets GOMEMLIMIT itself. The Go runtime does
not read the cgroup limit: without this the collector lets garbage grow
to as much as the live heap again (GOGC=100), and a pod whose live heap
fits its limit is OOM-killed anyway. The 10% left is for memory the Go
heap does not count. The limit is a number of bytes (a values file's YAML
number arrives as a float64, --set's as an int64) or a quantity string
with an optional exponent and a k..E or Ki..Ei suffix. Anything else (say
"100m") is "" too: serve and agent then read the limit from their cgroup
themselves. Call with (dict "resources" .resources "extraEnv" .extraEnv).
*/}}
{{- define "upgradescope.goMemLimit" -}}
{{- $v := dig "limits" "memory" "" (.resources | default dict) -}}
{{- if and $v (not (include "upgradescope.setsEnv" (dict "env" .extraEnv "name" "GOMEMLIMIT"))) -}}
{{- $bytes := 0.0 -}}
{{- if or (kindIs "float64" $v) (kindIs "int64" $v) (kindIs "int" $v) -}}
{{- $bytes = float64 $v -}}
{{- else -}}
{{- $q := toString $v -}}
{{- $n := regexFind "^[0-9]+(\\.[0-9]+)?([eE][+-]?[0-9]+)?" $q -}}
{{- $units := dict "" 1 "k" 1000 "M" 1000000 "G" 1000000000 "T" 1000000000000 "P" 1000000000000000 "E" 1000000000000000000 "Ki" 1024 "Mi" 1048576 "Gi" 1073741824 "Ti" 1099511627776 "Pi" 1125899906842624 "Ei" 1152921504606846976 -}}
{{- $unit := trimPrefix $n $q -}}
{{- if and $n (hasKey $units $unit) -}}
{{- $bytes = mulf (float64 $n) (float64 (get $units $unit)) -}}
{{- end -}}
{{- end -}}
{{- if and (ge $bytes 1.0) (lt $bytes 9e18) -}}
{{- printf "%d" (int64 (floor (mulf $bytes 0.9))) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Chart-managed Secret for an inline agent.serverToken */}}
{{- define "upgradescope.agentTokenSecretName" -}}
{{- include "upgradescope.derived" (dict "root" . "suffix" "-agent-token") -}}
{{- end -}}

{{/*
secretKeyRef (name + key) for the agent's push token, first match wins:
agent.existingSecret (key serverToken), an inline agent.serverToken (chart
Secret, key serverToken), then the in-chart server's own shared ingest token (key
ingestToken of the server Secret, generated or existing).
*/}}
{{- define "upgradescope.agentTokenRef" -}}
{{- if .Values.agent.existingSecret -}}
name: {{ .Values.agent.existingSecret }}
key: serverToken
{{- else if .Values.agent.serverToken -}}
name: {{ include "upgradescope.agentTokenSecretName" . }}
key: serverToken
{{- else if and .Values.server.enabled .Values.server.sharedIngestToken -}}
name: {{ include "upgradescope.serverSecretName" . }}
key: ingestToken
{{- else if .Values.server.enabled -}}
{{- fail "server.sharedIngestToken=false: the in-chart agent needs a per-cluster token. Mint one with 'upgradescope tokens create <cluster>' and set agent.existingSecret (key serverToken) or agent.serverToken" -}}
{{- else -}}
{{- fail "a push token is required with agent.serverUrl: set agent.existingSecret (key serverToken) or agent.serverToken" -}}
{{- end -}}
{{- end -}}

{{/* Does the server get a read token? Non-empty string = yes. */}}
{{- define "upgradescope.readTokenEnabled" -}}
{{- if and .Values.server.readTokenFromSecret (not .Values.server.existingSecret) -}}
{{- fail "server.readTokenFromSecret reads key readToken from server.existingSecret; set server.existingSecret, or set server.readToken instead" -}}
{{- end -}}
{{- if or .Values.server.readToken .Values.server.readTokenFromSecret -}}true{{- end -}}
{{- end -}}

{{/* Does the server get an admin token? Non-empty string = yes. */}}
{{- define "upgradescope.adminTokenEnabled" -}}
{{- if and .Values.server.adminTokenFromSecret (not .Values.server.existingSecret) -}}
{{- fail "server.adminTokenFromSecret reads key adminToken from server.existingSecret; set server.existingSecret, or set server.adminToken instead" -}}
{{- end -}}
{{- if or (and .Values.server.adminToken (not .Values.server.existingSecret)) .Values.server.adminTokenFromSecret -}}true{{- end -}}
{{- end -}}

{{/* Does the server use Postgres (server.database.existingSecret)? Non-empty string = yes. */}}
{{- define "upgradescope.postgres" -}}
{{- if .Values.server.database.existingSecret -}}true{{- end -}}
{{- end -}}

{{/* Secret holding the server's tokens */}}
{{- define "upgradescope.serverSecretName" -}}
{{- if .Values.server.existingSecret -}}
{{- .Values.server.existingSecret -}}
{{- else -}}
{{- include "upgradescope.derived" (dict "root" . "suffix" "-server-tokens") -}}
{{- end -}}
{{- end -}}

{{/*
A Go duration ("90s", "1.5h", "1h30m") as seconds, printed as a number.
The schema only lets valid durations through, so a string with no parsable
part reads as 0.
*/}}
{{- define "upgradescope.durationSeconds" -}}
{{- $units := dict "ns" 0.000000001 "us" 0.000001 "µs" 0.000001 "μs" 0.000001 "ms" 0.001 "s" 1.0 "m" 60.0 "h" 3600.0 -}}
{{- $total := 0.0 -}}
{{- range $part := regexFindAll "(?:[0-9]+(?:\\.[0-9]*)?|\\.[0-9]+)(?:ns|us|µs|μs|ms|s|m|h)" (toString .) -1 -}}
{{- $n := regexFind "^(?:[0-9]+(?:\\.[0-9]*)?|\\.[0-9]+)" $part -}}
{{- $total = addf $total (mulf (float64 $n) (float64 (get $units (trimPrefix $n $part)))) -}}
{{- end -}}
{{- printf "%f" $total -}}
{{- end -}}

{{/*
How long the server waits for a cluster's push before it calls the cluster
stale, in whole seconds. server.staleAfter when set (the render fails when
it is not above agent.interval: every cluster would flap stale between its
ticks), else the larger of 2h and three agent intervals. An unchanged
cluster pushes at its next tick after the hourly force-sync, so the gap
between pushes is about max(interval, 1h) plus a tick: three intervals
leave two missed ticks of slack, and 2h is the default for any interval up
to 40m.
*/}}
{{- define "upgradescope.staleAfterSeconds" -}}
{{- $interval := float64 (include "upgradescope.durationSeconds" .Values.agent.interval) -}}
{{- if .Values.server.staleAfter -}}
{{- $stale := float64 (include "upgradescope.durationSeconds" .Values.server.staleAfter) -}}
{{- if and .Values.agent.enabled (le $stale $interval) -}}
{{- fail (printf "server.staleAfter (%s) must be above agent.interval (%s): the agent pushes at most once per interval, so every cluster would read stale between pushes. Leave server.staleAfter empty to follow the interval (the larger of 2h and 3 x interval)" .Values.server.staleAfter .Values.agent.interval) -}}
{{- end -}}
{{- printf "%d" (int64 (ceil $stale)) -}}
{{- else -}}
{{- printf "%d" (int64 (ceil (maxf 7200.0 (mulf 3.0 $interval)))) -}}
{{- end -}}
{{- end -}}

{{/* serve --stale-after: server.staleAfter as written, else "2h" when that is
the default threshold, else the default in seconds ("10800s"). */}}
{{- define "upgradescope.staleAfter" -}}
{{- if .Values.server.staleAfter -}}
{{- $_ := include "upgradescope.staleAfterSeconds" . -}}
{{- .Values.server.staleAfter -}}
{{- else if eq (include "upgradescope.staleAfterSeconds" .) "7200" -}}
2h
{{- else -}}
{{- printf "%ss" (include "upgradescope.staleAfterSeconds" .) -}}
{{- end -}}
{{- end -}}

{{/* The UpgradescopeClusterStale alert's threshold in seconds:
metrics.prometheusRule.clusterStaleAfterSeconds when set, else the server's
own threshold, so the alert and the stale flag agree. */}}
{{- define "upgradescope.clusterStaleAfterSeconds" -}}
{{- $set := int64 (.Values.metrics.prometheusRule.clusterStaleAfterSeconds | default 0) -}}
{{- if $set -}}
{{- $interval := float64 (include "upgradescope.durationSeconds" .Values.agent.interval) -}}
{{- if and .Values.agent.enabled (le (float64 $set) $interval) -}}
{{- fail (printf "metrics.prometheusRule.clusterStaleAfterSeconds (%d) must be above agent.interval (%s). Leave it 0 to follow server.staleAfter" $set .Values.agent.interval) -}}
{{- end -}}
{{- $set -}}
{{- else -}}
{{- include "upgradescope.staleAfterSeconds" . -}}
{{- end -}}
{{- end -}}

{{/*
A checksum for the pod template of a Deployment whose environment holds
tokens read from a chart-managed Secret. The Secrets are read at container
start, so without it `helm upgrade --set server.readToken=<new>` changes the
Secret and leaves the old token working in the running pod. Only the
values the user supplied are hashed (not a generated ingest token, which
helm template and GitOps renderers would regenerate on every render and so
roll the pods every sync), salted with the release and namespace so the
hash is not a rainbow-table lookup; a short or guessable token can still
be tested against it by anyone who can read the Deployment, so use long
random ones. A Secret named by an existingSecret cannot be hashed: its
contents are not in the chart.
*/}}
{{- define "upgradescope.serverSecretChecksum" -}}
{{- if not .Values.server.existingSecret -}}
{{- $v := pick .Values.server "ingestToken" "readToken" "adminToken" "slackWebhook" "webhook" "webhookSecret" "sharedIngestToken" -}}
{{- printf "%s/%s/%s" .Release.Namespace .Release.Name (toJson $v) | sha256sum -}}
{{- end -}}
{{- end -}}

{{/* As serverSecretChecksum, for the agent's push token: its own inline
agent.serverToken, or the in-chart server's ingest token it falls back to.
Empty with agent.existingSecret, or with a generated server token. */}}
{{- define "upgradescope.agentSecretChecksum" -}}
{{- if .Values.agent.existingSecret -}}
{{- else if .Values.agent.serverToken -}}
{{- printf "%s/%s/%s" .Release.Namespace .Release.Name .Values.agent.serverToken | sha256sum -}}
{{- else if and .Values.server.enabled .Values.server.sharedIngestToken (not .Values.server.existingSecret) .Values.server.ingestToken -}}
{{- printf "%s/%s/%s" .Release.Namespace .Release.Name .Values.server.ingestToken | sha256sum -}}
{{- end -}}
{{- end -}}
