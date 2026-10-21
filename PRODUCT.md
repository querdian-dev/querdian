# Product

## What it is
Querdian (query + guardian) is an Apache-2.0, Kubernetes-native reverse proxy that enforces label-based
access control on read queries against Prometheus/Thanos (PromQL), Loki (LogQL) and Tempo (TraceQL).
Policies are Kubernetes CRDs (`ClusterAccessPolicy`, `AccessPolicy`). Queries are rewritten with real
parsers, never with regexes. Everything fails closed: unknown endpoints, unknown users and unparseable
queries are rejected.

Unique mechanism: one policy language, three query languages. A team's identity (OIDC group) becomes
label matchers that are injected into every query before it reaches the backend, so a team can only ever
read its own namespace's metrics, logs and traces.

## Status (must be stated plainly on every public surface)
Pre-alpha, not released, no published image or chart. Repository is public. No adopters, no
benchmarks, no production claims. Known limits are documented in docs/limitations.md.

## Audience
Broad cloud-native / CNCF audience evaluating multi-tenant observability: platform and SRE engineers
first, plus security reviewers and maintainers of Prometheus/Loki/Tempo stacks. Do not assume deep
PromQL/LogQL/TraceQL or Kubernetes-RBAC knowledge on the landing page; explain the problem in one scene.

## Surfaces
- Landing page (Persuade): querdian.dev, GitHub Pages. What it is, why it matters, what to do next.
- Documentation (Read): the existing docs/ markdown, rendered by Hugo on the same site.

## Action the visitor should take
Read the docs and run the kind quickstart (`hack/quickstart/up.sh`), which gives a laptop demo with Keycloak,
Grafana, two teams and 24 h of data. Star/watch the repo once it is public.

## Real proof available (use this, invent nothing)
- Real policy YAML (hack/quickstart/manifests/policies.yaml).
- Real rewrites from the test suite: `rate(http_requests_total[5m])` becoming a query with the team's matcher.
- 150-check quickstart verification; three differential E2E suites against real Prometheus, Loki and Tempo.
- Threat model and self-assessment in docs/security/.
- Supply chain: cosign keyless signing, SBOM, provenance attestations, pinned digests (pipeline not yet run publicly).

## Not allowed
Invented customers, logos, quotes, benchmarks, performance numbers, "production ready", "trusted by",
CNCF affiliation claims, a fake terminal full of fabricated output. Illustrative material is allowed only
when labelled as an example.

## Brand commitments
Name: Querdian. Domain: querdian.dev. Licence: Apache-2.0. Inspired by evoila/janus (credit in README only).
Tone: precise, calm, security-literate; no hype, no gamification.
