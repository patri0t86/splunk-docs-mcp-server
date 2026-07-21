-- Run this against any existing database (e.g. 'postgres'), not 'splunkdocs'
-- itself -- you can't create a database while connected to it.
--   psql "postgres://<user>@127.0.0.1:5432/postgres?sslmode=disable" -f schema.sql

SELECT 'CREATE DATABASE splunkdocs'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'splunkdocs')\gexec

\c splunkdocs

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS documents (
    id           SERIAL PRIMARY KEY,
    url          TEXT NOT NULL UNIQUE,
    title        TEXT NOT NULL,
    product      TEXT NOT NULL,
    version      TEXT NOT NULL,
    breadcrumb   TEXT,
    content_hash TEXT NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS documents_product_idx ON documents (product);

-- 768 dims matches nomic-embed-text (via Ollama). Change to match
-- whatever embedding model you use, e.g. 1536 for OpenAI text-embedding-3-small.
--
-- tsv is written by the indexer, not generated: it combines the document
-- title (weight A), chunk heading (B), and content (C), and title lives in
-- the documents table so a generated column can't reference it.
-- heading_level is 2 or 3 (the markdown heading depth) or 0 for the
-- heading-less intro chunk, so get_page can reassemble pages faithfully.
CREATE TABLE IF NOT EXISTS chunks (
    id            SERIAL PRIMARY KEY,
    document_id   INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    chunk_index   INTEGER NOT NULL,
    heading       TEXT,
    heading_level SMALLINT NOT NULL DEFAULT 0,
    content       TEXT NOT NULL,
    embedding     vector(768),
    tsv           tsvector,
    UNIQUE (document_id, chunk_index)
);

-- Approximate nearest-neighbor index for semantic search.
CREATE INDEX IF NOT EXISTS chunks_embedding_idx ON chunks USING hnsw (embedding vector_cosine_ops);

-- Full-text keyword index.
CREATE INDEX IF NOT EXISTS chunks_tsv_idx ON chunks USING GIN (tsv);
