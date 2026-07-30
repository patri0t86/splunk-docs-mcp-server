# PostgreSQL Setup

This folder contains database bootstrap assets for the Splunk docs retrieval stack.

- `schema.sql`: creates the `splunkdocs` database (if missing), enables pgvector, and creates required tables/indexes.
- `pgvector/`: vendored pgvector source.

## Install pgvector (from vendored source)

Install PostgreSQL server development headers/tools for your platform first, then:

```bash
cd postgres/pgvector
make
make install
```

## Initialize database schema

```bash
cd postgres
psql -h <host> -U <user> -f schema.sql
```

## Migrating an existing database

`schema.sql` is idempotent and safe to re-run: new columns are added with
`ADD COLUMN IF NOT EXISTS`, so applying it to a populated database preserves
the indexed content and embeddings.

Re-run it, then re-run the indexer. The indexer detects rows whose derived
metadata is missing and backfills them in place without re-embedding, so the
migration costs a pass over the markdown tree rather than a full reindex.

## Schema notes

- `documents.canonical_id` is the version-independent identity of a page, used
  to collapse the near-identical copy of each page held for every release.
- `document_aliases` maps normalized lookup keys to documents, so the server
  can resolve a cross-reference URL with an exact index lookup instead of a
  substring scan over `documents.url`, which could match the wrong page.
- `chunks.section_id` is a reindex-stable handle used for citations.
- `chunks.tsv_simple` indexes the chunk with the `simple` configuration, which
  preserves punctuation-bearing identifiers (`props.conf`, `_time`) that the
  english configuration in `chunks.tsv` stems away.
