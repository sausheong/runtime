from fastapi.testclient import TestClient
from app import app

client = TestClient(app)

def test_healthz():
    r = client.get("/healthz")
    assert r.status_code == 200
    assert r.json()["status"] == "ok"

def test_embeddings_shape_and_dim():
    r = client.post("/embeddings", json={"model": "x", "input": "hello world"})
    assert r.status_code == 200
    body = r.json()
    assert "data" in body and len(body["data"]) == 1
    vec = body["data"][0]["embedding"]
    assert isinstance(vec, list)
    assert len(vec) == 384
    assert all(isinstance(f, float) for f in vec[:5])

def test_embeddings_deterministic():
    payload = {"model": "x", "input": "the quick brown fox"}
    a = client.post("/embeddings", json=payload).json()["data"][0]["embedding"]
    b = client.post("/embeddings", json=payload).json()["data"][0]["embedding"]
    assert a == b

def test_empty_input_rejected():
    r = client.post("/embeddings", json={"model": "x", "input": ""})
    assert r.status_code == 422

def test_embeddings_distinguishes_related_from_unrelated():
    def emb(t):
        return client.post("/embeddings", json={"model": "x", "input": t}).json()["data"][0]["embedding"]
    def cos(u, v):
        import math
        dot = sum(x*y for x, y in zip(u, v))
        nu = math.sqrt(sum(x*x for x in u)); nv = math.sqrt(sum(y*y for y in v))
        return dot/(nu*nv)
    schema = emb("the database schema uses an append-only event log")
    query = emb("tell me about the database design")
    unrelated = emb("the user prefers dark mode in the UI")
    assert cos(schema, query) > cos(schema, unrelated)
