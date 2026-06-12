{{/* Expand the name of the chart. */}}
{{- define "runtime.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified app name. */}}
{{- define "runtime.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "runtime.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "runtime.labels" -}}
helm.sh/chart: {{ include "runtime.chart" . }}
{{ include "runtime.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "runtime.selectorLabels" -}}
app.kubernetes.io/name: {{ include "runtime.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "runtime.serviceAccountName" -}}
{{- include "runtime.fullname" . -}}
{{- end -}}

{{/* The name of the Secret env refs target: existing, or our own. */}}
{{- define "runtime.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else -}}
{{- include "runtime.fullname" . -}}
{{- end -}}
{{- end -}}

{{/*
Resolve the Postgres DSN.
- postgresql.enabled  → synthesize the in-cluster DSN.
- else secrets.pgDsn or secrets.existingSecret → caller provides it.
- else → fail (fail-closed).
Returns the literal DSN string ONLY for the synthesized case; for BYO it returns
empty (the Secret/existingSecret supplies it). Use runtime.requirePg to validate.
*/}}
{{- define "runtime.pgDsn" -}}
{{- if .Values.postgresql.enabled -}}
{{- $a := .Values.postgresql.auth -}}
{{- printf "postgres://%s:%s@%s-postgresql:5432/%s?sslmode=disable" $a.username $a.password (include "runtime.fullname" .) $a.database -}}
{{- else -}}
{{- .Values.secrets.pgDsn -}}
{{- end -}}
{{- end -}}

{{/* Fail-closed validation: a DSN source must exist. */}}
{{- define "runtime.requirePg" -}}
{{- if not .Values.postgresql.enabled -}}
{{- if and (not .Values.secrets.pgDsn) (not .Values.secrets.existingSecret) -}}
{{- fail "runtime: set postgresql.enabled=true, or secrets.pgDsn, or secrets.existingSecret (with a RUNTIME_PG_DSN key)" -}}
{{- end -}}
{{- end -}}
{{- end -}}
