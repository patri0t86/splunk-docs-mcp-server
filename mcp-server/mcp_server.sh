#!/usr/bin/env bash
set -euo pipefail

export OLLAMA_URL="${OLLAMA_URL:-http://127.0.0.1:11434}"
export MCP_AUTH_TOKEN="${MCP_AUTH_TOKEN:-}"
export DATABASE_URL="${DATABASE_URL:-}"

if [[ -z "${MCP_AUTH_TOKEN:-}" ]]; then
	echo "MCP_AUTH_TOKEN is required (recommend generating with: openssl rand -hex 32)" >&2
	exit 1
fi

if [[ -z "${DATABASE_URL:-}" ]]; then
	echo "DATABASE_URL is required (example: postgres://user:password@127.0.0.1:5432/splunkdocs)" >&2
	exit 1
fi

go run .
