# Demo data generator image (quickstart only). up.sh assembles a small build context (go.mod,
# this Dockerfile and the demogen sources) and builds it, so nothing but docker is needed on the
# host. The final stage is the pinned Prometheus image, because the same image runs `promtool`
# to turn the generated OpenMetrics file into TSDB blocks.
FROM golang:1.27.3@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e AS build
WORKDIR /src
COPY go.mod ./
COPY demogen ./hack/quickstart/demogen
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/demogen ./hack/quickstart/demogen

FROM docker.io/prom/prometheus:v3.15.1
COPY --from=build /out/demogen /bin/demogen
