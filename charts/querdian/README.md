# querdian Helm chart

Deploys [Querdian](https://github.com/querdian-dev/querdian), the multi-tenant access proxy for
Prometheus/Thanos, Loki and Tempo. The chart is alpha; the CRD API is `policy.querdian.dev/v1alpha1`.

```sh
helm install querdian oci://ghcr.io/querdian-dev/charts/querdian --version 0.1.2 \
  --namespace querdian --create-namespace -f my-values.yaml
```

From a checkout: `helm install querdian charts/querdian -n querdian --create-namespace -f my-values.yaml`.

The chart and the image it deploys (`ghcr.io/querdian-dev/querdian`, tag = the chart's appVersion)
are signed with cosign by the release workflow. Verify before installing:

```sh
cosign verify ghcr.io/querdian-dev/charts/querdian:0.1.2 \
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
ServiceMonitor and NetworkPolicies. The four CRDs live in `crds/` (Helm installs them on first
install but never upgrades or deletes them; apply `crds/` manually on chart upgrades).

This is `mode: proxy`, the default. For a multi-cluster deployment, see
[Multi-cluster: hub and spoke](#multi-cluster-hub-and-spoke).

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
| `tracing.recordUser` / `tracing.recordQueries` | `false` | Privacy-sensitive span attributes (`enduser.id`, original/rewritten query, query string in the upstream `url.full`). |
| `tracing.trustInboundContext` | `false` | Use the trace context of the client as the parent of the server span. When `false`, the server span starts a new trace and links the trace context of the client. Set it only when all clients are trusted. |
| `tracing.serviceName`, `tracing.exporter`, `tracing.otlp.endpoint`, `tracing.otlp.protocol`, `tracing.sampler`, `tracing.samplerArg` | see values.yaml | Mapped to `OTEL_SERVICE_NAME`, `OTEL_TRACES_EXPORTER`, `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG`. Empty values are not set. Tracing stays off without exporter/endpoint. |
| `tracing.resourceAttributes` | `{}` | Extra `OTEL_RESOURCE_ATTRIBUTES`. `k8s.pod.name`, `k8s.namespace.name`, `k8s.node.name` (Downward API) and `service.version` are always set. |
| `resources` | 100m / 128Mi req, 512Mi mem limit | Container resources. |
| `podSecurityContext`, `securityContext` | non-root (65532), read-only root fs, drop ALL, seccomp RuntimeDefault, no privilege escalation | A `/tmp` emptyDir is mounted. |
| `podAntiAffinityPreset` / `affinity` | `soft` / `{}` | `soft`, `hard` or "" (host-level spreading); `affinity` replaces the preset. |
| `topologySpreadConstraints`, `nodeSelector`, `tolerations`, `priorityClassName` | empty | Scheduling. |
| `podDisruptionBudget.*` | enabled, `minAvailable: 1` | Or set `maxUnavailable`. |
| `serviceMonitor.enabled` / `.labels` / `.interval` / `.namespace` | `false` / {} / `30s` / release ns | Prometheus Operator scraping of `/metrics`. |
| `rbac.create`, `serviceAccount.*` | `true`, create | RBAC: get/list/watch on `clusteraccesspolicies`, `accesspolicies`, `namespaces`; update/patch on both `*/status`; leases and events in the release namespace. With `config.multicluster`, also get/list/watch on `spokeclusters`, `spokepolicies` and update/patch on `spokepolicies/status`. |
| `mode` | `proxy` | `proxy` or `spoke`. See [Multi-cluster: hub and spoke](#multi-cluster-hub-and-spoke). |
| `config.multicluster`, `config.backends[].clusterLabel` | absent | Hub of a multi-cluster deployment (PLAN 18.4). |
| `receiver.*` | disabled | Receiver of a hub. |
| `agent.*` | see values.yaml | Agent of a spoke (only with `mode: spoke`). |
| `prometheusRule.*` | disabled | Alerts for spoke policies. |
| `ui.enabled` / `ui.port` / `ui.auth` / `ui.adminGroup` | `false` / `8081` / `oidc` / "" | Read-only policy dashboard (`--ui-listen`, `--ui-auth`, `--ui-admin-group`). Use `auth: none` only through a port-forward. See [docs/dashboard.md](../../docs/dashboard.md). |
| `ui.allowedHosts` | `[]` | More host names that the dashboard accepts in the `Host` header, with any port (`--ui-allowed-host`). `localhost`, `127.0.0.1` and `[::1]` are always accepted, so a port-forward needs nothing. The chart has no Ingress for the dashboard: if you add one, put its host here. Other hosts get 403. |
| `ui.service.*` | disabled, `ClusterIP`, `8081` | Service for the dashboard port. `NodePort` only for local clusters. |
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

## Multi-cluster: hub and spoke

A multi-cluster deployment (PLAN section 18) has one hub, which runs the proxy and the backends, and
spokes, which send telemetry to the hub. Install the chart one time on the hub and one time on each
spoke. `ci/hub-values.yaml` and `ci/spoke-values.yaml` are complete examples.

Before you set `config.multicluster`, do the migration steps of the
[multi-cluster guide](../../docs/multicluster.md#migration-ordered-steps) in their order. First upgrade the
chart and its CRDs without the block, then examine each hub `ClusterAccessPolicy` and make sure that hub
telemetry has `cluster="<hubCluster>"`.

### Hub (`mode: proxy`)

```yaml
config:
  multicluster:
    hubCluster: mla          # cluster label value of hub telemetry
    maxStaleness: 48h
  backends:
    - name: metrics
      # ...
      clusterLabel: cluster  # required on each backend with multicluster
receiver:
  enabled: true
  service:
    type: LoadBalancer
  tls:
    existingSecret: querdian-receiver-tls   # tls.crt, tls.key
  clientCA:
    existingSecret: querdian-agent-ca       # ca.crt
prometheusRule:
  enabled: true
```

- The schema requires `clusterLabel` on each backend if `config.multicluster` is set. It rejects
  `clusterLabel` if the block is absent, because Querdian refuses that configuration.
- With `config.multicluster`, the proxy ClusterRole also gets get/list/watch on `spokeclusters` and
  `spokepolicies`, and update/patch on `spokepolicies/status` only.
- `receiver.enabled` adds the receiver (`querdian receiver`) as a separate Deployment with its own
  ServiceAccount, Service, ClusterRole and Role. The proxy keeps no write path.
- Receiver RBAC: get/list on `spokeclusters`, update on `spokeclusters/status` only,
  list/create/update/delete on `spokepolicies`, and get/create/update on leases and create/patch on
  events in the release namespace. No wildcard.
- The receiver terminates mTLS itself. A load balancer or an ingress in front of it must pass TLS through.
- With `receiver.service.type: NodePort`, set `receiver.service.nodePort` (30000 to 32767) to keep the same
  port after a reinstall. Otherwise Kubernetes selects a port, and the `agent.receiverURL` of each spoke can
  become incorrect.
- `receiver.clientCA.existingSecret` is the CA of the agent certs. Only the hub may issue certs from this
  CA. Do not use the CA of the ingest certs.
- The receiver reads its cert and the client CA at startup only. After a rotation, restart the receiver.
- The receiver uses leader election, and only the leader reports ready. For this reason, the Deployment
  uses `strategy: Recreate`, and `receiver.replicaCount` is 1 by default. With more replicas, a standby is
  ready to take over, but `kubectl rollout status` and `helm --wait` do not finish, because a standby never
  becomes ready.
- `networkPolicy.enabled` also adds a NetworkPolicy for the receiver. Set
  `networkPolicy.ingress.receiverFrom` to the source of the agent connections. Empty means nobody. With a
  load balancer, the source address that the pod sees can be a node address (for example with
  `externalTrafficPolicy: Cluster`). Then an `ipBlock` with the agent addresses blocks all agents.
- `prometheusRule.enabled` adds a PrometheusRule with two alerts. `QuerdianSpokePolicyStale` fires if
  `querdian_cluster_policy_age_seconds` is more than `prometheusRule.policyAgeThresholdSeconds` (default
  86400, 24 hours). `QuerdianSpokePolicyExpired` fires if `querdian_cluster_policy_expired` is 1. Both
  need Prometheus to scrape the proxy, for example with `serviceMonitor.enabled`.

Each spoke also needs a `SpokeCluster` object on the hub. The chart does not create it.

### Spoke (`mode: spoke`)

```yaml
mode: spoke
agent:
  receiverURL: https://querdian-receiver.mla.example.com:8443
  tls:
    existingSecret: querdian-agent-tls   # tls.crt, tls.key
  receiverCA:
    existingSecret: querdian-agent-tls   # ca.crt
```

- The chart installs only the agent (`querdian agent`): a Deployment, a ServiceAccount, a ClusterRole, a
  Role for the lease, and optionally a NetworkPolicy. It installs no proxy, no ConfigMap, no proxy
  Service and no proxy RBAC. The agent and the proxy are mutually exclusive, because both write the
  `Ready` condition and `status.observedGeneration` of the spoke policies. If you set `agent.receiverURL`,
  `agent.tls.existingSecret` or `agent.receiverCA.existingSecret` with `mode: proxy`, the render stops.
  Do not install a second release with the proxy in a spoke.
- Agent RBAC (the minimum of PLAN section 18.5): get/list/watch on `clusteraccesspolicies`,
  `accesspolicies` and `namespaces`, update/patch on `clusteraccesspolicies/status` and
  `accesspolicies/status`, and get/create/update on leases in the release namespace. No wildcard.
- Use a dedicated agent cert. It must not be the ingest cert, and its URI SAN must be equal to
  `spec.identity` of the `SpokeCluster` object on the hub. The agent reads the files again before each
  request, so a rotated cert takes effect without a restart.
- `networkPolicy.enabled` adds a NetworkPolicy for the agent. Set `networkPolicy.egress.receiver.to` and
  `.ports` to the address of the receiver.
- The chart rejects proxy features in this mode (`receiver`, `prometheusRule`, `config.multicluster`,
  `ui`, `serviceMonitor`, `dashboards`, `networkPolicy.backendProtection`).
- Helm installs all CRDs from `crds/`, also `SpokeCluster` and `SpokePolicy`, which a spoke does not use.
  Nothing on the spoke reads them, and the agent has no access to them. To install only the policy CRDs,
  use `helm install --skip-crds` and apply `crds/policy.querdian.dev_clusteraccesspolicies.yaml` and
  `crds/policy.querdian.dev_accesspolicies.yaml` yourself.

## Development

```sh
make chart-crds        # sync crds/ from config/crd
make chart-lint        # CRD diff, helm lint (default + all features), go test ./charts/...
```

`testdata/*.golden.yaml` hold the render with the default values and with `ci/all-features-values.yaml`.
`TestRenderUnchanged` fails if a change modifies this output. After an intended change, run
`go test ./charts/querdian -run TestRenderUnchanged -update` and review the diff. The test ignores empty
lines, because helm versions differ in them.

`Chart.yaml` carries `0.0.0-dev` for `version`/`appVersion`; the release pipeline overrides both
(`helm package --version X --app-version X`). `ci/*-values.yaml` are picked up by chart-testing (`ct`).
