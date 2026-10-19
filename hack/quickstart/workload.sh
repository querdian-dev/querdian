#!/bin/sh
# Demo workload (busybox, tiny footprint). Simulates a small web service and continues the
# backfilled history of hack/quickstart/demogen at the "now" end:
#  * serves Prometheus metrics on :8080/metrics.txt (http_requests_total, the duration histogram,
#    saturation gauges and the demo_requests_total{customer_id} series of the denied-label example),
#  * logs structured lines to stdout (JSON for order-service, logfmt otherwise); they are collected
#    by the OTel Collector. Errors, slow requests and a fraction of the others carry a trace_id,
#  * sends the matching multi-span traces as OTLP/JSON to the collector.
# The request rates, error ratios and latencies are those of demogen at age 0, so the live data
# joins the history without a step.
#
# SPOOF_NAMESPACE (order-service only): the workload tries to claim another namespace in its
# metric labels and in its span resource attributes. The collection pipeline (Prometheus
# relabeling, OTel Collector) must ignore that; hack/quickstart/verify.sh asserts it does.
SERVICE="${SERVICE_NAME:?SERVICE_NAME required}"
OTLP="${OTLP_TRACES_URL:-http://otel-collector.observability:4318/v1/traces}"
mkdir -p /www
httpd -p 8080 -h /www

exec awk -v SERVICE="$SERVICE" -v OTLP="$OTLP" -v SPOOF="${SPOOF_NAMESPACE:-}" '
function gauss(   u) { u = rand(); if (u < 1e-9) u = 1e-9; return sqrt(-2 * log(u)) * cos(6.283185307 * rand()) }
function hex(n,   s, i) { s = ""; for (i = 0; i < n; i++) s = s substr("0123456789abcdef", int(rand() * 16) + 1, 1); return s }
function ts(ms) { return sprintf("%.0f", ms) "000000" }
function attr(k, v) { return "{\"key\":\"" k "\",\"value\":{\"stringValue\":\"" v "\"}}" }
function iattr(k, v) { return "{\"key\":\"" k "\",\"value\":{\"intValue\":\"" v "\"}}" }

# Route table. children: "name~kind-of-dependency~median ms~attr key~attr value" separated by "|".
function route(i, m, p, ok, rps, nf, kids) { M[i] = m; P[i] = p; OK[i] = ok; RPS[i] = rps; NFD[i] = nf; KIDS[i] = kids; TOTAL += rps }
function setup() {
  if (SERVICE == "order-service") {
    JSON = 1
    route(1, "GET",  "/orders",              200, 1.5,  0, "redis GET orders:recent~cache~1.5~db.system~redis|SELECT orders~db~12~db.system~postgresql")
    route(2, "POST", "/orders",              201, 0.7,  0, "SELECT stock~db~8~db.system~postgresql|POST payment-service /payments~payment~120~peer.service~payment-service|INSERT orders~db~22~db.system~postgresql")
    route(3, "GET",  "/orders/{id}",         200, 1.3,  1, "redis GET order~cache~1.5~db.system~redis|SELECT orders by id~db~4~db.system~postgresql")
    route(4, "POST", "/orders/{id}/cancel",  200, 0.12, 1, "UPDATE orders~db~20~db.system~postgresql|POST payment-service /payments/{id}/refund~payment~150~peer.service~payment-service")
    route(5, "GET",  "/cart",                200, 1.0,  0, "redis GET cart~cache~2~db.system~redis|SELECT cart_items~db~1~db.system~postgresql")
    NR = 5
  } else {
    JSON = 0
    route(1, "POST", "/payments",             201, 0.85, 0, "SELECT payment_methods~db~6~db.system~postgresql|POST acme-pay /v1/charges~provider~160~peer.service~acme-pay|INSERT payments~db~18~db.system~postgresql")
    route(2, "GET",  "/payments/{id}",        200, 0.7,  1, "redis GET payment~cache~1.5~db.system~redis|SELECT payments by id~db~5~db.system~postgresql")
    route(3, "POST", "/payments/{id}/refund", 200, 0.1,  1, "SELECT payments by id~db~10~db.system~postgresql|POST acme-pay /v1/refunds~provider~190~peer.service~acme-pay|UPDATE payments~db~16~db.system~postgresql")
    route(4, "GET",  "/payment-methods",      200, 0.5,  0, "redis GET methods~cache~1.5~db.system~redis|SELECT payment_methods~db~1~db.system~postgresql")
    route(5, "POST", "/webhooks/provider",    202, 0.4,  0, "INSERT webhook_events~db~14~db.system~postgresql|UPDATE payments~db~12~db.system~postgresql")
    NR = 5
  }
  NB = split("0.005 0.01 0.025 0.05 0.1 0.25 0.5 1 2.5 5 10", LE, " ")
  # median latency of a route = 3 ms + the sum of its steps (as in demogen)
  for (i = 1; i <= NR; i++) { n = split(KIDS[i], k, "|"); med = 3; for (j = 1; j <= n; j++) { split(k[j], f, "~"); med += f[3] } MED[i] = med }
}

