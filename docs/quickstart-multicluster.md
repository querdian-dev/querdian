# Multi-cluster quickstart on kind

This demo shows [multi-cluster enforcement](multicluster.md) on one laptop, with one hub and two spoke
[kind](https://kind.sigs.k8s.io) clusters.

In short:

* The **hub** has the backends, Keycloak, Grafana and Querdian. The two **spokes** run only the
  Querdian agent, and they send their telemetry to the hub.
* All three clusters have a namespace with the same name, `order-service`. A different team owns it in
  each cluster, and each team sees only the data of its own cluster.
* `up.sh` follows the [ordered migration steps](multicluster.md#migration-ordered-steps). It tests the
  ingest gateway with forged cluster labels before it activates a spoke.
* `verify.sh` checks the isolation and the suspension of a spoke through the Querdian proxy.
* The container VM needs about 8 GiB of memory, and `up.sh` needs about 8 to 20 minutes. These values
  are estimates.

> **Demo only.** Keycloak runs in dev mode over plain HTTP with known passwords. All CA keys are in one
> directory on the laptop. The ingest gateway is a demo pattern, not a part of Querdian. The pods of the
> spokes can get access to Keycloak, Grafana and the dashboard of the hub (see
> [limits of the gateway](quickstart-multicluster-details.md#limits-of-this-design)). Do not use any of
> it in production. The multi-cluster feature is alpha.

This demo uses the stack of the [single-cluster quickstart](quickstart.md): read it first. The
[multi-cluster quickstart reference](quickstart-multicluster-details.md) gives the details: options,
topology, certificates, the ingest gateway, the ordered setup and the checks.

## Run

```sh
hack/quickstart-multicluster/up.sh       # three clusters and the ordered setup, ends with the URLs
hack/quickstart-multicluster/verify.sh   # automatic checks through the Querdian proxy
hack/quickstart-multicluster/down.sh     # removes the clusters, the certs and the built image
```

`up.sh` sets the current kubectl context to the hub. For the environment variables, see
[Options](quickstart-multicluster-details.md#options).

## Requirements

* The tools of the single-cluster quickstart: `kind`, `kubectl`, `helm`, `make`, `curl`, `jq`, and
  Docker or Podman with the `docker` shim.
* `openssl` (OpenSSL 1.1 or later; LibreSSL was not tested).
* Bash 3.2 or later. The scripts do not use features of later versions.
* A container VM with **8 GiB** of memory and 4 CPUs or more, and about 6 GiB of disk space (estimates, see
  [resources](quickstart-multicluster-details.md#resources-and-run-time)).
* Free host ports `8080` (Keycloak), `3000` (Grafana) and `8081` (policy dashboard). The single-cluster
  quickstart uses the same three ports, so remove it first (`hack/quickstart/down.sh`).
  `KEYCLOAK_PORT` changes only the port of Keycloak.
* Free local ports `18080`, `19090`, `13100` and `13200` for the port-forwards of the scripts.
* Internet access. Each kind node pulls its images from Docker Hub and quay.io.

## Users

| User | Password | Group | Data that the user sees |
|---|---|---|---|
| alice | `alice-demo` | `team-a` | `order-service` of spoke-a |
| bob | `bob-demo` | `team-b` | `order-service` of spoke-b |
| hana | `hana-demo` | `team-hub` | `order-service` of the hub |
| sam | `sam-demo` | `sre` | all namespaces of both spokes, not the hub |
| carol | `carol-demo` | `platform-admins` | everything in tenant `demo` |

alice and bob get their access from an `AccessPolicy` in their spoke. The other users get it from hub
policies. See [Policies](quickstart-multicluster-details.md#policies).

## Try it

### See the isolation in Grafana

1. Open <http://localhost:3000>.
2. Select **Sign in with Keycloak** and log in as alice.
3. In **Explore**, select *Prometheus (via Querdian)*.
4. Run `count by (cluster, namespace) (demo_requests_total)`. The result shows only `spoke-a`,
   `order-service`.
5. Log in as bob, hana and sam in private windows, and compare.

The four dashboards of the single-cluster quickstart are also available. They have no cluster variable.
Thus a user with grants in several clusters sees the sum of those clusters.

### See the origin of the policies

1. Open the policy dashboard on <http://localhost:8081>.
2. Open **Policies**. It shows the origin of each policy: `hub`, or the spoke cluster with the kind,
   namespace and name of the source object.
3. Open **Access** and enter the group `team-a`, `team-b`, `team-hub` or `sre`.

### Suspend a spoke

1. On the hub, suspend spoke-a:

   ```sh
   kubectl --context kind-querdian-hub patch spokecluster spoke-a --type merge -p '{"spec":{"suspended":true}}'
   ```

2. Query as alice. The queries of alice now get `403` with the message
   `the policy source of this grant has expired`. The dashboard shows the spoke policy as expired. bob,
   sam and carol are not affected.
3. Set `"suspended":false` again. The grants return after the next sync of the agent. The heartbeat of
   the demo is 15 seconds.

### See the state on the hub and in the spokes

```sh
kubectl --context kind-querdian-hub get spokeclusters,spokepolicies
kubectl --context kind-querdian-spoke-a -n order-service get accesspolicy   # Ready, written by the agent
```

## Verify

`hack/quickstart-multicluster/verify.sh` gets tokens for the five users and queries the Querdian proxy
of the hub directly. It stops with a non-zero exit code if a check fails. It checks:

* the isolation on all three backends;
* the access of sam and carol;
* the state of the `SpokeCluster` and `SpokePolicy` objects;
* the suspension and the unsuspension of spoke-a;
* Grafana and its data sources.

See [Automatic verification](quickstart-multicluster-details.md#automatic-verification) for the full
list.

## If a step fails

| Symptom | Cause and correction |
|---|---|
| `up.sh` fails | The script keeps the clusters and prints the pods that do not run in each cluster. `CLEANUP_ON_FAILURE=1` deletes the clusters after a failure. |
| A collector or a kubelet fails with "too many open files" (Linux) | Three kind clusters can reach the inotify limits of the host. Increase `fs.inotify.max_user_instances` and `fs.inotify.max_user_watches` as the kind [known issues](https://kind.sigs.k8s.io/docs/user/known-issues/#pod-errors-due-to-too-many-open-files) describe. |
| A spoke does not sync | Examine the agent log (`kubectl --context kind-querdian-spoke-a -n querdian logs deploy/querdian-agent`) and the receiver log on the hub. `403` while the spoke is suspended is correct. See [receiver HTTP statuses](troubleshooting.md#receiver-http-statuses). |
| No spoke data | Examine the collector log in the spoke and the gateway log on the hub (`kubectl --context kind-querdian-hub -n observability logs deploy/ingest-gateway`). A TLS handshake error there shows a wrong cert or CA. |
| The hub node has a new address, for example after a restart of Docker | The address of the hub node is in the server certs. Run `down.sh`, then `up.sh` again. |
