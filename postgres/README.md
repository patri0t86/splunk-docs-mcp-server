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