# One simulated request of route i that ended at wall-clock second NOW.
function request(i,   u, st, lat, d, j, b, n, k, f, kidsum, off, dur, tid, rid, spans, status, errtxt, lvl, msg, line, concrete, fail, c, fr, share) {
  u = rand(); st = OK[i]
  if (u < 0.001) st = 503; else if (u < 0.004) st = 500; else if (u < 0.019) st = (NFD[i] ? 404 : 400)
  lat = MED[i] * exp(0.45 * gauss()); if (st == 400) lat = 4 + 3 * rand()
  CNT[i, st]++; SUMS[i] += lat / 1000; TOT[i]++
  for (b = 1; b <= NB; b++) if (lat / 1000 <= LE[b]) BKT[i, b]++
  c = rand(); CUST[(c < 0.5 ? 1 : (c < 0.8 ? 2 : 3))]++

  if (rand() >= 0.30 / TOTAL) return          # not part of the sampled access log
  dur = lat; if (dur < 3) dur = 3
  tid = ""
  if (st >= 500 || dur >= 1000 || rand() < 0.12) {
    tid = hex(32)
    rid = hex(16); n = split(KIDS[i], k, "|"); kidsum = 0
    for (j = 1; j <= n; j++) { split(k[j], f, "~"); kidsum += f[3] }
    concrete = P[i]; sub(/\{id\}/, substr(SERVICE, 1, 1) "-" sprintf("%05d", int(rand() * 100000)), concrete)
    errtxt = ""
    if (st == 503) errtxt = "timeout after 2s waiting for upstream"
    if (st == 500) errtxt = "pq: could not obtain a connection from the pool"
    endms = NOW * 1000 + int(rand() * 900); startms = endms - dur
    spans = "{\"traceId\":\"" tid "\",\"spanId\":\"" rid "\",\"name\":\"" M[i] " " P[i] "\",\"kind\":2,\"startTimeUnixNano\":\"" ts(startms) "\",\"endTimeUnixNano\":\"" ts(endms) "\",\"attributes\":[" attr("http.request.method", M[i]) "," attr("http.route", P[i]) "," attr("url.path", concrete) "," iattr("http.response.status_code", st) "]"
    if (st >= 500) spans = spans ",\"status\":{\"code\":2,\"message\":\"" st " " errtxt "\"}"
    spans = spans "}"
    if (st == 400) n = 0; else if (st == 404) n = 1
    off = startms + 1
    for (j = 1; j <= n; j++) {
      split(k[j], f, "~")
      share = (dur - 3) * f[3] / kidsum; fail = (st >= 500 && j == n)
      spans = spans ",{\"traceId\":\"" tid "\",\"spanId\":\"" hex(16) "\",\"parentSpanId\":\"" rid "\",\"name\":\"" f[1] "\",\"kind\":3,\"startTimeUnixNano\":\"" ts(off) "\",\"endTimeUnixNano\":\"" ts(off + share) "\",\"attributes\":[" attr(f[4], f[5]) "]"
      if (fail) spans = spans ",\"status\":{\"code\":2,\"message\":\"" errtxt "\"},\"events\":[{\"timeUnixNano\":\"" ts(off + share) "\",\"name\":\"exception\",\"attributes\":[" attr("exception.type", "UpstreamError") "," attr("exception.message", errtxt) "]}]"
      spans = spans "}"
      off += share + 0.3
    }
    body = "{\"resourceSpans\":[{\"resource\":{\"attributes\":[" attr("service.name", SERVICE) SPOOFATTR "]},\"scopeSpans\":[{\"scope\":{\"name\":\"quickstart\"},\"spans\":[" spans "]}]}]}"
    printf "%s", body > "/tmp/trace.json"; close("/tmp/trace.json")
    if (system("wget -q -O /dev/null --header \"Content-Type: application/json\" --post-file /tmp/trace.json " OTLP " 2>/dev/null") != 0)
      print "level=warn service=" SERVICE " msg=\"trace export failed\""
  }
  lvl = "info"; msg = "request completed"
  if (st >= 500) { lvl = "error"; msg = "request failed" } else if (st >= 400) { lvl = "warn"; msg = "request rejected" } else if (dur >= 1000) { lvl = "warn"; msg = "slow request" }
  ms = sprintf("%03d", int(rand() * 1000))
  if (JSON) {
    line = "{\"time\":\"" ISO "." ms "Z\",\"level\":\"" lvl "\",\"service\":\"" SERVICE "\",\"msg\":\"" msg "\",\"method\":\"" M[i] "\",\"route\":\"" P[i] "\",\"status\":" st ",\"duration_ms\":" sprintf("%.1f", dur)
    if (tid != "") line = line ",\"trace_id\":\"" tid "\""
    if (st >= 500) line = line ",\"error\":\"" errtxt "\""
    line = line "}"
  } else {
    line = "time=" ISO "." ms "Z level=" lvl " service=" SERVICE " msg=\"" msg "\" method=" M[i] " route=" P[i] " status=" st " duration_ms=" sprintf("%.1f", dur)
    if (tid != "") line = line " trace_id=" tid
    if (st >= 500) line = line " error=\"" errtxt "\""
  }
  print line
}

