#!/usr/bin/env python3
"""Run each line of a cases file as a TraceQL search; print a compact summary.
usage: tempo_run.py cases.txt [--raw]   (env TEMPO_URL, default http://localhost:13200)
"""
import json, os, sys, urllib.error, urllib.parse, urllib.request

base = os.environ.get("TEMPO_URL", "http://localhost:13200")
raw = "--raw" in sys.argv
for q in open(sys.argv[1]).read().splitlines():
    if not q.strip():
        continue
    url = base + "/api/search?" + urllib.parse.urlencode({"q": q, "limit": 20})
    try:
        body = urllib.request.urlopen(url).read().decode()
        d = json.loads(body)
        if raw:
            print(q, "\n", json.dumps(d, indent=1)[:3000])
            continue
        out = []
        for t in d.get("traces", []):
            sets = t.get("spanSets") or ([t["spanSet"]] if t.get("spanSet") else [])
            n = [(s.get("matched"), sorted(sp.get("name", sp["spanID"][-1:]) for sp in s.get("spans", []))) for s in sets]
            out.append("%s root=%s/%s dur=%s sets=%s" % (t["traceID"][-2:], t.get("rootServiceName"), t.get("rootTraceName"), t.get("durationMs"), n))
        print(q, "\n   => OK", out or "no traces")
    except urllib.error.HTTPError as e:
        print(q, "\n   => HTTP", e.code, e.read().decode()[:200].replace("\n", " "))
