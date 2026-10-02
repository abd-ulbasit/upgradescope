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
chart's appVersion), plus @digest when image.digest is set. The release
workflow sets image.digest when it packages the chart, so the published
chart runs exactly the image released with it.
*/}}
{{- define "upgradescope.image" -}}
{{- $ref := printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- with .Values.image.digest -}}
{{- $ref = printf "%s@%s" $ref . -}}
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
{{- printf "%s-server" (include "upgradescope.fullname" .) -}}
{{- end -}}

{{/* Is the agent pushing to a server at all? Non-empty string = yes. */}}
{{- define "upgradescope.pushEnabled" -}}
{{- if or .Values.agent.serverUrl .Values.server.enabled -}}true{{- end -}}
{{- end -}}

{{/* Effective server URL for the agent */}}
{{- define "upgradescope.serverUrl" -}}
{{- if .Values.agent.serverUrl -}}
{{- .Values.agent.serverUrl -}}
{{- else -}}
{{- printf "http://%s.%s.svc:%d" (include "upgradescope.serverFullname" .) .Release.Namespace (int .Values.server.service.port) -}}
{{- end -}}
{{- end -}}

{{/* Chart-managed Secret for an inline agent.serverToken */}}
{{- define "upgradescope.agentTokenSecretName" -}}
{{- printf "%s-agent-token" (include "upgradescope.fullname" .) -}}
{{- end -}}

{{/*
secretKeyRef (name + key) for the agent's push token, first match wins:
agent.existingSecret (key serverToken), an inline agent.serverToken (chart
Secret, key serverToken), then the in-chart server's own ingest token (key
ingestToken of the server Secret, generated or existing).
*/}}
{{- define "upgradescope.agentTokenRef" -}}
{{- if .Values.agent.existingSecret -}}
name: {{ .Values.agent.existingSecret }}
key: serverToken
{{- else if .Values.agent.serverToken -}}
name: {{ include "upgradescope.agentTokenSecretName" . }}
key: serverToken
{{- else if .Values.server.enabled -}}
name: {{ include "upgradescope.serverSecretName" . }}
key: ingestToken
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

{{/* Secret holding the server's tokens */}}
{{- define "upgradescope.serverSecretName" -}}
{{- if .Values.server.existingSecret -}}
{{- .Values.server.existingSecret -}}
{{- else -}}
{{- printf "%s-tokens" (include "upgradescope.serverFullname" .) -}}
{{- end -}}
{{- end -}}
