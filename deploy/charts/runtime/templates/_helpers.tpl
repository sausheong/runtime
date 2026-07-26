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

{{/* Immutable digest wins over the development-compatible tag. */}}
{{- define "runtime.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
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
{{/*
The Bitnami postgresql subchart names its primary Service from the RELEASE name
(common.names.fullname → "<release>-postgresql"), NOT from runtime.fullname. Use
.Release.Name here so the DSN host matches the actual Service for every release
name (using runtime.fullname only coincides when the release contains "runtime").
*/}}
{{- printf "postgres://%s:%s@%s-postgresql:5432/%s?sslmode=disable" $a.username $a.password .Release.Name $a.database -}}
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

{{/* Stable env/Secret key for one agent's control-plane-to-agent bearer. */}}
{{- define "runtime.agentAuthKey" -}}
{{- printf "RUNTIME_AGENT_AUTH_TOKEN_%s" (.id | upper | replace "-" "_") -}}
{{- end -}}

{{/*
Identity-enabled and perAgentPods deployments must not give agents the
control-plane DSN. existingSecret is accepted because Helm cannot inspect its
keys; operators must provide RUNTIME_AGENT_PG_DSN there.
*/}}
{{- define "runtime.requireAgentPg" -}}
{{- if or .Values.identity.enabled (eq .Values.scheduling.mode "perAgentPods") -}}
{{- if and (not .Values.secrets.agentPgDsn) (not .Values.secrets.existingSecret) -}}
{{- fail "runtime: identity/perAgentPods requires secrets.agentPgDsn or an existingSecret with RUNTIME_AGENT_PG_DSN" -}}
{{- end -}}
{{- if gt (len .Values.config.agents) 1 -}}
{{- fail "runtime: one restricted agent database role cannot isolate multiple agents; deploy one chart release and role per agent" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* perAgentPods requires one bearer per configured agent. */}}
{{- define "runtime.requireAgentAuth" -}}
{{- if eq .Values.scheduling.mode "perAgentPods" -}}
{{- if not .Values.secrets.existingSecret -}}
{{- range $a := .Values.config.agents -}}
{{- if not (index $.Values.secrets.agentAuthTokens $a.id) -}}
{{- fail (printf "runtime: perAgentPods requires secrets.agentAuthTokens.%s (distinct bearer per agent) or an existingSecret with per-agent token keys" $a.id) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Subject forwarding requires a stable asymmetric key pair in Kubernetes. */}}
{{- define "runtime.requireIdentitySigning" -}}
{{- if .Values.identity.subjectForwarding -}}
{{- if and (not .Values.secrets.existingSecret) (not .Values.secrets.identitySigningPrivateKey) -}}
{{- fail "runtime: identity.subjectForwarding requires secrets.identitySigningPrivateKey, or an existingSecret carrying it. The matching public key is derived from it at startup; set secrets.identitySigningPublicKey only to pin the value." -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Fail-closed validation: at least one agent must be configured. runtimed's config
loader rejects an empty registry ("config: at least one agent is required") and
os.Exit(1)s, so an install with no agents would CrashLoop. Fail at render instead
with an actionable message.
*/}}
{{- define "runtime.requireAgents" -}}
{{- if not .Values.config.agents -}}
{{- fail "runtime: config.agents must list at least one agent (each needs id, name, model, plus listen_addr in monolith mode) — runtimed refuses to start with an empty registry" -}}
{{- end -}}
{{- end -}}

{{/*
Per-agent StatefulSet name: "<release>-agent-<id>". Takes a dict {root, id}.
*/}}
{{- define "runtime.agentFullname" -}}
{{- printf "%s-agent-%s" .root.Release.Name .id | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Per-agent headless Service name: "<release>-agent-<id>-hl". dict {root, id}. */}}
{{- define "runtime.agentHeadless" -}}
{{- printf "%s-agent-%s-hl" .root.Release.Name .id | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Stable DBOS schema for one tenant/agent trust domain. dict {tenant, id}. */}}
{{- define "runtime.agentDBOSSchema" -}}
{{- $tenant := .tenant | default "default" -}}
{{- printf "dbos_agent_%s" (printf "%s/%s" $tenant .id | sha256sum | trunc 24) -}}
{{- end -}}

{{/*
runtimed's dial url template for an agent's remote pool, with the literal {i}
ordinal placeholder config.RemoteReplicaURL expands. Pod DNS in a StatefulSet is
<pod>.<headless-svc>.<ns>.svc.cluster.local, and pod name is <sts>-<ordinal>, so:
  http://<release>-agent-<id>-{i}.<release>-agent-<id>-hl.<ns>.svc.cluster.local:8080
dict {root, id}.
*/}}
{{- define "runtime.agentDialTemplate" -}}
{{- $sts := include "runtime.agentFullname" . -}}
{{- $hl := include "runtime.agentHeadless" . -}}
{{- printf "http://%s-{i}.%s.%s.svc.cluster.local:8080" $sts $hl .root.Release.Namespace -}}
{{- end -}}

{{/*
Fail-closed validation for perAgentPods mode: each agent must NOT set
listen_addr or url (the chart generates the url) and must have id/name/model.
*/}}
{{- define "runtime.requirePerAgentPods" -}}
{{- $tenants := dict -}}
{{- range $i, $a := .Values.config.agents -}}
{{- if or (not $a.id) (not $a.name) (not $a.model) -}}
{{- fail (printf "runtime: perAgentPods agent[%d] needs id, name, model" $i) -}}
{{- end -}}
{{- if or $a.listen_addr $a.url -}}
{{- fail (printf "runtime: perAgentPods agent %q must NOT set listen_addr or url (the chart generates the per-ordinal url)" $a.id) -}}
{{- end -}}
{{- if not $a.registration_generation -}}
{{- fail (printf "runtime: perAgentPods agent %q requires registration_generation; persist it across restarts and rotate it on replacement" $a.id) -}}
{{- end -}}
{{- $_ := set $tenants ($a.tenant | default "default") true -}}
{{- end -}}
{{- if gt (len $tenants) 1 -}}
{{- fail "runtime: perAgentPods uses one restricted database role and therefore requires all agents to belong to one tenant; use separate releases/databases for other tenants" -}}
{{- end -}}
{{- end -}}
