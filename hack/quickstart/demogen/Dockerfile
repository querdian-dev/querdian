# Demo data generator image (quickstart only). up.sh assembles a small build context (go.mod,
# this Dockerfile and the demogen sources) and builds it, so nothing but docker is needed on the
# host. The final stage is the pinned Prometheus image, because the same image runs `promtool`
# to turn the generated OpenMetrics file into TSDB blocks.
FROM golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS build
WORKDIR /src
COPY go.mod ./
COPY demogen ./hack/quickstart/demogen
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/demogen ./hack/quickstart/demogen

FROM docker.io/prom/prometheus:v3.5.0
COPY --from=build /out/demogen /bin/demogen
