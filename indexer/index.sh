#!/usr/bin/env bash
set -euo pipefail

export OLLAMA_URL="${OLLAMA_URL:-http://127.0.0.1:11434}"

if [[ -z "${DATABASE_URL:-}" ]]; then
	echo "DATABASE_URL is required (example: postgres://user:password@127.0.0.1:5432/splunkdocs)" >&2
	exit 1
fi

go run . -prune -embed-workers 4 -batch-size 128 ../data/markdown
