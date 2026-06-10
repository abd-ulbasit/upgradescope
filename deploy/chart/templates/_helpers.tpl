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

{{/* Agent ServiceAccount name */}}
{{- define "upgradescope.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "upgradescope.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
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

{{/* Secret holding the agent's push token */}}
{{- define "upgradescope.agentTokenSecretName" -}}
{{- if .Values.agent.existingSecret -}}
{{- .Values.agent.existingSecret -}}
{{- else -}}
{{- printf "%s-agent-token" (include "upgradescope.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* Secret holding the server's tokens */}}
{{- define "upgradescope.serverSecretName" -}}
{{- if .Values.server.existingSecret -}}
{{- .Values.server.existingSecret -}}
{{- else -}}
{{- printf "%s-tokens" (include "upgradescope.serverFullname" .) -}}
{{- end -}}
{{- end -}}
