"""Crawl orchestration: sitemap -> filter -> fetch -> extract -> markdown files."""

from __future__ import annotations

import asyncio
import logging
import os
import signal
from concurrent.futures import ProcessPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path
from urllib.parse import urlsplit

from .config import VERSION_RE, CrawlerConfig, ProductConfig
from .convert import content_hash, render_document, to_markdown
from .extract import ExtractError, PageContent, extract
from .fetcher import Fetcher
from .sitemap import SitemapEntry, parse_sitemap, select_entries
from .state import CrawlState

log = logging.getLogger(__name__)


def _parse_and_convert(html: str, url: str) -> tuple[PageContent, str]:
    """Runs in a pool process: parsing+conversion is CPU-bound and would
    otherwise block the event loop, starving the fetch workers."""
    if urlsplit(url).hostname == "dev.splunk.com":
        from .extract_dev_splunk import extract_dev_splunk
        return extract_dev_splunk(html, url)
    page = extract(html, url)
    return page, to_markdown(page.content_html)


def _init_pool_worker() -> None:
    # Ctrl+C is handled by the parent for graceful shutdown; pool workers
    # must not die mid-parse from the terminal's process-group SIGINT.
    signal.signal(signal.SIGINT, signal.SIG_IGN)


@dataclass
class CrawlStats:
    selected: int = 0
    skipped_fresh: int = 0
    fetched: int = 0
    written: int = 0
    errors: list[str] = field(default_factory=list)


def output_path(base: Path, entry: SitemapEntry, product: ProductConfig) -> Path:
    """data/<product>/<version>/<url path minus prefix and version segments>.md

    Patch-page URLs carry two version markers (".../admin-manual/10.4/
    configuration-file-reference/10.4.1-configuration-file-reference/..."), so
    every pure-version segment is stripped, not just entry.version — otherwise
    the minor version would linger as a redundant mid-path directory.
    """
    path = urlsplit(entry.url).path
    rest = path[len(product.path_prefix):].strip("/")
    segments = [s for s in rest.split("/") if s and not VERSION_RE.match(s)]
    if not segments:
        segments = ["index"]
    version_dir = entry.version or "_unversioned"
    return base / product.name / version_dir / ("/".join(segments) + ".md")


async def load_sitemap_entries(fetcher: Fetcher, product: ProductConfig) -> list[SitemapEntry]:
    """Fetch the product sitemap, following one level of sitemap index if needed."""
    response = await fetcher.get(product.sitemap)
    response.raise_for_status()
    kind, raw_entries = parse_sitemap(response.content)
    if kind == "index":
        raw_entries_all = []
        for child_url, _ in raw_entries:
            child = await fetcher.get(child_url)
            child.raise_for_status()
            child_kind, child_entries = parse_sitemap(child.content)
            if child_kind == "urlset":
                raw_entries_all.extend(child_entries)
        raw_entries = raw_entries_all
    return select_entries(raw_entries, product)


async def crawl_product(
    config: CrawlerConfig,
    product: ProductConfig,
    fetcher: Fetcher,
    state: CrawlState,
    *,
    limit: int | None = None,
    force: bool = False,
    stop_event: asyncio.Event | None = None,
) -> CrawlStats:
    stats = CrawlStats()

    if not await fetcher.allowed_by_robots(product.sitemap):
        raise RuntimeError(f"robots.txt disallows {product.sitemap}")

    entries = await load_sitemap_entries(fetcher, product)
    stats.selected = len(entries)
    log.info("%s: %d pages in scope", product.name, len(entries))

    pending: list[SitemapEntry] = []
    for entry in entries:
        if not force and not state.needs_fetch(entry.url, entry.lastmod):
            stats.skipped_fresh += 1
        else:
            pending.append(entry)
    if limit is not None:
        pending = pending[:limit]
    log.info("%s: %d to fetch, %d already fresh", product.name, len(pending), stats.skipped_fresh)

    queue: asyncio.Queue[SitemapEntry] = asyncio.Queue()
    for entry in pending:
        queue.put_nowait(entry)

    async def worker() -> None:
        while not (stop_event is not None and stop_event.is_set()):
            try:
                entry = queue.get_nowait()
            except asyncio.QueueEmpty:
                return
            try:
                await process_page(entry)
            except Exception as exc:  # keep the crawl going on per-page failures
                stats.errors.append(f"{entry.url}: {exc}")
                log.error("%s: %s", entry.url, exc)
            finally:
                queue.task_done()
                done = stats.fetched + len(stats.errors)
                if done and done % 50 == 0:
                    log.info("%s: %d/%d fetched", product.name, done, len(pending))

    async def process_page(entry: SitemapEntry) -> None:
        if not await fetcher.allowed_by_robots(entry.url):
            log.warning("robots.txt disallows %s; skipping", entry.url)
            return
        response = await fetcher.get(entry.url)
        stats.fetched += 1
        if response.status_code != 200:
            state.record(
                entry.url, product=entry.product, version=entry.version,
                lastmod=entry.lastmod, content_hash=None, file_path=None,
                http_status=response.status_code,
            )
            stats.errors.append(f"{entry.url}: HTTP {response.status_code}")
            return

        try:
            page, markdown = await loop.run_in_executor(
                pool, _parse_and_convert, response.text, entry.url
            )
        except ExtractError as exc:
            state.record(
                entry.url, product=entry.product, version=entry.version,
                lastmod=entry.lastmod, content_hash=None, file_path=None,
                http_status=200,
            )
            stats.errors.append(str(exc))
            return

        document = render_document(entry, page, markdown)
        path = output_path(config.output_dir, entry, product)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(document)
        stats.written += 1
        state.record(
            entry.url, product=entry.product, version=entry.version,
            lastmod=entry.lastmod, content_hash=content_hash(markdown),
            file_path=str(path.relative_to(config.output_dir)), http_status=200,
        )

    loop = asyncio.get_running_loop()
    with ProcessPoolExecutor(
        max_workers=min(4, os.cpu_count() or 1), initializer=_init_pool_worker
    ) as pool:
        workers = [asyncio.create_task(worker()) for _ in range(config.concurrency)]
        await asyncio.gather(*workers)
    return stats
