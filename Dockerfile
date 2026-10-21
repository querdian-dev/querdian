# syntax=docker/dockerfile:1

# Two ways to get the binary into the image:
#   BINARY_SOURCE=build    (default) compile from source; used by `make docker-build`.
#   BINARY_SOURCE=release  copy the binary GoReleaser already built for
#                          $TARGETPLATFORM (linux/amd64/querdian, ...); used by the
#                          release pipeline so the image ships the exact released binary.
ARG BINARY_SOURCE=build

FROM --platform=$BUILDPLATFORM golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/querdian ./cmd/querdian

FROM scratch AS binary-build
COPY --from=builder /out/querdian /querdian

FROM scratch AS binary-release
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/querdian /querdian

FROM binary-${BINARY_SOURCE} AS binary

# distroless static (Debian 13), nonroot variant, pinned by index digest (multi-arch).
# No shell, no package manager, CA certificates and tzdata included.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=binary --chmod=0555 /querdian /usr/local/bin/querdian
# 65532 is the distroless "nonroot" uid/gid; numeric so Kubernetes can verify runAsNonRoot.
USER 65532:65532
EXPOSE 8080 9090
# The binary only writes to stdout/stderr; compatible with readOnlyRootFilesystem.
ENTRYPOINT ["/usr/local/bin/querdian"]
