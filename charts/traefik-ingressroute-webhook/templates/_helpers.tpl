{{- define "tirw.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "tirw.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "tirw.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "tirw.selectorLabels" -}}
app.kubernetes.io/name: {{ include "tirw.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "tirw.labels" -}}
helm.sh/chart: {{ include "tirw.chart" . }}
{{ include "tirw.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "tirw.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "tirw.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "tirw.tlsSecretName" -}}
{{- if .Values.certManager.enabled }}
{{- printf "%s-tls" (include "tirw.fullname" .) }}
{{- else }}
{{- required "existingSecret.name is required when certManager.enabled is false" .Values.existingSecret.name }}
{{- end }}
{{- end }}

{{- define "tirw.dnsNames" -}}
{{- $svc := include "tirw.fullname" . }}
{{- list (printf "%s.%s.svc" $svc .Release.Namespace) (printf "%s.%s.svc.cluster.local" $svc .Release.Namespace) | toJson }}
{{- end }}
