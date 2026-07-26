# Splunk Docs MCP (Self-Hosted)

This project is for self-hosting a Splunk documentation MCP server after crawling and indexing Splunk docs into PostgreSQL + pgvector.

## Pipeline Overview

1. Crawl docs from help.splunk.com into local markdown.
2. Index markdown into PostgreSQL with full-text + vector search.
3. Run the MCP server with authenticated `search_docs` and `get_page` tools.

## Repository Guide

- [crawler](crawler/README.md): Sitemap-driven crawler and crawl-state management.
- [indexer](indexer/README.md): Chunking, embedding, and database ingestion.
- [mcp-server](mcp-server/README.md): MCP HTTP server, auth, and runtime behavior.
- [postgres](postgres/README.md): pgvector installation and database initialization.
- [postgres schema](postgres/schema.sql): SQL schema used by the indexer/server.
- [env template](.env.example): Environment variables for local/dev/prod.

## Quick Start (High Level)

1. Crawl docs: follow [crawler](crawler/README.md).
2. Install pgvector and initialize DB: follow [postgres](postgres/README.md).
3. Index markdown: follow [indexer](indexer/README.md).
4. Start MCP server: follow [mcp-server](mcp-server/README.md).

## CI/CD and Deployment

- GitHub Actions workflow: `.github/workflows/mcp-server-ci-cd.yml`
  - Runs `go test ./...` in `mcp-server`.
  - Builds Linux amd64 binary artifact: `mcp-server-linux-amd64`.
  - Uploads the binary as a workflow artifact.
- Optional EC2 deployment is included in the same workflow and runs only on `push` to `main` when all required secrets are set:
  - `EC2_HOST`
  - `EC2_USER`
  - `EC2_SSH_PRIVATE_KEY`

The deployment job copies the artifact to EC2, installs it to `/opt/splunk-docs-mcp-server/bin/mcp-server`, and restarts `splunk-docs-mcp-server.service`.
