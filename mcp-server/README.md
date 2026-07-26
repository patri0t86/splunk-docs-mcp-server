# MCP Server

This service exposes the indexed Splunk docs corpus through MCP tools over HTTP.

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

- `POST /mcp`: Streamable MCP HTTP endpoint (****** required)
- `GET /healthz`: Liveness endpoint (no auth)

## systemd Service (Linux)

Service unit file is provided at:

- `/home/runner/work/splunk-docs-mcp-server/splunk-docs-mcp-server/deploy/systemd/splunk-docs-mcp-server.service`

Install example:

1. Create runtime directories and user:
   - `sudo useradd --system --home /opt/splunk-docs-mcp-server --shell /usr/sbin/nologin splunkdocs`
   - `sudo mkdir -p /opt/splunk-docs-mcp-server/bin /etc/splunk-docs-mcp-server`
2. Copy the built binary to:
   - `/opt/splunk-docs-mcp-server/bin/mcp-server`
3. Create env file:
   - `/etc/splunk-docs-mcp-server/mcp-server.env`
   - include `DATABASE_URL`, `MCP_AUTH_TOKEN`, and optional `OLLAMA_URL`, `EMBEDDING_MODEL`, `LISTEN_ADDR`
4. Install service file:
   - `sudo cp deploy/systemd/splunk-docs-mcp-server.service /etc/systemd/system/`
5. Enable and start:
   - `sudo systemctl daemon-reload`
   - `sudo systemctl enable --now splunk-docs-mcp-server.service`
