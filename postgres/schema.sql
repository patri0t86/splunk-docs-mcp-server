-- Run this against any existing database (e.g. 'postgres'), not 'splunkdocs'
-- itself -- you can't create a database while connected to it.
--   psql "postgres://<user>@127.0.0.1:5432/postgres?sslmode=disable" -f schema.sql

SELECT 'CREATE DATABASE splunkdocs'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'splunkdocs')\gexec

\c splunkdocs

CREATE EXTENSION IF NOT EXISTS vector;

-- canonical_id is the version-independent identity of a page: product,
-- manual, and normalized page path, with the version segment removed. The
-- corpus holds near-identical copies of every page across versions, and
-- hundreds of pages share a title ("Troubleshooting", "Prerequisites"), so
-- version copies must be collapsed by canonical_id and never by title.
--
-- source_updated_at is the docs site's own last-modified date (frontmatter
-- last_modified), distinct from updated_at, which records ingestion time.
CREATE TABLE IF NOT EXISTS documents (
    id                SERIAL PRIMARY KEY,
    url               TEXT NOT NULL UNIQUE,
    title             TEXT NOT NULL,
    product           TEXT NOT NULL,
    version           TEXT NOT NULL,
    manual            TEXT NOT NULL DEFAULT '',
    canonical_id      TEXT NOT NULL DEFAULT '',
    breadcrumb        TEXT,
    content_hash      TEXT NOT NULL,
    source_updated_at TIMESTAMPTZ,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Columns added after the initial release; harmless to re-run.
ALTER TABLE documents ADD COLUMN IF NOT EXISTS manual TEXT NOT NULL DEFAULT '';
ALTER TABLE documents ADD COLUMN IF NOT EXISTS canonical_id TEXT NOT NULL DEFAULT '';
ALTER TABLE documents ADD COLUMN IF NOT EXISTS source_updated_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS documents_product_idx ON documents (product);
CREATE INDEX IF NOT EXISTS documents_canonical_idx ON documents (canonical_id);
CREATE INDEX IF NOT EXISTS documents_product_version_idx ON documents (product, version);

-- Alias -> document lookup used to resolve cross-reference URLs that are not
-- themselves indexed: help.splunk.com/?resourceId=... redirect links, and
-- URLs that differ from the crawled one only by version or manual edition.
-- An exact lookup on a stored alias replaces substring matching against the
-- url column, which could silently resolve a generic slug to the wrong page.
CREATE TABLE IF NOT EXISTS document_aliases (
    alias       TEXT NOT NULL,
    document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    PRIMARY KEY (alias, document_id)
);

-- No separate index on alias: the primary key's btree on (alias, document_id)
-- already serves lookups by alias, since alias is its leading column.

-- 768 dims matches nomic-embed-text (via Ollama). Change to match
-- whatever embedding model you use, e.g. 1536 for OpenAI text-embedding-3-small.
--
-- tsv is written by the indexer, not generated: it combines the document
-- title (weight A), chunk heading (B), and content (C), and title lives in
-- the documents table so a generated column can't reference it.
-- tsv_simple is the same text under the 'simple' configuration: no stemming
-- and no stopword removal, so punctuation-heavy technical identifiers
-- (props.conf, tstats, REST paths, error codes) can be matched literally
-- instead of being mangled by the English stemmer.
-- heading_level is 2 or 3 (the markdown heading depth) or 0 for the
-- heading-less intro chunk, so get_page can reassemble pages faithfully.
-- section_id is a stable, reindex-surviving handle for one section, so
-- search results can cite a section that get_section can fetch back.
CREATE TABLE IF NOT EXISTS chunks (
    id            SERIAL PRIMARY KEY,
    document_id   INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    chunk_index   INTEGER NOT NULL,
    section_id    TEXT,
    anchor        TEXT,
    heading       TEXT,
    heading_level SMALLINT NOT NULL DEFAULT 0,
    content       TEXT NOT NULL,
    embedding     vector(768),
    tsv           tsvector,
    tsv_simple    tsvector,
    UNIQUE (document_id, chunk_index)
);

-- Columns added after the initial release; harmless to re-run.
ALTER TABLE chunks ADD COLUMN IF NOT EXISTS section_id TEXT;
ALTER TABLE chunks ADD COLUMN IF NOT EXISTS anchor TEXT;
ALTER TABLE chunks ADD COLUMN IF NOT EXISTS tsv_simple tsvector;

-- Approximate nearest-neighbor index for semantic search.
CREATE INDEX IF NOT EXISTS chunks_embedding_idx ON chunks USING hnsw (embedding vector_cosine_ops);

-- Full-text keyword index.
CREATE INDEX IF NOT EXISTS chunks_tsv_idx ON chunks USING GIN (tsv);

-- Literal-identifier keyword index.
CREATE INDEX IF NOT EXISTS chunks_tsv_simple_idx ON chunks USING GIN (tsv_simple);

-- get_section and compare_versions look chunks up by section_id.
CREATE INDEX IF NOT EXISTS chunks_section_id_idx ON chunks (section_id);
