{{/*
Chart name.
*/}}
{{- define "pelican-panel.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name.
*/}}
{{- define "pelican-panel.fullname" -}}
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

{{- define "pelican-panel.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "pelican-panel.labels" -}}
helm.sh/chart: {{ include "pelican-panel.chart" . }}
{{ include "pelican-panel.selectorLabels" . }}
app.kubernetes.io/version: {{ include "pelican-panel.imageTag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: pelican
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "pelican-panel.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pelican-panel.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "pelican-panel.panelSelectorLabels" -}}
{{ include "pelican-panel.selectorLabels" . }}
app.kubernetes.io/component: panel
{{- end }}

{{/*
Annotations applied to every object.
*/}}
{{- define "pelican-panel.annotations" -}}
{{- with .Values.commonAnnotations }}
{{- toYaml . }}
{{- end }}
{{- end }}

{{- define "pelican-panel.imageTag" -}}
{{- default .Chart.AppVersion .Values.image.tag }}
{{- end }}

{{- define "pelican-panel.image" -}}
{{- printf "%s:%s" .Values.image.repository (include "pelican-panel.imageTag" .) }}
{{- end }}

{{- define "pelican-panel.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "pelican-panel.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the chart-managed Secret (APP_KEY, generated passwords, ...).
*/}}
{{- define "pelican-panel.secretName" -}}
{{- printf "%s-env" (include "pelican-panel.fullname" .) }}
{{- end }}

{{- define "pelican-panel.configMapName" -}}
{{- printf "%s-env" (include "pelican-panel.fullname" .) }}
{{- end }}

{{- define "pelican-panel.pvcName" -}}
{{- if .Values.persistence.existingClaim }}
{{- .Values.persistence.existingClaim }}
{{- else }}
{{- printf "%s-data" (include "pelican-panel.fullname" .) }}
{{- end }}
{{- end }}

{{- define "pelican-panel.mariadb.fullname" -}}
{{- printf "%s-mariadb" (include "pelican-panel.fullname" .) }}
{{- end }}

{{- define "pelican-panel.redis.fullname" -}}
{{- printf "%s-redis" (include "pelican-panel.fullname" .) }}
{{- end }}

{{/*
Whether the chart has to manage its own Secret at all.
*/}}
{{- define "pelican-panel.createSecret" -}}
{{- $create := false -}}
{{- if not .Values.panel.existingSecret -}}
  {{- $create = true -}}
{{- end -}}
{{- if and .Values.mariadb.enabled (not .Values.mariadb.auth.existingSecret) -}}
  {{- $create = true -}}
{{- end -}}
{{- if and .Values.redis.enabled .Values.redis.password (not .Values.redis.existingSecret) -}}
  {{- $create = true -}}
{{- end -}}
{{- if and (not .Values.mariadb.enabled) (ne .Values.database.connection "sqlite") .Values.database.password (not .Values.database.existingSecret) -}}
  {{- $create = true -}}
{{- end -}}
{{- if and (not .Values.redis.enabled) .Values.externalRedis.password (not .Values.externalRedis.existingSecret) -}}
  {{- $create = true -}}
{{- end -}}
{{- if and (not .Values.panel.existingSecret) .Values.panel.mail.password -}}
  {{- $create = true -}}
{{- end -}}
{{- $create -}}
{{- end }}

{{/*
APP_KEY resolution for the chart-managed Secret.

Precedence:
  1. .Values.panel.appKey
  2. the APP_KEY already stored in the chart-managed Secret (via `lookup`)
  3. a freshly generated key

`lookup` returns nothing under `helm template` (and therefore under Argo CD),
so a generated key is NOT stable for GitOps - use `panel.existingSecret` there.
*/}}
{{- define "pelican-panel.appKey" -}}
{{- if .Values.panel.appKey -}}
{{- .Values.panel.appKey -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "pelican-panel.secretName" .) -}}
{{- if and $existing $existing.data (index $existing.data "APP_KEY") -}}
{{- index $existing.data "APP_KEY" | b64dec -}}
{{- else -}}
{{- printf "base64:%s" (randBytes 32) -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Re-usable "generate once, then reuse" helper for the bundled MariaDB passwords.
Args: (dict "ctx" $ "key" "MARIADB_PASSWORD" "value" "<explicit>")
*/}}
{{- define "pelican-panel.stickySecretValue" -}}
{{- $ctx := .ctx -}}
{{- if .value -}}
{{- .value -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" $ctx.Release.Namespace (include "pelican-panel.secretName" $ctx) -}}
{{- if and $existing $existing.data (index $existing.data .key) -}}
{{- index $existing.data .key | b64dec -}}
{{- else -}}
{{- randAlphaNum 24 -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Effective database settings (the bundled MariaDB overrides the external one).
*/}}
{{- define "pelican-panel.db.connection" -}}
{{- if .Values.mariadb.enabled -}}mariadb{{- else -}}{{ .Values.database.connection }}{{- end -}}
{{- end }}

{{- define "pelican-panel.db.host" -}}
{{- if .Values.mariadb.enabled -}}
{{- include "pelican-panel.mariadb.fullname" . -}}
{{- else -}}
{{- .Values.database.host -}}
{{- end -}}
{{- end }}

{{- define "pelican-panel.db.port" -}}
{{- if .Values.mariadb.enabled -}}
3306
{{- else if .Values.database.port -}}
{{- .Values.database.port -}}
{{- else if eq .Values.database.connection "pgsql" -}}
5432
{{- else -}}
3306
{{- end -}}
{{- end }}

{{- define "pelican-panel.db.name" -}}
{{- if .Values.mariadb.enabled -}}{{ .Values.mariadb.auth.database }}{{- else -}}{{ .Values.database.name }}{{- end -}}
{{- end }}

{{- define "pelican-panel.db.username" -}}
{{- if .Values.mariadb.enabled -}}{{ .Values.mariadb.auth.username }}{{- else -}}{{ .Values.database.username }}{{- end -}}
{{- end }}

{{/*
Redis host / secret resolution.
*/}}
{{- define "pelican-panel.redis.host" -}}
{{- if .Values.redis.enabled -}}
{{- include "pelican-panel.redis.fullname" . -}}
{{- else -}}
{{- .Values.externalRedis.host -}}
{{- end -}}
{{- end }}

{{- define "pelican-panel.redis.port" -}}
{{- if .Values.redis.enabled -}}6379{{- else -}}{{ .Values.externalRedis.port }}{{- end -}}
{{- end }}

{{- define "pelican-panel.usesRedis" -}}
{{- if or (eq .Values.panel.cacheStore "redis") (eq .Values.panel.sessionDriver "redis") (eq .Values.panel.queueConnection "redis") -}}true{{- end -}}
{{- end }}

{{/*
Validation of mutually exclusive / unsupported combinations.
*/}}
{{- define "pelican-panel.validate" -}}
{{- if eq .Values.panel.cacheStore "database" -}}
{{- fail "panel.cacheStore=database is not supported by Pelican Panel (no `cache` table migration). Use file or redis." -}}
{{- end -}}
{{- if gt (int .Values.replicaCount) 1 -}}
  {{- if not .Values.panel.skipMigrations -}}
  {{- fail "replicaCount > 1 requires panel.skipMigrations=true (every pod would run migrations)." -}}
  {{- end -}}
  {{- if or (eq .Values.panel.cacheStore "file") (eq .Values.panel.sessionDriver "file") -}}
  {{- fail "replicaCount > 1 requires a shared cache/session store (redis, or sessionDriver=database)." -}}
  {{- end -}}
{{- end -}}
{{- if and (ne (include "pelican-panel.db.connection" .) "sqlite") (not (include "pelican-panel.db.host" .)) (not .Values.database.existingSecretHostKey) -}}
{{- fail "database.host is required unless database.connection=sqlite, mariadb.enabled=true, or database.existingSecretHostKey is set." -}}
{{- end -}}
{{- if and (hasPrefix "https://" .Values.panel.url) (not .Values.panel.behindProxy) (not .Values.panel.leEmail) (not .Values.panel.skipCaddy) -}}
{{- fail "panel.url is https and panel.behindProxy is false: the bundled Caddy needs panel.leEmail for Let's Encrypt." -}}
{{- end -}}
{{- if and (include "pelican-panel.usesRedis" .) (not (include "pelican-panel.redis.host" .)) -}}
{{- fail "A redis driver is selected but neither redis.enabled nor externalRedis.host is set." -}}
{{- end -}}
{{- if and (not .Values.panel.skipCaddy) (has .Values.panel.trustedProxies (list "*" "**")) -}}
{{- fail "panel.trustedProxies='*'/'**' is rejected by the bundled Caddy (the entrypoint passes it to `trusted_proxies static`, which needs IPs/CIDRs). Use \"0.0.0.0/0,::/0\" instead, or set panel.skipCaddy=true." -}}
{{- end -}}
{{- end }}

{{/*
Secret-backed environment variables for the Panel container.
*/}}
{{- define "pelican-panel.panelSecretEnv" -}}
{{- $chartSecret := include "pelican-panel.secretName" . -}}
- name: APP_KEY
  valueFrom:
    secretKeyRef:
      {{- if .Values.panel.existingSecret }}
      name: {{ .Values.panel.existingSecret }}
      key: {{ .Values.panel.existingSecretAppKeyKey }}
      {{- else }}
      name: {{ $chartSecret }}
      key: APP_KEY
      {{- end }}
{{- if and .Values.panel.existingSecret .Values.panel.existingSecretMailPasswordKey }}
- name: MAIL_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ .Values.panel.existingSecret }}
      key: {{ .Values.panel.existingSecretMailPasswordKey }}
{{- else if and (not .Values.panel.existingSecret) .Values.panel.mail.password }}
- name: MAIL_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $chartSecret }}
      key: MAIL_PASSWORD
{{- end }}
{{- if ne (include "pelican-panel.db.connection" .) "sqlite" }}
{{- if .Values.mariadb.enabled }}
- name: DB_PASSWORD
  valueFrom:
    secretKeyRef:
      {{- if .Values.mariadb.auth.existingSecret }}
      name: {{ .Values.mariadb.auth.existingSecret }}
      key: {{ .Values.mariadb.auth.existingSecretPasswordKey }}
      {{- else }}
      name: {{ $chartSecret }}
      key: MARIADB_PASSWORD
      {{- end }}
{{- else if .Values.database.existingSecret }}
- name: DB_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecret }}
      key: {{ .Values.database.existingSecretPasswordKey }}
{{- with .Values.database.existingSecretUsernameKey }}
- name: DB_USERNAME
  valueFrom:
    secretKeyRef:
      name: {{ $.Values.database.existingSecret }}
      key: {{ . }}
{{- end }}
{{- with .Values.database.existingSecretDatabaseKey }}
- name: DB_DATABASE
  valueFrom:
    secretKeyRef:
      name: {{ $.Values.database.existingSecret }}
      key: {{ . }}
{{- end }}
{{- with .Values.database.existingSecretHostKey }}
- name: DB_HOST
  valueFrom:
    secretKeyRef:
      name: {{ $.Values.database.existingSecret }}
      key: {{ . }}
{{- end }}
{{- with .Values.database.existingSecretPortKey }}
- name: DB_PORT
  valueFrom:
    secretKeyRef:
      name: {{ $.Values.database.existingSecret }}
      key: {{ . }}
{{- end }}
{{- else if .Values.database.password }}
- name: DB_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $chartSecret }}
      key: DB_PASSWORD
{{- end }}
{{- end }}
{{- if include "pelican-panel.usesRedis" . }}
{{- if .Values.redis.enabled }}
{{- if .Values.redis.existingSecret }}
- name: REDIS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ .Values.redis.existingSecret }}
      key: {{ .Values.redis.existingSecretPasswordKey }}
{{- else if .Values.redis.password }}
- name: REDIS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $chartSecret }}
      key: REDIS_PASSWORD
{{- end }}
{{- else }}
{{- if .Values.externalRedis.existingSecret }}
- name: REDIS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ .Values.externalRedis.existingSecret }}
      key: {{ .Values.externalRedis.existingSecretPasswordKey }}
{{- else if .Values.externalRedis.password }}
- name: REDIS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $chartSecret }}
      key: REDIS_PASSWORD
{{- end }}
{{- end }}
{{- end }}
{{- end }}
