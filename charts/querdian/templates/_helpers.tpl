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

{{/* True ("true") when the chart installs the proxy (mode proxy). In mode spoke, the chart installs only the agent. */}}
{{- define "querdian.proxyEnabled" -}}
{{- if ne .Values.mode "spoke" -}}true{{- end -}}
{{- end -}}

{{/* True ("true") when the chart installs the receiver of a hub (PLAN section 18.5). */}}
{{- define "querdian.receiverEnabled" -}}
{{- if and (include "querdian.proxyEnabled" .) .Values.receiver.enabled -}}true{{- end -}}
{{- end -}}

{{/*
Name of a component (receiver or agent): <fullname>-<component>, at most 63 characters.
Call with (dict "root" $ "component" "receiver").
*/}}
{{- define "querdian.componentName" -}}
{{- printf "%s-%s" (include "querdian.fullname" .root | trunc (int (sub 62 (len .component))) | trimSuffix "-") .component -}}
{{- end -}}

{{/*
Selector labels of a component. The name label differs from the proxy, so the selectors of the proxy
(Service, PodDisruptionBudget, NetworkPolicy, anti-affinity) never select the pods of a component.
*/}}
{{- define "querdian.componentSelectorLabels" -}}
app.kubernetes.io/name: {{ printf "%s-%s" (include "querdian.name" .root | trunc (int (sub 62 (len .component))) | trimSuffix "-") .component }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "querdian.componentLabels" -}}
helm.sh/chart: {{ include "querdian.chart" .root }}
{{ include "querdian.componentSelectorLabels" . }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/part-of: querdian
{{- with .root.Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
ServiceAccount name of a component.
Call with (dict "root" $ "component" "receiver" "sa" .Values.receiver.serviceAccount).
*/}}
{{- define "querdian.componentServiceAccountName" -}}
{{- if .sa.create -}}
{{- default (include "querdian.componentName" .) .sa.name -}}
{{- else -}}
{{- default "default" .sa.name -}}
{{- end -}}
{{- end -}}

{{/* Egress rules of the receiver and the agent: DNS and the Kubernetes API. Call with .Values.networkPolicy. */}}
{{- define "querdian.componentEgress" -}}
{{- if .egress.dns.enabled }}
- to:
    {{- toYaml .egress.dns.to | nindent 4 }}
  ports:
    - port: 53
      protocol: UDP
    - port: 53
      protocol: TCP
{{- end }}
{{- if .egress.kubeApi.enabled }}
- {{- with .egress.kubeApi.to }}
  to:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  ports:
    {{- range .egress.kubeApi.ports }}
    - port: {{ . }}
      protocol: TCP
    {{- end }}
{{- end }}
{{- end -}}

{{/*
Checks of the values that the schema cannot express. Each failure stops the render, so a wrong
combination never installs.
*/}}
{{- define "querdian.validate" -}}
{{- $v := .Values -}}
{{- /*
The agent and the proxy are mutually exclusive: both write the Ready condition and status.observedGeneration
of the spoke policies. mode selects one of them. Agent values with mode proxy are a sign that the user wants
both, so the render stops.
*/ -}}
{{- if and (ne $v.mode "spoke") (or $v.agent.receiverURL $v.agent.tls.existingSecret $v.agent.receiverCA.existingSecret) -}}
{{- fail "agent values are set, but mode is proxy. A spoke runs only the agent, never the proxy too: set mode: spoke to install only the agent, or remove the agent values" -}}
{{- end -}}
{{- if eq $v.mode "spoke" -}}
{{- if $v.receiver.enabled -}}
{{- fail "receiver.enabled is not possible with mode spoke: the receiver runs on the hub" -}}
{{- end -}}
{{- if $v.prometheusRule.enabled -}}
{{- fail "prometheusRule.enabled is not possible with mode spoke: the proxy of the hub supplies the metrics" -}}
{{- end -}}
{{- if $v.config.multicluster -}}
{{- fail "config.multicluster is not possible with mode spoke: set it on the hub" -}}
{{- end -}}
{{- if or $v.ui.enabled $v.serviceMonitor.enabled $v.dashboards.enabled $v.networkPolicy.backendProtection.enabled -}}
{{- fail "ui, serviceMonitor, dashboards and networkPolicy.backendProtection are proxy features; mode spoke installs only the agent" -}}
{{- end -}}
{{- if not (hasPrefix "https://" $v.agent.receiverURL) -}}
{{- fail "mode spoke needs agent.receiverURL with the https scheme" -}}
{{- end -}}
{{- if not $v.agent.tls.existingSecret -}}
{{- fail "mode spoke needs agent.tls.existingSecret (the client cert and key of the agent)" -}}
{{- end -}}
{{- if not $v.agent.receiverCA.existingSecret -}}
{{- fail "mode spoke needs agent.receiverCA.existingSecret (the CA that verifies the receiver cert)" -}}
{{- end -}}
{{- /* The agent rejects an interval below 10s. A Go duration has many forms (0.1m, 05s, 9000ms), and a regular
expression cannot compare them with 10s. So the chart accepts only <n>s, <n>m and <n>h, with an integer n of 1 to 6
digits and no leading zero, and compares the number of seconds with 10. The digit limit keeps n below the overflow
of atoi and of a Go duration. */ -}}
{{- $hb := toString $v.agent.heartbeatInterval -}}
{{- if not (regexMatch "^[1-9][0-9]{0,5}[smh]$" $hb) -}}
{{- fail "agent.heartbeatInterval must be <n>s, <n>m or <n>h, with an integer n of 1 to 6 digits and no leading zero, and at least 10s" -}}
{{- end -}}
{{- $seconds := mul (atoi (trimSuffix (regexFind "[smh]$" $hb) $hb)) (get (dict "s" 1 "m" 60 "h" 3600) (regexFind "[smh]$" $hb)) -}}
{{- if lt (int64 $seconds) 10 -}}
{{- fail "agent.heartbeatInterval must be at least 10s" -}}
{{- end -}}
{{- if and (not $v.agent.leaderElection.enabled) (gt (int $v.agent.replicaCount) 1) -}}
{{- fail "agent.leaderElection.enabled=false needs agent.replicaCount=1: only one agent may send at a time" -}}
{{- end -}}
{{- end -}}
{{- if $v.receiver.enabled -}}
{{- if not $v.config.multicluster -}}
{{- fail "receiver.enabled needs config.multicluster: without the block, the proxy ignores spoke policies" -}}
{{- end -}}
{{- if not $v.receiver.tls.existingSecret -}}
{{- fail "receiver.enabled needs receiver.tls.existingSecret (the server cert and key)" -}}
{{- end -}}
{{- if not $v.receiver.clientCA.existingSecret -}}
{{- fail "receiver.enabled needs receiver.clientCA.existingSecret (the CA of the agent certs; only the hub may issue from it)" -}}
{{- end -}}
{{- if and (not $v.receiver.leaderElection.enabled) (gt (int $v.receiver.replicaCount) 1) -}}
{{- fail "receiver.leaderElection.enabled=false needs receiver.replicaCount=1: one receiver must handle a cluster at a time" -}}
{{- end -}}
{{- if eq (int $v.receiver.port) (int $v.receiver.managementPort) -}}
{{- fail "receiver.port and receiver.managementPort must differ" -}}
{{- end -}}
{{- end -}}
{{- if and $v.prometheusRule.enabled (not $v.config.multicluster) -}}
{{- fail "prometheusRule.enabled needs config.multicluster: the alerts use the metrics of spoke policies" -}}
{{- end -}}
{{- end -}}
