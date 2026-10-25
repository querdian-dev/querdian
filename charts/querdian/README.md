# querdian Helm chart

Deploys [Querdian](https://github.com/querdian-dev/querdian), the multi-tenant access proxy for
Prometheus/Thanos, Loki and Tempo. The chart is alpha; the CRD API is `policy.querdian.dev/v1alpha1`.

```sh
helm install querdian oci://ghcr.io/querdian-dev/charts/querdian --version 0.1.0 \
  --namespace querdian --create-namespace -f my-values.yaml
```

From a checkout: `helm install querdian charts/querdian -n querdian --create-namespace -f my-values.yaml`.

The chart and the image it deploys (`ghcr.io/querdian-dev/querdian`, tag = the chart's appVersion)
are signed with cosign by the release workflow. Verify before installing:

```sh
cosign verify ghcr.io/querdian-dev/charts/querdian:0.1.0 \
  --certificate-identity-regexp '^https://github.com/querdian-dev/querdian/\.github/workflows/release\.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Release notes and all versions: [GitHub releases](https://github.com/querdian-dev/querdian/releases),
[Artifact Hub](https://artifacthub.io/packages/helm/querdian/querdian).

Requirements: Kubernetes >= 1.27. The `ServiceMonitor` needs the Prometheus Operator CRDs, the
NetworkPolicies need a CNI that enforces them.

## What gets installed

Deployment (2 replicas, leader election, soft pod anti-affinity), Service, ConfigMap with the static
config (checksum annotation triggers a rollout on change), ServiceAccount, ClusterRole + Role
(leases, events) with bindings, PodDisruptionBudget, and optionally a management Service,
ServiceMonitor and NetworkPolicies. The two CRDs live in `crds/` (Helm installs them on first
install but never upgrades or deletes them; apply `crds/` manually on chart upgrades).

CRD sync: `crds/` is a copy of `config/crd` in the repository. `make chart-crds` refreshes it,
`make chart-crds-check` (and `go test ./charts/...`) fail when they differ.

## Values that matter

The defaults for `config` are examples. **You must set `config.auth` and `config.backends`.**

| Value | Default | Description |
|---|---|---|
| `image.repository` / `image.tag` / `image.digest` | `ghcr.io/querdian-dev/querdian` / appVersion / "" | Image. `digest` wins over `tag`. |
| `replicaCount` | `2` | Replicas. All serve; one is the status-writing leader. |
| `leaderElection.enabled` | `true` | `--leader-elect` (Lease `querdian-leader-election` in the release namespace). |
| `proxy.port` / `service.port` / `service.type` | `8080` / `8080` / `ClusterIP` | Proxy listener and Service. |
| `management.port` | `9090` | `/metrics`, `/healthz`, `/readyz`. |
| `management.service.enabled` | `false` | Separate ClusterIP Service for the management port (implied by `serviceMonitor.enabled`). |
| `logLevel` / `logFormat` | `info` / `json` | Process flags. |
| `config.auth` | example Keycloak | `issuer`, `audiences` (required), `claims.username`, `claims.groups`. |
| `config.backends` | example 3 backends | List of `{name,type,url,pathPrefix,namespaceLabel,tenantHeader,tenantFederation,traceByID}` (PLAN 5.1). |
| `config.limits` | defaults | `maxPolicyTerms`, `maxFanout`, `maxMergeBytes`, `upstreamTimeout`. |
| `oidcCA.existingSecret` / `oidcCA.existingConfigMap` / `oidcCA.key` | "" / "" / `ca.crt` | Extra CA bundle for the OIDC issuer. Mounted at `/etc/querdian-oidc-ca`; `config.auth.caFile` is set automatically. |
| `tracing.recordUser` / `tracing.recordQueries` | `false` | Privacy-sensitive span attributes (`enduser.id`, original/rewritten query). |
| `tracing.serviceName`, `tracing.exporter`, `tracing.otlp.endpoint`, `tracing.otlp.protocol`, `tracing.sampler`, `tracing.samplerArg` | see values.yaml | Mapped to `OTEL_SERVICE_NAME`, `OTEL_TRACES_EXPORTER`, `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG`. Empty values are not set. Tracing stays off without exporter/endpoint. |
| `tracing.resourceAttributes` | `{}` | Extra `OTEL_RESOURCE_ATTRIBUTES`. `k8s.pod.name`, `k8s.namespace.name`, `k8s.node.name` (Downward API) and `service.version` are always set. |
| `resources` | 100m / 128Mi req, 512Mi mem limit | Container resources. |
| `podSecurityContext`, `securityContext` | non-root (65532), read-only root fs, drop ALL, seccomp RuntimeDefault, no privilege escalation | A `/tmp` emptyDir is mounted. |
| `podAntiAffinityPreset` / `affinity` | `soft` / `{}` | `soft`, `hard` or "" (host-level spreading); `affinity` replaces the preset. |
| `topologySpreadConstraints`, `nodeSelector`, `tolerations`, `priorityClassName` | empty | Scheduling. |
| `podDisruptionBudget.*` | enabled, `minAvailable: 1` | Or set `maxUnavailable`. |
| `serviceMonitor.enabled` / `.labels` / `.interval` / `.namespace` | `false` / {} / `30s` / release ns | Prometheus Operator scraping of `/metrics`. |
| `rbac.create`, `serviceAccount.*` | `true`, create | RBAC: get/list/watch on `clusteraccesspolicies`, `accesspolicies`, `namespaces`; update/patch on both `*/status`; leases and events in the release namespace. |
| `extraArgs`, `extraEnv`, `extraVolumes`, `extraVolumeMounts` | empty | Escape hatches. |
| `networkPolicy.*` | disabled | See below. |

`values.schema.json` is strict: unknown keys and invalid enums/ranges fail `helm install/lint/template`.

## NetworkPolicy

`networkPolicy.enabled=true` restricts the querdian pods:

- Ingress to the proxy port only from `networkPolicy.ingress.from` (a list of `NetworkPolicyPeer`,
  e.g. Grafana). Empty means nobody. The management port is open only to `ingress.managementFrom`
  (e.g. Prometheus).
- Egress to DNS (`egress.dns`), the Kubernetes API (`egress.kubeApi`, ports 443/6443, set `to` to the
  API server address to tighten), the backends (`egress.backends.to` / `.ports`: match your
  `config.backends[].url`), the OIDC issuer (`egress.oidc`, 443 by default) and optionally an OTLP
  collector (`egress.tracing`). Raw extra rules go to `egress.extra`.

Example for a Grafana in namespace `grafana`:

```yaml
networkPolicy:
  enabled: true
  ingress:
    from:
      - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: grafana}}
```

### Restricting the backends to querdian (PLAN section 12)

Clients must not reach the backends directly. With `networkPolicy.backendProtection` the chart
renders one ingress policy per target in the **backend** namespace that admits querdian pods only
(plus `allowFrom`):

```yaml
networkPolicy:
  backendProtection:
    enabled: true
    targets:
      - namespace: monitoring
        podSelector: {matchLabels: {app.kubernetes.io/name: loki}}
        ports: [3100]
        allowFrom:                     # writers etc. that must keep working
          - podSelector: {matchLabels: {app.kubernetes.io/name: alloy}}
```

Selecting backend pods with an ingress policy denies every source that is not listed, so include all
legitimate writers/scrapers in `allowFrom`. `ci/all-features-values.yaml` is a complete example.

## Development

```sh
make chart-crds        # sync crds/ from config/crd
make chart-lint        # CRD diff, helm lint (default + all features), go test ./charts/...
```

`Chart.yaml` carries `0.0.0-dev` for `version`/`appVersion`; the release pipeline overrides both
(`helm package --version X --app-version X`). `ci/*-values.yaml` are picked up by chart-testing (`ct`).
