VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
FUZZTIME ?= 30s
ENVTEST_K8S_VERSION ?= 1.33.0

.PHONY: build test test-race lint fuzz generate manifests licenses envtest e2e fmt

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

licenses:
	go tool go-licenses check ./... --disallowed_types=forbidden,restricted,reciprocal,unknown

envtest:
	KUBEBUILDER_ASSETS="$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) -p path)" go test -tags envtest ./internal/controller/... ./api/...

e2e:
	go test -tags e2e -timeout 20m ./test/e2e/...

fmt:
	gofmt -s -w .
