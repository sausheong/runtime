#!/usr/bin/env bash
# Measures cosine similarity from the bundled embedder for a related pair and an
# unrelated pair, and prints a suggested RUNTIME_EMBED_RECALL_FLOOR (midpoint).
# Requires the embedder reachable at $EMB (default http://localhost:8000).
set -euo pipefail
EMB="${EMB:-http://localhost:8000}"

python3 - "$EMB" <<'PY'
import json, sys, math, urllib.request
EMB = sys.argv[1]
def emb(t):
    req = urllib.request.Request(EMB + "/embeddings",
        data=json.dumps({"model":"x","input":t}).encode(),
        headers={"content-type":"application/json"})
    return json.load(urllib.request.urlopen(req))["data"][0]["embedding"]
def cos(u,v):
    d=sum(a*b for a,b in zip(u,v)); nu=math.sqrt(sum(a*a for a in u)); nv=math.sqrt(sum(b*b for b in v))
    return d/(nu*nv)
schema = emb("the database schema uses an append-only event log")
query  = emb("tell me about the database design")
unrel  = emb("the user prefers dark mode in the UI")
rel = cos(schema, query); un = cos(schema, unrel)
print(f"related={rel:.4f} unrelated={un:.4f} suggested_floor={ (rel+un)/2 :.2f}")
assert rel > un, "related pair must outscore unrelated pair"
PY
