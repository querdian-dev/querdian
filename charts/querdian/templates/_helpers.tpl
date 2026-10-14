{{- define "querdian.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "querdian.fullname" -}}
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

{{- define "querdian.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "querdian.selectorLabels" -}}
app.kubernetes.io/name: {{ include "querdian.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "querdian.labels" -}}
helm.sh/chart: {{ include "querdian.chart" . }}
{{ include "querdian.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: querdian
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "querdian.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "querdian.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "querdian.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{/* True ("true") when the management Service must exist. */}}
{{- define "querdian.managementService" -}}
{{- if or .Values.management.service.enabled .Values.serviceMonitor.enabled -}}true{{- end -}}
{{- end -}}

{{/* Effective config: values.config plus auth.caFile when an OIDC CA bundle is mounted. */}}
{{- define "querdian.config" -}}
{{- $cfg := deepCopy .Values.config -}}
{{- if or .Values.oidcCA.existingSecret .Values.oidcCA.existingConfigMap -}}
{{- $_ := set $cfg.auth "caFile" (printf "/etc/querdian-oidc-ca/%s" .Values.oidcCA.key) -}}
{{- end -}}
{{- toYaml $cfg -}}
{{- end -}}

{{/* OTEL_RESOURCE_ATTRIBUTES; $(VAR) references are expanded by the kubelet from earlier env entries. */}}
{{- define "querdian.resourceAttributes" -}}
{{- $attrs := list "k8s.pod.name=$(K8S_POD_NAME)" "k8s.namespace.name=$(K8S_NAMESPACE_NAME)" "k8s.node.name=$(K8S_NODE_NAME)" (printf "service.version=%s" .Chart.AppVersion) -}}
{{- range $k, $v := .Values.tracing.resourceAttributes -}}
{{- $attrs = append $attrs (printf "%s=%s" $k $v) -}}
{{- end -}}
{{- join "," $attrs -}}
{{- end -}}
