VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
FUZZTIME ?= 30s
ENVTEST_K8S_VERSION ?= 1.33.0
IMAGE ?= ghcr.io/querdian-dev/querdian
GORELEASER_VERSION ?= v2.20.1

.PHONY: build test test-race lint fuzz generate manifests chart-crds chart-crds-check chart-lint licenses envtest e2e e2e-single e2e-multicluster fmt docker-build release-check site site-serve

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/querdian ./cmd/querdian

test:
	go test ./...

test-race:
	go test -race ./...

lint:
	go tool golangci-lint run ./...

# Runs every Fuzz* target found in the module for $(FUZZTIME) each.
fuzz:
	@for pkg in $$(go list ./...); do \
		for t in $$(go test -list 'Fuzz.*' $$pkg | grep '^Fuzz' || true); do \
			echo "fuzz $$pkg $$t"; \
			go test $$pkg -run '^$$' -fuzz "^$$t$$" -fuzztime $(FUZZTIME) || exit 1; \
		done; \
	done

generate:
	go tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./api/...

manifests:
	go tool controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd

# Copies the generated CRDs into the Helm chart (run after `make manifests`).
chart-crds:
	mkdir -p charts/querdian/crds
	rm -f charts/querdian/crds/*.yaml
	cp config/crd/*.yaml charts/querdian/crds/

# Fails if the chart CRDs differ from config/crd (CI check).
chart-crds-check:
	diff -r config/crd charts/querdian/crds

chart-lint: chart-crds-check
	helm lint charts/querdian
	helm lint charts/querdian -f charts/querdian/ci/all-features-values.yaml
	helm lint charts/querdian -f charts/querdian/ci/hub-values.yaml
	helm lint charts/querdian -f charts/querdian/ci/spoke-values.yaml
	go test ./charts/...

# hashicorp/go-uuid (MPL-2.0, file-level copyleft, used unmodified) arrives via prometheus/common >= v0.73.0.
licenses:
	go tool go-licenses check ./... --disallowed_types=forbidden,restricted,reciprocal,unknown --ignore github.com/hashicorp/go-uuid

# Sets KUBEBUILDER_ASSETS; setup-envtest downloads the binaries on the first run.
ENVTEST_ASSETS = KUBEBUILDER_ASSETS="$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) -p path)"

envtest:
	$(ENVTEST_ASSETS) go test -tags envtest ./...

# All end-to-end suites: first the single-cluster suites in parallel, then the multicluster suite alone. The
# multicluster suite measures real time (time limit, tail close), so the load of the other suites must not
# delay it.
e2e: e2e-single
	$(MAKE) e2e-multicluster

# The single-cluster suites against real Prometheus, Loki and Tempo containers (needs docker).
e2e-single:
	pkgs="$$(go list -tags e2e ./test/e2e/... | grep -v '/test/e2e/multicluster$$')" && test -n "$$pkgs" && \
		go test -tags e2e -count=1 -timeout 15m $$pkgs

# The multicluster suite: envtest API servers for the hub and the spokes, and real backend containers.
e2e-multicluster:
	$(ENVTEST_ASSETS) go test -tags e2e -count=1 -timeout 8m -v -run TestMultiCluster ./test/e2e/multicluster/

fmt:
	gofmt -s -w .

# Builds the image from source for the host platform (override with PLATFORM=linux/arm64).
docker-build:
	docker build --build-arg VERSION=$(VERSION) $(if $(PLATFORM),--platform $(PLATFORM)) -t $(IMAGE):$(VERSION) .

# Validates .goreleaser.yaml (module-proxy run, stays out of go.mod).
release-check:
	go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) check

# Landing page and documentation (Hugo, pinned in .github/workflows/pages.yml).
HUGO ?= go run github.com/gohugoio/hugo@v0.168.0
site:
	site/build-content.sh
	$(HUGO) --source site --minify --gc

site-serve:
	site/build-content.sh
	$(HUGO) server --source site