function metrics(   i, st, b, lbl, codes, nc, c, f) {
  f = "/www/metrics.txt.tmp"
  printf "# TYPE http_requests_total counter\n" > f
  for (i = 1; i <= NR; i++) {
    nc = split(OK[i] " " (NFD[i] ? 404 : 400) " 500 503", codes, " ")
    for (c = 1; c <= nc; c++) printf "http_requests_total{method=\"%s\",route=\"%s\",status=\"%s\"} %d\n", M[i], P[i], codes[c], CNT[i, codes[c]] > f
  }
  printf "# TYPE http_request_duration_seconds histogram\n" > f
  for (i = 1; i <= NR; i++) {
    lbl = "method=\"" M[i] "\",route=\"" P[i] "\""
    for (b = 1; b <= NB; b++) printf "http_request_duration_seconds_bucket{%s,le=\"%s\"} %d\n", lbl, LE[b], BKT[i, b] > f
    printf "http_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\nhttp_request_duration_seconds_sum{%s} %.3f\nhttp_request_duration_seconds_count{%s} %d\n", lbl, TOT[i], lbl, SUMS[i], lbl, TOT[i] > f
  }
  printf "# TYPE demo_requests_total counter\n" > f
  for (i = 1; i <= 3; i++) printf "demo_requests_total{customer_id=\"c-%d\"} %d\n", i, CUST[i] > f
  CPU += 0.08 * DT
  u = 0.30 + 0.03 * gauss(); d = int(5 + 0.8 * gauss() + 0.5); q = int(1.2 + gauss() + 0.5); if (q < 0) q = 0
  printf "# TYPE demo_worker_pool_utilization gauge\ndemo_worker_pool_utilization %.3f\n# TYPE demo_db_connections_in_use gauge\ndemo_db_connections_in_use %d\n# TYPE demo_db_connections_max gauge\ndemo_db_connections_max 20\n# TYPE demo_queue_depth gauge\ndemo_queue_depth %d\n# TYPE process_resident_memory_bytes gauge\nprocess_resident_memory_bytes %d\n# TYPE process_cpu_seconds_total counter\nprocess_cpu_seconds_total %.2f\n", u, d, q, 12000000 + 1000000 * gauss() + 90000000, CPU > f
  if (SPOOF != "") printf "# TYPE demo_spoof_attempt gauge\ndemo_spoof_attempt{namespace=\"%s\"} 1\n", SPOOF > f
  close(f)
  system("mv /www/metrics.txt.tmp /www/metrics.txt")
}

BEGIN {
  setup(); DT = 2
  SPOOFATTR = (SPOOF != "") ? "," attr("k8s.namespace.name", SPOOF) : ""
  cmd = "date -u +\"%s %Y-%m-%dT%H:%M:%S\""
  cmd | getline line0; close(cmd); split(line0, tt, " "); srand(tt[1])
  # an initial dump so Prometheus has all series at once
  NOW = tt[1]; ISO = tt[2]; metrics()
  if (JSON) printf "{\"time\":\"%s.000Z\",\"level\":\"info\",\"service\":\"%s\",\"msg\":\"service started\",\"version\":\"1.4.2\",\"port\":8080}\n", ISO, SERVICE
  else printf "time=%s.000Z level=info service=%s msg=\"service started\" version=1.4.2 port=8080\n", ISO, SERVICE
  fflush()
  while (1) {
    cmd | getline line0; close(cmd); split(line0, tt, " "); NOW = tt[1]; ISO = tt[2]
    for (i = 1; i <= NR; i++) {
      lam = RPS[i] * DT; n = int(lam + sqrt(lam) * gauss() + 0.5); if (n < 0) n = 0
      for (j = 0; j < n; j++) request(i)
    }
    metrics()
    fflush()
    system("sleep " DT)
  }
}'
