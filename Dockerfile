# syntax=docker/dockerfile:1

# Two ways to get the binary into the image:
#   BINARY_SOURCE=build    (default) compile from source; used by `make docker-build`.
#   BINARY_SOURCE=release  copy the binary GoReleaser already built for
#                          $TARGETPLATFORM (linux/amd64/querdian, ...); used by the
#                          release pipeline so the image ships the exact released binary.
ARG BINARY_SOURCE=build

FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:08bcb99a38f85a265f9a93d1d3323e2440eec333380f3ea26ee0dec9264bb41c AS builder
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
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
COPY --from=binary --chmod=0555 /querdian /usr/local/bin/querdian
# 65532 is the distroless "nonroot" uid/gid; numeric so Kubernetes can verify runAsNonRoot.
USER 65532:65532
EXPOSE 8080 9090
# The binary only writes to stdout/stderr; compatible with readOnlyRootFilesystem.
ENTRYPOINT ["/usr/local/bin/querdian"]
