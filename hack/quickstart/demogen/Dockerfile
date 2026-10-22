# Demo data generator image (quickstart only). up.sh assembles a small build context (go.mod,
# this Dockerfile and the demogen sources) and builds it, so nothing but docker is needed on the
# host. The final stage is the pinned Prometheus image, because the same image runs `promtool`
# to turn the generated OpenMetrics file into TSDB blocks.
FROM golang:1.27.3@sha256:f20d83a83e18dbd2a4714b8ffd4b30b287b6243962dd75608e86abcc63ba4efc AS build
WORKDIR /src
COPY go.mod ./
COPY demogen ./hack/quickstart/demogen
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/demogen ./hack/quickstart/demogen

FROM docker.io/prom/prometheus:v3.5.0
COPY --from=build /out/demogen /bin/demogen
