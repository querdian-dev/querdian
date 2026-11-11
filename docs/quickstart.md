# Quickstart on kind

This demo shows the isolation between two teams on a laptop. One script makes a single-node
[kind](https://kind.sigs.k8s.io) cluster with Keycloak, Prometheus, Loki, Tempo, Grafana, two sample
services and Querdian with example policies.

In short:

* Three demo users log in to Grafana with Keycloak. Each user sees different metrics, logs and traces.
* The backends contain 24 hours of synthetic data with three incidents. Grafana has four dashboards.
* `verify.sh` proves the isolation with 153 automatic checks.
* `up.sh` needs about 3 minutes. The first run needs 5 to 10 minutes, because it downloads the images.

> **Demo only.** Keycloak runs in dev mode over plain HTTP with known passwords. The demo OAuth client
> permits the password grant, and no data is persistent. Do not use this configuration in production.
> Querdian is alpha.

The [quickstart reference](quickstart-details.md) gives the details: options, components, demo data,
dashboards and internals. For a hub and two spoke clusters, see the
[multi-cluster quickstart](quickstart-multicluster.md).

## Run

```sh
hack/quickstart/up.sh       # makes the cluster and all components, then prints the URLs
hack/quickstart/verify.sh   # 153 automatic checks, about 25 seconds
hack/quickstart/down.sh     # removes the cluster and the built image
```

## Requirements

* `kind` (tested with v0.33), `kubectl`, `helm` 3 or 4, `make`, `curl` and `jq`.
* Docker, or Podman with the `docker` CLI shim.
* About 3 GiB of free memory for the container VM, and about 5 GiB of disk space for the images.
* Free host ports: `8080` (Keycloak), `3000` (Grafana) and `8081` (policy dashboard).
  `KEYCLOAK_PORT=8082` changes the Keycloak port.
* Internet access. The kind node pulls the images from Docker Hub and quay.io.

`up.sh` builds Querdian from the checkout. To use the published image, or to change other settings, see
[Options](quickstart-details.md#options).

## Users

| User | Password | Group | Data that the user sees |
|---|---|---|---|
| alice | `alice-demo` | `order-team` | only `order-service` |
| bob | `bob-demo` | `payment-team` | only `payment-service` |
| carol | `carol-demo` | `platform-admins` | all data of the tenant `demo`, also the telemetry of Querdian and of the stack |

Three example policies give this access. See [Policies](quickstart-details.md#policies).

## See the isolation in Grafana

1. Open <http://localhost:3000>. Use `localhost`, not `127.0.0.1`.
2. Select **Sign in with Keycloak** and log in as `alice`.
3. Open the dashboard **What can I see?** in the folder *Querdian demo*. It shows the namespaces and
   the labels that each backend gives to alice.
4. Open a private window. Log in as `bob`, then as `carol`, and compare.

The local Grafana admin (`admin` / `admin`) has no OAuth token, thus Querdian refuses its queries.
This is the intended result: use the Keycloak login.

To see the enforcement for each query, open **Explore** as alice:

| Data source | Query | Result for alice |
|---|---|---|
| Prometheus (via Querdian) | `count by (namespace) (up)` | only `order-service` |
| Prometheus (via Querdian) | `sum by (customer_id) (demo_requests_total)` | `403`: the label `customer_id` is denied for the order team |
| Prometheus (via Querdian) | `demo_requests_total` | the values of `customer_id` are `redacted-1`, `redacted-2`, ..., with the warning *label values redacted by policy* |
| Loki (via Querdian) | `{k8s_namespace_name=~".+"}` | only logs of `order-service` |
| Loki (via Querdian) | `{k8s_namespace_name="payment-service"}` | no data |
| Tempo (via Querdian), query type TraceQL | `{}` | only traces of `order-service` |

* Bob gets the data of `payment-service` for the same queries. He sees `c-1`, `c-2`, ... as the values
  of `customer_id`.
* Carol sees the two services, and also `querdian` and `observability`.
* Tempo search results for alice and bob do not show `rootServiceName` and similar trace-level fields.
  Querdian redacts them when the policy cannot prove that the root span matches (see
  [limitations](limitations.md)). Carol sees them.

To follow an incident from metrics to logs to traces:

1. On **Service overview (RED)**, find the 5xx ratio of `order-service` about 3 hours 25 minutes
   before the start of `up.sh`.
2. On **Logs**, open an error line and select **TraceID**. Grafana opens the trace in Tempo.
3. On the trace, select *Logs* to go back to the log lines of that trace.

## See the policies in the policy dashboard

The quickstart enables the read-only [policy dashboard](dashboard.md) on <http://localhost:8081>. It
listens on `127.0.0.1` without authentication, for the laptop demo only.

1. Open **Policies**. It shows the three example policies and their status.
2. Open **Access** or **Test bench** and enter a group: `order-team`, `payment-team` or
   `platform-admins`. Policies match groups, not user names.
3. Try `demo_requests_total` for `order-team` and for `platform-admins`.
4. Try `sum by (customer_id) (demo_requests_total)` for `order-team` to see the denial.

## Verify

`hack/quickstart/verify.sh` does the checks of this page automatically. It stops with a non-zero exit
code if a check fails. The script:

* gets tokens for the three users and sends queries directly to Querdian;
* logs in to Grafana as a browser does, and sends queries through Grafana;
* runs each query of each dashboard panel as each user.

It does not examine how Grafana shows the results. See
[Verification](quickstart-details.md#verification) for the full list.

## If a step fails

| Symptom | Cause and correction |
|---|---|
| `up.sh` fails | The script keeps the cluster and prints the pods that do not run. Read the logs: `kubectl --context kind-querdian-demo -n <ns> logs deploy/<name>`. `CLEANUP_ON_FAILURE=1` deletes the cluster after a failure. |
| `up.sh` stops at the image pulls | Run `down.sh`, then start again. |
| The login fails with "invalid redirect", or the issuer does not match | Open `http://localhost:3000`, not `127.0.0.1`. Make sure that `keycloak.localtest.me` resolves (see [Hostnames](quickstart-details.md#hostnames)). |
| Grafana shows `401` or "Authentication to data source failed" for the local admin | This is the intended result. Log in with Keycloak. |
| The queries fail after 30 minutes | The tokens are valid for 30 minutes (realm setting). Log out, then log in again. |

For the decision reasons of Querdian, see [troubleshooting](troubleshooting.md).
