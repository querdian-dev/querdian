// Command querdian is a multi-tenant access proxy for Prometheus, Loki and Tempo.
package main

import (
	"fmt"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	fmt.Println("querdian", version)
}
