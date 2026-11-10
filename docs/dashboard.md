# Policy dashboard

An optional, strictly read-only web UI that shows what this Querdian replica is enforcing right now.
It is off by default. It has no write path: it never changes a policy and never contacts a backend.

## Views

* **Overview**: snapshot generation, policy counts (ready / not ready), users, groups, backends. With
  the `multicluster` block, also the number of spoke policies and of expired spoke policies.
* **Policies**: every policy with its `Ready` reason and message: each `ClusterAccessPolicy` and
  `AccessPolicy` of the hub and, with the `multicluster` block, each `SpokePolicy`. Select a row for its
  compiled rules (backend, tenants, matchers, hidden labels, and redacted labels with an example of
  their effect, such as `customer_id: 42 → redacted-1`).
* **Access**: pick a user and groups and see the exact union of rules per backend, drawn as a pass,
  including which labels are hidden and which are redacted with which strategy.
* **Backends**: configured backends (no URLs or credentials) and the policies that reach each.

### Multi-cluster

With the `multicluster` block (see [multi-cluster enforcement](multicluster.md)), the dashboard also
shows where each policy comes from. Without the block, it looks and behaves as described above.

* **Origin column**: `Hub`, or the name of the `SpokeCluster`. The ID of a spoke policy is a hash
  (`SpokePolicy/sp-<64 hex>`), so the Policies view names it by its source object in the spoke: kind,
  namespace and name, for example `AccessPolicy/shop/read`. The ID stays in a tooltip and in the row
  details (Hub object). The view lists hub policies first, then spoke policies by cluster, kind,
  namespace and name.
* **Unverified origin**: for a spoke policy that is not ready, the cluster and the source come from the
  spec of the hub object, which did not pass the checks. The dashboard marks them as unverified.
* **Expired**: a ready spoke policy whose spoke is expired now (no valid sync within `maxStaleness`,
  `suspended: true`, or a `SpokeCluster` change that the snapshot does not contain yet) has a hatched
  Expired mark. Its rules grant no data, so their matchers are struck through. Their hidden and redacted
  labels still apply, and the dashboard shows them as usual. The state comes from the same deadline
  table and the same check as the proxy: a rule is expired also if the table has a different version of the
  `SpokeCluster` than the rule was compiled from. A policy has the Expired mark when all its rules are
  expired.
* **Access and Backends**: the pass and the Backends view name spoke policies by their source and mark
  the expired ones. On the pass, a refused backend lists only its expired policies, for example for
  `policy_source_expired`.
* **API**: `/api/policies` adds `origin` (`hub`, or `cluster`, `kind`, `namespace`, `name` and
  `unverified`) and `expired` to each policy and rule. `/api/access` and `/api/backends` add `sources`
  to each backend: each policy with its `id`, `origin` and `expired` state, in the order of the Policies
  view. `/api/overview` adds `spokePolicies` (`total`, `expired`). The test bench answer does not change
  and does not name the cluster.
* **Test bench**: choose a user, groups, a backend and a query. The dashboard resolves the effective
  policy and runs the same enforcer as the proxy, then shows the incoming and the rewritten query, or
  the refusal and its reason. Unknown users, denied labels and unparseable queries are refused exactly
  as the proxy refuses them. When the user's policy redacts labels, the bench also shows what happens
  to them in the response: values rewritten (query endpoints) or names removed (series and label
  endpoints).

## Enabling it

```sh
querdian --ui-listen=127.0.0.1:8081 --ui-auth=none
```

| Flag | Default | Meaning |
|---|---|---|
| `--ui-listen` | empty (off) | Listener address. Must differ from `--listen` and `--management-listen`. |
| `--ui-auth` | `oidc` | `oidc`: every request needs a valid bearer token. `none`: no authentication. |
| `--ui-admin-group` | empty | With `oidc`: the token must carry this group. Empty: any valid token. |
| `--ui-allowed-host` | none | Host name that the dashboard accepts in the `Host` header, with any port. Repeat the flag for more names. |

The kind quickstart enables it by default on <http://localhost:8081> (see [quickstart](quickstart.md)).

With the Helm chart set `ui.enabled=true` (and `ui.service.enabled=true` for a ClusterIP Service), then
`kubectl port-forward deploy/querdian 8081:8081` and open <http://localhost:8081>.

### Allowed hosts

The dashboard answers a request only when the `Host` header names an allowed host. This check applies to
the API and to the static files, in all authentication modes, and before the authentication. The
dashboard always allows these hosts, with any port:

* `localhost`, `127.0.0.1` and `[::1]`. A port-forward and the kind quickstart use them, so they need no
  configuration.
* The host of `--ui-listen`, when it is a specific host. For example, `--ui-listen=192.0.2.10:8081` allows
  `192.0.2.10`. An empty host, `0.0.0.0` and `[::]` add no host.

Each `--ui-allowed-host` flag adds one host name or IP address, without a port. The dashboard compares
the names without case. It does not accept a wildcard, a scheme or a path. A request with an empty or a
different `Host` header gets `403` with the message `host not allowed`.

When an Ingress or a proxy sends requests to the dashboard, add the host name that the browser uses. With
the Helm chart, set `ui.allowedHosts`:

```yaml
ui:
  enabled: true
  allowedHosts:
    - querdian.example.com
```

The chart does not create an Ingress for the dashboard. If your Ingress or proxy changes the `Host`
header, add the name that arrives at the dashboard.

A request with a method other than `GET`, such as `POST /api/test`, must also come from the dashboard
itself. The dashboard rejects it with `403` and the message `cross-origin request not allowed` when the
`Origin` header has a different host or port than the `Host` header, or when the `Sec-Fetch-Site` header
is not `same-origin` or `none`. Clients that are not browsers, such as `curl`, do not send these headers.
An Ingress or a proxy in front of the dashboard must keep the `Host` header of the browser, or the test
bench does not work.

The dashboard compares IP addresses in canonical form: `--ui-allowed-host=fd00:0::1` accepts
`Host: [fd00::1]`. It does not accept an IPv4 address in IPv6 form, such as `::ffff:10.0.0.5`.

The dashboard logs each refusal at debug level (`dashboard request refused`) with the reason and the
first 64 bytes of the `Host` header. See [troubleshooting](troubleshooting.md#policy-dashboard).

## Security model

The dashboard discloses group names, tenants and label matchers, which describe your tenancy. Treat it
as an administrative surface.

With the `multicluster` block, it also discloses the `SpokeCluster` names, the source objects of each
spoke (kind, namespace and name) and the sync state of each spoke (expired or not). It shows the
policies of all spokes to each dashboard user.

* Browsers cannot send a bearer token on their own. Use `--ui-auth=oidc` behind a proxy that adds the
  token. Use `--ui-auth=none` (Helm: `ui.auth: none`) only on a loopback address or through a
  port-forward (`kubectl port-forward`). Do not use `none` behind an Ingress or on a shared network. With
  `none` Querdian logs a warning at startup.
* The `Host` check protects the dashboard against DNS rebinding, and the origin check protects the test
  bench against requests from other web pages. Add only the names of your own Ingress or proxy to the allowed hosts (see [Allowed hosts](#allowed-hosts)).
* The listener is plain HTTP; terminate TLS in front of it if it leaves the cluster.
* Responses carry `Cache-Control: no-store`, a CSP without inline script, and `nosniff`. The page renders
  all policy text with `textContent`.
* The test bench is a dry run in memory. It is bounded by a 64 KiB body limit and a 5 s planning timeout.
