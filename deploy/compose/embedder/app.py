"""Bundled local embedder for runtime's turnkey compose.

Exposes an OpenAI-compatible POST /embeddings backed by a CPU ONNX model
(BAAI/bge-small-en-v1.5, dim 384). The model is baked into the image at build
time, so the running container needs no network — true air-gap after
`docker compose build`. The `model` field in the request is accepted and
ignored (a single model is bundled).
"""
from fastapi import FastAPI
from pydantic import BaseModel
from fastembed import TextEmbedding

MODEL_NAME = "BAAI/bge-small-en-v1.5"
DIM = 384

app = FastAPI(title="runtime-embedder", version="1.0.0")

# Constructed once at import; the model files are already on disk (baked at
# build), so this does not hit the network.
_model = TextEmbedding(model_name=MODEL_NAME)


class EmbedRequest(BaseModel):
    model: str | None = None
    input: str


@app.get("/healthz")
def healthz():
    return {"status": "ok"}


@app.post("/embeddings")
def embeddings(req: EmbedRequest):
    # fastembed returns a generator of numpy arrays; take the first.
    vec = next(_model.embed([req.input]))
    return {"data": [{"embedding": [float(x) for x in vec.tolist()]}]}
