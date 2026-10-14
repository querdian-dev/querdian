#!/bin/sh
# Demo workload (busybox, tiny footprint): serves Prometheus metrics on :8080/metrics.txt,
# logs to stdout (collected by the OTel Collector) and sends one OTLP/JSON trace every 3 s.
#
# SPOOF_NAMESPACE (order-service only): the workload tries to claim another namespace in its
# metric labels and in its span resource attributes. The collection pipeline (Prometheus
# relabeling, OTel Collector) must ignore that; hack/quickstart/verify.sh asserts it does.
SERVICE="${SERVICE_NAME:?SERVICE_NAME required}"
OTLP="${OTLP_TRACES_URL:-http://otel-collector.observability:4318/v1/traces}"
mkdir -p /www
httpd -p 8080 -h /www

hex() { head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }

n1=0 n2=0 n3=0 i=0
while true; do
  i=$((i + 1))
  c=$((i % 3 + 1))
  eval "n$c=\$((n$c + 1))"

  {
    echo "# TYPE demo_requests_total counter"
    echo "demo_requests_total{customer_id=\"c-1\"} $n1"
    echo "demo_requests_total{customer_id=\"c-2\"} $n2"
    echo "demo_requests_total{customer_id=\"c-3\"} $n3"
    if [ -n "${SPOOF_NAMESPACE:-}" ]; then
      echo "# TYPE demo_spoof_attempt gauge"
      echo "demo_spoof_attempt{namespace=\"$SPOOF_NAMESPACE\"} 1"
    fi
  } >/www/metrics.txt.tmp
  mv /www/metrics.txt.tmp /www/metrics.txt

  echo "level=info service=$SERVICE msg=\"request handled\" customer_id=c-$c seq=$i"
  if [ $((i % 5)) -eq 0 ]; then
    echo "level=error service=$SERVICE msg=\"upstream timeout\" customer_id=c-$c seq=$i"
  fi

  now=$(date +%s)
  start="${now}000000000"
  mid="${now}010000000"
  end="${now}050000000"
  tid=$(hex 16)
  sid=$(hex 8)
  cid=$(hex 8)
  spoof=""
  if [ -n "${SPOOF_NAMESPACE:-}" ]; then
    spoof=",{\"key\":\"k8s.namespace.name\",\"value\":{\"stringValue\":\"$SPOOF_NAMESPACE\"}}"
  fi
  wget -q -O /dev/null --header 'Content-Type: application/json' --post-data \
    "{\"resourceSpans\":[{\"resource\":{\"attributes\":[{\"key\":\"service.name\",\"value\":{\"stringValue\":\"$SERVICE\"}}$spoof]},\"scopeSpans\":[{\"scope\":{\"name\":\"quickstart\"},\"spans\":[{\"traceId\":\"$tid\",\"spanId\":\"$sid\",\"name\":\"POST /$SERVICE\",\"kind\":2,\"startTimeUnixNano\":\"$start\",\"endTimeUnixNano\":\"$end\",\"attributes\":[{\"key\":\"customer_id\",\"value\":{\"stringValue\":\"c-$c\"}}]},{\"traceId\":\"$tid\",\"spanId\":\"$cid\",\"parentSpanId\":\"$sid\",\"name\":\"db query\",\"kind\":3,\"startTimeUnixNano\":\"$mid\",\"endTimeUnixNano\":\"$end\",\"attributes\":[{\"key\":\"db.system\",\"value\":{\"stringValue\":\"postgresql\"}}]}]}]}]}" \
    "$OTLP" || echo "level=warn service=$SERVICE msg=\"trace export failed\""
  sleep 3
done
