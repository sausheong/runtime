# Bundled local embedder for runtime's turnkey compose.
# OpenAI-compatible POST /embeddings backed by BAAI/bge-small-en-v1.5 (dim 384),
# baked into the image at build time so the running container needs no network.
FROM python:3.12-slim

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py .

# Warm the model into the image layer (downloads ONNX weights at BUILD time).
# After this, the running container never needs network for embeddings.
RUN python -c "from fastembed import TextEmbedding; TextEmbedding(model_name='BAAI/bge-small-en-v1.5')"

EXPOSE 8000
# uvicorn serves app:app on 0.0.0.0:8000; reachable as http://embedder:8000 on
# the compose network.
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8000"]
