# Bundled local embedder for runtime's turnkey compose.
# OpenAI-compatible POST /embeddings backed by BAAI/bge-small-en-v1.5 (dim 384),
# baked into the image at build time so the running container needs no network.
FROM python:3.12-slim

# Non-root user (matches deploy/ house pattern, e.g. sandbox.Dockerfile).
RUN useradd --uid 1000 --create-home embedder

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py .

# Cache dir owned by the non-root user; warm the model into it at BUILD time so
# the running container needs no network (air-gap) AND can read the weights.
# fastembed 0.5.1 honors FASTEMBED_CACHE_PATH; HF_HOME keeps any HF metadata
# under the same writable, owned tree.
ENV FASTEMBED_CACHE_PATH=/home/embedder/.cache/fastembed \
    HF_HOME=/home/embedder/.cache/hf
RUN mkdir -p /home/embedder/.cache && chown -R embedder:embedder /home/embedder/.cache /app

USER embedder
RUN python -c "from fastembed import TextEmbedding; TextEmbedding(model_name='BAAI/bge-small-en-v1.5')"

EXPOSE 8000
# uvicorn serves app:app on 0.0.0.0:8000; reachable as http://embedder:8000 on
# the compose network.
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000"]
