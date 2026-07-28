# MCP Server

This service exposes the indexed Splunk docs corpus (including Splunk Lantern)
through MCP tools over HTTP.

Tools:
- `search_docs`: hybrid keyword + semantic retrieval
- `get_page`: full-page reconstruction by URL

## Requirements

- Go 1.25+
- Indexed PostgreSQL database from the indexer
- Reachable Ollama embedding endpoint with same model family used for indexing

## Environment Variables

Required:
- `DATABASE_URL`
- `MCP_AUTH_TOKEN` (must be at least 32 characters)

Optional:
- `OLLAMA_URL` (default `http://localhost:11434`)
- `EMBEDDING_MODEL` (default `nomic-embed-text`)
- `LISTEN_ADDR` (default `:8080`)

## Run

Direct:

```bash
cd mcp-server
export DATABASE_URL='postgres://<user>:<password>@<host>:5432/splunkdocs'
export MCP_AUTH_TOKEN="$(openssl rand -hex 32)"
go run .
```

Using helper script:

```bash
cd mcp-server
./mcp_server.sh
```

## Endpoints

- `POST /mcp`: Streamable MCP HTTP endpoint (Bearer auth required)
- `GET /healthz`: Liveness endpoint (no auth)
