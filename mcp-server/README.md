# MCP Server

This service exposes the indexed Splunk docs corpus (help.splunk.com, Splunk
Lantern, dev.splunk.com, and Splunk UI Design System docs) through MCP tools
over HTTP.

Tools:
- `search_docs`: ranked section retrieval, returning stable `section_id`s,
  citation URLs and match provenance
- `get_section`: one section by `section_id`, optionally with neighbours
- `get_page`: full-page reconstruction by URL
- `list_docsets`: indexed products, versions, manuals and page counts
- `compare_versions`: what changed between two versions of the same page

Every tool returns both a rendered text block and a `structuredContent`
payload, so a client can either read the text or consume the fields directly.

## Retrieval

`search_docs` fuses four independent retrieval legs by reciprocal rank, so no
leg's score scale can dominate another's:

| Leg | Index | Finds |
| --- | --- | --- |
| `keyword` | `chunks.tsv` (english) | the query as written |
| `partial` | `chunks.tsv` (english) | any subset of the query terms |
| `identifier` | `chunks.tsv_simple` (simple) | literal tokens the english stemmer destroys: `props.conf`, `_time`, `TRANSFORMS-null`, `/services/search/jobs` |
| `semantic` | `chunks.embedding` (HNSW cosine) | paraphrases of the question |

`mode` picks the weighting:

- `auto` (default) enables the identifier leg only when the query actually
  contains an identifier or SPL command name.
- `exact` promotes the identifier leg and demotes the semantic one.
- `conceptual` turns the identifier leg off entirely.

Results are then deduplicated. The corpus holds a near-identical copy of most
pages for every release, and hundreds of unrelated pages share a title
("Troubleshooting", "Prerequisites"), so collapsing keys on
`(canonical_id, section)` rather than on the title: version copies of one page
collapse to the newest, unrelated same-titled pages do not suppress each other.
Set `latest_only: false` to see every version. At most two sections of any one
page are returned, so a single well-matching page cannot fill the result set.

Filters (`products`, `versions`, `manuals`) accept lists; call `list_docsets`
to discover valid values. The singular `product` and `version` arguments are
still accepted and are merged with the list forms.

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
- `MCP_ALLOWED_ORIGINS` (comma-separated browser origins allowed to call `/mcp`; unset rejects requests carrying `Origin`)

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

- `/mcp`: Dual-era Streamable HTTP endpoint (Bearer auth required): MCP 2026-07-28 clients use stateless POST requests; legacy clients through 2025-11-25 retain the SDK's `initialize` session flow.
- `GET /healthz`: Liveness endpoint (no auth)

Both eras advertise the same five tools with the same descriptions, which a
test enforces, so a client's capabilities don't depend on the protocol version
it negotiated.

## Citing results

`search_docs` returns a `citation_url` per result: the page URL with the
section's anchor appended. Quote from `get_section` rather than `get_page`
where possible — it is bounded, and its `section_id` pins the exact text that
was read. `section_id` is derived from the page URL and its heading, so it
survives a reindex and stays valid between sessions.
