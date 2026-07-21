# Splunk Docs Retrieval Pipeline

This repository builds a searchable retrieval stack for Splunk documentation:

1. Crawl selected product/version docs from help.splunk.com into markdown.
2. Chunk and embed that corpus into PostgreSQL + pgvector.
3. Serve it through an authenticated MCP endpoint for AI clients.

## Repository layout

- `crawler/`: sitemap-driven crawler (Python, incremental by sitemap lastmod)
- `indexer/`: markdown chunking + embeddings + PostgreSQL ingestion (Go)
- `mcp-server/`: authenticated MCP server exposing `search_docs` and `get_page` (Go)
- `postgres/schema.sql`: database schema for documents/chunks/vector + full-text search
- `data/`: local crawl output (ignored by git except sentinel files)

## What it does

- Crawls only URLs present in product sitemaps (no broad link crawling)
- Writes normalized markdown with structured frontmatter
- Performs hybrid retrieval:
  - full-text ranking (`tsvector` / `ts_rank`)
  - semantic vector search (`pgvector` cosine distance)
- Merges keyword + semantic rankings with reciprocal rank fusion
- De-duplicates near-identical pages by product/title while preferring newest versions

## Prerequisites

- Python 3.12+
- uv
- Go 1.25+
- PostgreSQL 15+ with pgvector
- Ollama with an embedding model (default: `nomic-embed-text`)

## Environment variables

See `.env.example` for all variables.

Required in normal usage:

- `DATABASE_URL`
- `MCP_AUTH_TOKEN` (for MCP server)

Common optional overrides:

- `OLLAMA_URL` (default `http://127.0.0.1:11434`)
- `EMBEDDING_MODEL` (default `nomic-embed-text`)
- `LISTEN_ADDR` (default `:8080`)

## Quick start

### 1) Crawl documentation

```bash
cd crawler
uv sync
uv run splunk-docs-crawler plan
uv run splunk-docs-crawler crawl
uv run splunk-docs-crawler prune --apply
```

### 2) Initialize database

Build and install pgvector from source, then initialize the DB schema:

```bash
# Ensure PostgreSQL server development headers/tooling are installed first.
cd postgres/pgvector
make
make install
```

Then create the database/schema:

```bash
cd postgres
psql -h <host> -U <user> -f schema.sql
```

### 3) Index markdown into PostgreSQL

```bash
export DATABASE_URL='postgres://<user>:<password>@<host>:5432/splunkdocs'
export OLLAMA_URL='http://127.0.0.1:11434'

cd indexer
go run . -prune ../data/markdown
```

Notes:

- Indexing is incremental: unchanged files are skipped by content hash.
- By default, superseded patch-version directories are skipped to reduce duplicate chunks.
- Use `-all-versions` to keep every patch version.

### 4) Run MCP server

```bash
export DATABASE_URL='postgres://<user>:<password>@<host>:5432/splunkdocs'
export MCP_AUTH_TOKEN="$(openssl rand -hex 32)"

cd mcp-server
go run .
```

Server endpoint:

- `POST /mcp` (streamable HTTP, bearer auth required)
- `GET /healthz` (unauthenticated liveness check)

## Security and production notes

- Keep secrets in a runtime secret manager or deployment environment, not in files.
- Rotate `MCP_AUTH_TOKEN` regularly and enforce TLS in front of the server.
- Restrict database network access to trusted services only.
