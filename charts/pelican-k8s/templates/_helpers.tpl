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
{{- printf "http://%s.%s.svc:8081" (include "pelican-k8s.gatewayName" .) .Release.Namespace -}}
{{- end -}}
{{- end -}}

{{- define "pelican-k8s.nodeSecretName" -}}
{{- if .Values.gateway.existingSecret -}}
{{- .Values.gateway.existingSecret -}}
{{- else -}}
{{- printf "%s-node-token" (include "pelican-k8s.gatewayName" .) -}}
{{- end -}}
{{- end -}}
