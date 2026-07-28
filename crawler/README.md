# splunk-docs-crawler

Crawler for Splunk docs sources that outputs markdown files (with YAML
frontmatter) ready for chunking and indexing:

- [help.splunk.com](https://help.splunk.com) (sitemap-driven, versioned docs)
- [lantern.splunk.com](https://lantern.splunk.com) (sitemap-driven, unversioned)
- [dev.splunk.com](https://dev.splunk.com) (sitemap-driven, Next.js RSC/MDX extraction)
- [splunkui.splunk.com](https://splunkui.splunk.com) (SPA-rendered via Playwright with a curated route list)

## How it works

- help.splunk.com, lantern.splunk.com, and dev.splunk.com publish sitemaps.
  help.splunk.com publishes one sitemap per product
  (e.g. `/en/splunk-enterprise/sitemap.xml`) with `<lastmod>` per page, so the
  crawler never link-crawls — it fetches exactly the pages in scope.
- Most help.splunk.com docs carry a version path segment
  (`.../overview/10.4/...`), so product/version scoping is pure URL filtering.
  Version comparison is numeric (`10.2 > 9.4`). Lantern/dev/splunk-ui pages are
  unversioned and are written under `_unversioned/`.
- splunkui.splunk.com is a client-side SPA with no sitemap; the crawler renders
  a maintained set of public routes with Playwright and extracts from the
  rendered DOM.
- Re-crawls are incremental: a page is skipped when its sitemap `lastmod`
  matches the stored value and its output file still exists.
- Politeness: robots.txt is honored, requests carry an identifying User-Agent,
  and a global token-interval rate limiter caps requests/second. Transient
  failures (429/5xx/network) retry with exponential backoff and honor
  `Retry-After`.

## Setup

```sh
cd crawler
uv sync
```

The `dev.splunk.com` MDX converter also requires Node.js 18 or later on `PATH`.

### Playwright system dependencies (splunk-ui only)

Crawling `splunkui.splunk.com` uses Playwright to render the SPA. After
`uv sync`, install the browser binary:

```sh
uv run playwright install chromium
```

**Amazon Linux 2023 / RHEL / Fedora (including Graviton aarch64)** — install
system libraries first (Playwright's `install-deps` only covers Debian/Ubuntu):

```sh
sudo dnf install -y atk libX11 libXcomposite libXdamage libXext \
  libXfixes libXrandr mesa-libgbm libxcb libxkbcommon \
  alsa-lib at-spi2-atk nss nspr libdrm cups-libs \
  cairo pango
uv run playwright install chromium
```

**Debian / Ubuntu:**

```sh
uv run playwright install-deps
uv run playwright install chromium
```

If you don't need `splunk-ui`, set `enabled: false` in `config.yaml` for that
product and skip this step.

## Usage

```sh
uv run splunk-docs-crawler plan                  # what's in scope, per version (no page fetches)
uv run splunk-docs-crawler crawl --limit 10      # smoke test
uv run splunk-docs-crawler crawl                 # full incremental crawl
uv run splunk-docs-crawler crawl --force         # ignore lastmod, re-fetch everything
uv run splunk-docs-crawler prune                 # list files no longer in scope (dry run)
uv run splunk-docs-crawler prune --apply         # delete them (run before re-indexing)
uv run splunk-docs-crawler status                # crawl state summary
```

The full corpus (Enterprise >= 9.4, SOAR >= 6.2, ES 8.x, ITSI >= 4.18,
Cloud 10.x trains, plus Lantern/dev/splunk-ui) is large; run `plan` for
current per-version counts. Re-runs only fetch changed pages.

## Configuration (`config.yaml`)

Per product: `sitemap`, `path_prefix`, optional exact `exclude_paths`, and either
`min_version` ("this version and newer") or `versions` (explicit allowlist,
overrides `min_version`).
Enable/disable products with `enabled`. Pages without a version segment are
skipped unless `include_unversioned: true` (used for Lantern/dev/splunk-ui).
Set `spa: true` for SPA-backed products that should bypass sitemap fetching and
use a custom route provider + browser-render pipeline.

Sub-components inside a product tree keep their own version numbers (e.g. 4.x
app manuals under splunk-enterprise); a `min_version` of 9.4 naturally
excludes them.

## Output

```
data/markdown/<product>/<version>/<url-path>.md   # version segment removed from path
data/crawl_state.db                               # SQLite crawl state
```

Each file starts with frontmatter the chunker/indexer needs:

```yaml
---
url: https://help.splunk.com/en/splunk-enterprise/get-started/overview/10.4/...
title: About Splunk Enterprise
product: splunk-enterprise
version: '10.4'
breadcrumbs: [Splunk Enterprise, Get Started, Overview, About Splunk Enterprise, ...]
last_modified: '2026-05-02'
fetched_at: '2026-07-16T18:00:00+00:00'
content_hash: sha256:...
---
```

Body is the article as markdown: ATX headings (`#`/`##`/`###` mirror the DITA
topic nesting, so chunking on h2/h3 boundaries works), GitHub-style tables,
fenced code blocks, absolute links.

## Refresh

Run `crawl` from cron/systemd on whatever cadence you want; `lastmod`
comparison makes quiet runs cheap (one sitemap fetch, no page fetches). The
indexer hashes each file's title+body itself, so re-indexing only touches
changed files. Follow up with `prune --apply` periodically: pages that leave
sitemap scope (aged-out versions, removed pages) otherwise stay on disk and
keep getting indexed; the indexer's `-prune` flag then drops the matching DB
rows.
