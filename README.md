# Splunk Docs MCP (Self-Hosted)

This project is for self-hosting a Splunk documentation MCP server after crawling and indexing Splunk docs (help.splunk.com, Splunk Lantern, dev.splunk.com, and Splunk UI Design System docs) into PostgreSQL + pgvector.

## Pipeline Overview

1. Crawl docs from help.splunk.com, lantern.splunk.com, dev.splunk.com, and splunkui.splunk.com into local markdown.
2. Index markdown into PostgreSQL with full-text + vector search.
3. Run the MCP server, which exposes five authenticated tools over HTTP:
   - `search_docs`: ranked section retrieval with stable ids and citation URLs
   - `get_section`: one section by id, for bounded quotable evidence
   - `get_page`: a complete page by URL
   - `list_docsets`: the indexed products, versions and manuals
   - `compare_versions`: what changed between two versions of a page

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
