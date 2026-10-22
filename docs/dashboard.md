# Policy dashboard

An optional, strictly read-only web UI that shows what this Querdian replica is enforcing right now.
It is off by default. It has no write path: it never changes a policy and never contacts a backend.

## Views

* **Overview**: snapshot generation, policy counts (ready / not ready), users, groups, backends.
* **Policies**: every `ClusterAccessPolicy` and `AccessPolicy` with its `Ready` reason and message.
  Select a row for its compiled rules (backend, tenants, matchers, hidden labels).
* **Access**: pick a user and groups and see the exact union of rules per backend, drawn as a pass.
* **Backends**: configured backends (no URLs or credentials) and the policies that reach each.
* **Test bench**: choose a user, groups, a backend and a query. The dashboard resolves the effective
  policy and runs the same enforcer as the proxy, then shows the incoming and the rewritten query, or
  the refusal and its reason. Unknown users, denied labels and unparseable queries are refused exactly
  as the proxy refuses them.

## Enabling it

```sh
querdian --ui-listen=127.0.0.1:8081 --ui-auth=none
```

| Flag | Default | Meaning |
|---|---|---|
| `--ui-listen` | empty (off) | Listener address. Must differ from `--listen` and `--management-listen`. |
| `--ui-auth` | `oidc` | `oidc`: every request needs a valid bearer token. `none`: no authentication. |
| `--ui-admin-group` | empty | With `oidc`: the token must carry this group. Empty: any valid token. |

The kind quickstart enables it by default on <http://localhost:8081> (see [quickstart](quickstart.md)).

With the Helm chart set `ui.enabled=true` (and `ui.service.enabled=true` for a ClusterIP Service), then
`kubectl port-forward deploy/querdian 8081:8081` and open <http://localhost:8081>.

## Security model

The dashboard discloses group names, tenants and label matchers, which describe your tenancy. Treat it
as an administrative surface.

* Browsers cannot send a bearer token on their own. Use `--ui-auth=oidc` behind a proxy that adds the
  token, or `--ui-auth=none` on a trusted network only (`kubectl port-forward`, a loopback address). With
  `none` Querdian logs a warning at startup.
* The listener is plain HTTP; terminate TLS in front of it if it leaves the cluster.
* Responses carry `Cache-Control: no-store`, a CSP without inline script, and `nosniff`. The page renders
  all policy text with `textContent`.
* The test bench is a dry run in memory. It is bounded by a 64 KiB body limit and a 5 s planning timeout.
