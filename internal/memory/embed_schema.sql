CREATE EXTENSION IF NOT EXISTS vector;
ALTER TABLE memory_events ADD COLUMN IF NOT EXISTS embedding vector(%d);
CREATE INDEX IF NOT EXISTS memory_events_embedding_idx
    ON memory_events USING hnsw (embedding vector_cosine_ops);
