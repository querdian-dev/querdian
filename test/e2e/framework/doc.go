// Package framework is the reusable harness of the Querdian end-to-end suites (Prometheus, Loki, Tempo).
//
// A suite starts an upstream container (RunContainer), starts the proxy in-process on a random port
// (StartProxy) with a static policy snapshot compiled from CRD objects (no Kubernetes), and issues
// requests as a user with groups (Proxy.As). Tokens come from a minimal in-process OIDC issuer (Issuer).
//
// Containers are driven through the container CLI ("docker", which is a Podman shim on development
// machines; override with QUERDIAN_E2E_CLI). testcontainers is deliberately not used: the CLI works
// identically on Docker and rootless Podman without a Ryuk reaper, and every container is removed in
// t.Cleanup.
package framework
