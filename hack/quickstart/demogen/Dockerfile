# Demo data generator image (quickstart only). up.sh assembles a small build context (go.mod,
# this Dockerfile and the demogen sources) and builds it, so nothing but docker is needed on the
# host. The final stage is the pinned Prometheus image, because the same image runs `promtool`
# to turn the generated OpenMetrics file into TSDB blocks.
FROM golang:1.27.4@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666 AS build
WORKDIR /src
COPY go.mod ./
COPY demogen ./hack/quickstart/demogen
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/demogen ./hack/quickstart/demogen

FROM docker.io/prom/prometheus:v3.16.0
COPY --from=build /out/demogen /bin/demogen
