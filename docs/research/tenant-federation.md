# Research T0.4 — Tenant federation and `__tenant_id__`

Clean-room note: all results below come from black-box HTTP experiments against released
container images (plus public grafana.com documentation for the config option names). No
upstream source code was read.

Reproduce with `hack/research/tenant-federation/{loki,tempo,mimir}.sh` (each starts one
container with pinned image, loads data for tenants `t1` and `t2`, runs the queries, and removes
the container). Run them one at a time. Requires `docker` (or the Podman shim), `curl`, ~2 GiB RAM.
Ports used are all above 1024 (13100, 13200, 14318, 19009).

## Results

| Backend | Version tested | `X-Scope-OrgID: t1\|t2` works for queries | Filter by `__tenant_id__` | Recommended `tenantFederation` |
|---|---|---|---|---|
| Loki | grafana/loki 3.5.0 | Yes (needs `querier.multi_tenant_queries_enabled: true`) | Yes, label matcher (`=`, `=~`, `!=`) | `tenantIdLabel` |
| Mimir | grafana/mimir 2.16.0 | Yes (needs `tenant_federation.enabled: true`) | Yes, label matcher (`=`, `=~`, `!=`) | `tenantIdLabel` |
| Tempo | grafana/tempo 2.8.1 | Yes for search, trace-by-ID v1/v2 and tag listing (needs `multitenancy_enabled: true` and `query_frontend.multi_tenant_queries_enabled: true`) | **No**: `resource.`/`span.`/unscoped `__tenant_id__` never match (no error, empty result; also for `!=`) | `none` |
| Thanos | not tested | n/a | n/a | `none` (reference only; Thanos Query has no `t1\|t2` federation concept that we could verify, and its tenant handling is on the write path) |

## Details and caveats

### Loki 3.5.0
- `{app=~".+"}` with `t1|t2` returns streams of both tenants; each stream carries an extra
  `__tenant_id__` label (also listed by `/loki/api/v1/labels`).
- `{__tenant_id__="t1",app=~".+"}` returns only t1; `=~"t1|t2"` returns both; `!="t1"` returns only t2.
- Selector with `__tenant_id__` as the only matcher fails with 400: "queries require at least
  one regexp or equality matcher that does not have an empty-compatible value" (the tenant
  matcher is stripped before execution). Querdian always adds a namespace matcher, but the
  rewriter must guarantee another non-empty matcher exists.
- With a single-tenant header (`t1`) there is no `__tenant_id__` label: `{__tenant_id__="t1",...}`
  silently returns **nothing**. The matcher must only be injected when the header is a federation
  header.

### Mimir 2.16.0
- `up` with `t1|t2` returns series of both tenants with a `__tenant_id__` label; `up{__tenant_id__="t1"}`,
  `=~`, `!=` filter as expected.
- Single-tenant header + `__tenant_id__="t1"` returns an empty result (label absent), same
  caveat as Loki.
- Data was ingested via a Prometheus 3.5.0 container using remote write with per-tenant headers.

### Tempo 2.8.1
- Federated `{ }` search returns traces of both tenants; trace-by-ID (`/api/traces/<id>`,
  `/api/v2/traces/<id>`) with `t1|t2` finds a trace that lives only in t2; `/api/search/tags` works.
- No tenant attribute is exposed: `{ resource.__tenant_id__ = "t1" }`, `{ .__tenant_id__ = "t1" }`,
  `{ span.__tenant_id__ = "t1" }` all return zero traces without error. Silent empty results mean a
  rewriter relying on it would fail closed for allowed data but is useless; do not use it.
- Consequence: for Tempo, cross-tenant unions with differing per-tenant terms must be rejected
  (`cross_tenant_union_unsupported`, PLAN §6.4), or fanned out per tenant (if/when implemented).
  Identical terms across tenants can use the federated header directly.

### Thanos
Not run. Thanos Query is stateless over StoreAPIs and the multi-tenancy header is a Thanos
Receive (write path) feature; behind a Mimir-style `t1|t2` query header it has no equivalent.
Treated as `tenantFederation: none` / single-tenant backend (`tenantHeader: ""`).

## Open follow-ups
- Re-verify on newer/older minor versions by overriding `LOKI_IMAGE`, `TEMPO_IMAGE`, `MIMIR_IMAGE`.
- Behaviour of `X-Scope-OrgID: t1|t1` (duplicate tenants) and tenant IDs with special characters
  is untested.
