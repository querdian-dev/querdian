#!/usr/bin/env python3
"""Push two fixture traces to Tempo via OTLP/HTTP JSON (T0.5 experiments).

Trace 1 (id 0001..): root  frontend "GET /"        1000ms status=ok  attrs a="x", http.status_code=200
                     child backend  "db query"     300ms  status=error kind=client  attrs a="y", http.status_code=500,
                                                         "weird name"="w", "dash-name"="d", resource attr team="t1"
                     child2 backend "cache"         50ms  attrs a="xy"
Trace 2 (id 0002..): root  other    "POST /x"       20ms
"""
import json, sys, time, urllib.request

URL = "http://localhost:14318/v1/traces"
now = int(time.time() * 1e9)
ms = 1_000_000


def attr(k, v):
    if isinstance(v, int):
        return {"key": k, "value": {"intValue": str(v)}}
    return {"key": k, "value": {"stringValue": v}}


def span(tid, sid, parent, name, start_ms, dur_ms, kind=1, status=1, attrs=()):
    s = {"traceId": tid, "spanId": sid, "name": name, "kind": kind,
         "startTimeUnixNano": str(now + start_ms * ms),
         "endTimeUnixNano": str(now + (start_ms + dur_ms) * ms),
         "attributes": [attr(k, v) for k, v in attrs],
         "status": {"code": status}}
    if parent:
        s["parentSpanId"] = parent
    return s


T1 = "00000000000000000000000000000001"
T2 = "00000000000000000000000000000002"
body = {"resourceSpans": [
    {"resource": {"attributes": [attr("service.name", "frontend")]},
     "scopeSpans": [{"scope": {"name": "s1", "version": "1.0"}, "spans": [
         span(T1, "0000000000000001", None, "GET /", 0, 1000, 2, 1, [("a", "x"), ("http.status_code", 200)])]}]},
    {"resource": {"attributes": [attr("service.name", "backend"), attr("team", "t1")]},
     "scopeSpans": [{"scope": {"name": "s2"}, "spans": [
         span(T1, "0000000000000002", "0000000000000001", "db query", 10, 300, 3, 2,
              [("a", "y"), ("http.status_code", 500), ("weird name", "w"), ("dash-name", "d"), ("slash/name", "s")]),
         span(T1, "0000000000000003", "0000000000000001", "cache", 20, 50, 3, 1, [("a", "xy")])]}]},
    {"resource": {"attributes": [attr("service.name", "other")]},
     "scopeSpans": [{"scope": {"name": "s3"}, "spans": [
         span(T2, "0000000000000004", None, "POST /x", 0, 20, 2, 1, [("a", "z")])]}]},
    # Trace 3: root 100ms, child starts at +50ms and lasts 500ms (trace spans 550ms, root only 100ms)
    {"resource": {"attributes": [attr("service.name", "rootsvc")]},
     "scopeSpans": [{"scope": {"name": "s4"}, "spans": [
         span("00000000000000000000000000000003", "0000000000000005", None, "root3", 0, 100, 2, 1, [("a", "r3")])]}]},
    {"resource": {"attributes": [attr("service.name", "childsvc")]},
     "scopeSpans": [{"scope": {"name": "s5"}, "spans": [
         span("00000000000000000000000000000003", "0000000000000006", "0000000000000005", "child3", 50, 500, 3, 1, [("a", "c3")])]}]},
    # Trace 4: no root span at all (only a child whose parent is never sent)
    {"resource": {"attributes": [attr("service.name", "orphansvc")]},
     "scopeSpans": [{"scope": {"name": "s6"}, "spans": [
         span("00000000000000000000000000000004", "0000000000000008", "0000000000000007", "orphan", 5, 40, 3, 1, [("a", "o4")])]}]},
]}
if "--extra" in sys.argv:  # only traces 3 and 4 (already-pushed fixtures are not duplicated)
    body["resourceSpans"] = body["resourceSpans"][3:]
req = urllib.request.Request(URL, json.dumps(body).encode(), {"Content-Type": "application/json"})
print(urllib.request.urlopen(req).status)
