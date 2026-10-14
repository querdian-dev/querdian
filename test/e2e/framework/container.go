package framework

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// ContainerSpec describes an upstream container.
type ContainerSpec struct {
	// Image must be pinned to a version tag (never "latest").
	Image string
	// Ports are the container ports to publish on random loopback ports, e.g. "9090".
	Ports []string
	// Args are appended after the image (the container command arguments).
	Args []string
	// Env are KEY=VALUE variables.
	Env []string
	// Mounts are "host:container[:opts]" bind mounts.
	Mounts []string
	// ReadyPath, when set, is polled on the first port (GET, expects 200) before RunContainer returns.
	ReadyPath string
	// ReadyTimeout defaults to 60s.
	ReadyTimeout time.Duration
}

// Container is a running container.
type Container struct {
	// Name is the generated container name.
	Name  string
	cli   string
	ports map[string]string
}

// CLI returns the container CLI binary (QUERDIAN_E2E_CLI or "docker").
func CLI() string {
	if c := os.Getenv("QUERDIAN_E2E_CLI"); c != "" {
		return c
	}
	return "docker"
}

// RequireRuntime fails the test with the exact error when the container runtime is not reachable.
func RequireRuntime(t testing.TB) {
	t.Helper()
	out, err := exec.Command(CLI(), "version", "--format", "{{.Server.Version}}").CombinedOutput() //nolint:gosec // test-only CLI
	if err != nil {
		t.Fatalf("container runtime unavailable (%s version): %v\n%s", CLI(), err, out)
	}
}

// RunContainer starts the container detached, publishes the ports on 127.0.0.1 with random host ports,
// waits for readiness and removes the container (also on failure) in t.Cleanup.
func RunContainer(t testing.TB, spec ContainerSpec) *Container {
	t.Helper()
	RequireRuntime(t)
	var rnd [5]byte
	_, _ = rand.Read(rnd[:])
	c := &Container{Name: "querdian-e2e-" + hex.EncodeToString(rnd[:]), cli: CLI(), ports: map[string]string{}}
	args := []string{"run", "-d", "--name", c.Name, "--label", "querdian-e2e=1"}
	for _, p := range spec.Ports {
		args = append(args, "-p", "127.0.0.1::"+p)
	}
	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	for _, m := range spec.Mounts {
		args = append(args, "-v", m)
	}
	args = append(args, spec.Image)
	args = append(args, spec.Args...)

	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("container %s logs (tail):\n%s", c.Name, tail(c.Logs(), 4000))
		}
		_ = exec.Command(c.cli, "rm", "-f", "-v", c.Name).Run() //nolint:gosec // test-only
	})
	if out, err := exec.Command(c.cli, args...).CombinedOutput(); err != nil { //nolint:gosec // test-only
		t.Fatalf("%s %s: %v\n%s", c.cli, strings.Join(args, " "), err, out)
	}
	for _, p := range spec.Ports {
		out, err := exec.Command(c.cli, "port", c.Name, p+"/tcp").CombinedOutput() //nolint:gosec // test-only
		if err != nil {
			t.Fatalf("port %s: %v\n%s", p, err, out)
		}
		line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		// "127.0.0.1:49153" (IPv6 lines look like "[::]:49153"); keep the port, force loopback.
		i := strings.LastIndex(line, ":")
		if i < 0 {
			t.Fatalf("unexpected port output %q", out)
		}
		c.ports[p] = "127.0.0.1:" + line[i+1:]
	}
	if spec.ReadyPath != "" && len(spec.Ports) > 0 {
		to := spec.ReadyTimeout
		if to == 0 {
			to = 60 * time.Second
		}
		WaitHTTP(t, "http://"+c.ports[spec.Ports[0]]+spec.ReadyPath, to)
	}
	return c
}

// Addr returns the host:port the container port is published on.
func (c *Container) Addr(port string) string { return c.ports[port] }

// URL returns http://host:port for the container port.
func (c *Container) URL(port string) string { return "http://" + c.ports[port] }

// Logs returns the container's combined output.
func (c *Container) Logs() string {
	out, _ := exec.Command(c.cli, "logs", c.Name).CombinedOutput() //nolint:gosec // test-only
	return string(out)
}

// Exec runs a command inside the container and returns its combined output.
func (c *Container) Exec(ctx context.Context, cmd ...string) (string, error) {
	var buf bytes.Buffer
	x := exec.CommandContext(ctx, c.cli, append([]string{"exec", c.Name}, cmd...)...) //nolint:gosec // test-only
	x.Stdout, x.Stderr = &buf, &buf
	err := x.Run()
	return buf.String(), err
}

// WaitHTTP polls url until it answers 200 or the timeout expires (then the test fails).
func WaitHTTP(t testing.TB, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:gosec,noctx // test-only
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			last = fmt.Sprintf("status %d", resp.StatusCode)
		} else {
			last = err.Error()
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s not ready after %s: %s", url, timeout, last)
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
