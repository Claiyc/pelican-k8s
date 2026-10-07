{{- define "pelican-k8s.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "pelican-k8s.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "pelican-k8s.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/part-of: pelican-k8s
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{- define "pelican-k8s.tag" -}}
{{- default .Chart.AppVersion .Values.image.tag -}}
{{- end -}}

{{- define "pelican-k8s.image" -}}
{{- printf "%s/%s:%s" .root.Values.image.registry .name (include "pelican-k8s.tag" .root) -}}
{{- end -}}

{{- define "pelican-k8s.gatewayName" -}}
{{- printf "%s-gateway" (include "pelican-k8s.fullname" .) -}}
{{- end -}}

{{- define "pelican-k8s.operatorName" -}}
{{- printf "%s-operator" (include "pelican-k8s.fullname" .) -}}
{{- end -}}

{{- define "pelican-k8s.serversNamespace" -}}
{{- .Values.serversNamespace.name -}}
{{- end -}}

{{- define "pelican-k8s.remoteURL" -}}
{{- if .Values.gateway.remoteURL -}}
{{- .Values.gateway.remoteURL -}}
{{- else -}}
{{- printf "%s://%s.%s.svc:8081" (ternary "https" "http" .Values.tls.enabled) (include "pelican-k8s.gatewayName" .) .Release.Namespace -}}
{{- end -}}
{{- end -}}

{{- define "pelican-k8s.nodeSecretName" -}}
{{- if .Values.gateway.existingSecret -}}
{{- .Values.gateway.existingSecret -}}
{{- else -}}
{{- printf "%s-node-token" (include "pelican-k8s.gatewayName" .) -}}
{{- end -}}
{{- end -}}

{{- define "pelican-k8s.caSecretName" -}}
{{- printf "%s-ca" (include "pelican-k8s.fullname" .) -}}
{{- end -}}

{{- define "pelican-k8s.gatewayTLSSecretName" -}}
{{- printf "%s-tls" (include "pelican-k8s.gatewayName" .) -}}
{{- end -}}

{{- define "pelican-k8s.operatorTLSSecretName" -}}
{{- printf "%s-tls" (include "pelican-k8s.operatorName" .) -}}
{{- end -}}

{{/* The cert-manager issuer of every certificate: the configured one, or the chart's own CA. */}}
{{- define "pelican-k8s.certManagerIssuer" -}}
{{- $ref := .Values.tls.certManager.issuerRef | default dict -}}
{{- if $ref.name -}}
name: {{ $ref.name }}
kind: {{ $ref.kind | default "ClusterIssuer" }}
group: {{ $ref.group | default "cert-manager.io" }}
{{- else -}}
name: {{ include "pelican-k8s.caSecretName" . }}
kind: ClusterIssuer
group: cert-manager.io
{{- end -}}
{{- end -}}

{{/* DNS names on the gateway's certificate: its Service, and the host of a custom remoteURL. */}}
{{- define "pelican-k8s.gatewayTLSNames" -}}
{{- $gw := include "pelican-k8s.gatewayName" . -}}
{{- $names := list $gw (printf "%s.%s" $gw .Release.Namespace) (printf "%s.%s.svc" $gw .Release.Namespace) (printf "%s.%s.svc.cluster.local" $gw .Release.Namespace) -}}
{{- with .Values.gateway.remoteURL -}}
{{- $host := (urlParse .).host | splitList ":" | first -}}
{{- if not (has $host $names) -}}
{{- $names = append $names $host -}}
{{- end -}}
{{- end -}}
{{- join "," $names -}}
{{- end -}}
