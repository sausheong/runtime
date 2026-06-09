-- Semantic-recall schema, applied by NewStore only when an embedder is configured
-- (the dimension placeholder below is templated from RUNTIME_EMBED_DIM).
--
-- PREREQUISITE: the pgvector `vector` extension must already exist in this
-- database. The unprivileged runtime role cannot CREATE EXTENSION (it requires a
-- superuser), so an operator must run `CREATE EXTENSION IF NOT EXISTS vector;`
-- once per database (the pgvector/pgvector image ships the extension). If it is
-- absent, the ALTER below fails loudly at startup — the intended hard-fail.
ALTER TABLE memory_events ADD COLUMN IF NOT EXISTS embedding vector(%d);
CREATE INDEX IF NOT EXISTS memory_events_embedding_idx
    ON memory_events USING hnsw (embedding vector_cosine_ops);
