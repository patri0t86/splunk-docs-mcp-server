"""Command-line interface.

    splunk-docs-crawler plan                 # show what would be crawled, per version
    splunk-docs-crawler crawl                # crawl all enabled products (incremental)
    splunk-docs-crawler crawl --limit 10     # smoke test
    splunk-docs-crawler crawl --force        # re-fetch everything in scope
    splunk-docs-crawler prune                # list output files no longer in scope
    splunk-docs-crawler prune --apply        # delete them
    splunk-docs-crawler status               # crawl state summary
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import os
import signal
import sys
from collections import Counter
from pathlib import Path

from .config import CrawlerConfig, ProductConfig, load_config
from .crawl import crawl_product, load_sitemap_entries, output_path
from .fetcher import Fetcher
from .state import CrawlState

log = logging.getLogger("splunk_docs_crawler")

DEFAULT_CONFIG = Path(__file__).resolve().parents[2] / "config.yaml"


def enabled_products(config: CrawlerConfig, only: str | None) -> list[ProductConfig]:
    if only:
        if only not in config.products:
            sys.exit(f"unknown product {only!r}; configured: {', '.join(config.products)}")
        return [config.products[only]]
    products = [p for p in config.products.values() if p.enabled]
    if not products:
        sys.exit("no products enabled in config")
    return products


async def run_plan(config: CrawlerConfig, only: str | None) -> None:
    async with Fetcher(config.user_agent, config.rate_limit_per_sec, config.timeout_seconds, config.retries) as fetcher:
        for product in enabled_products(config, only):
            entries = await load_sitemap_entries(fetcher, product)
            by_version = Counter(e.version or "(unversioned)" for e in entries)
            print(f"\n{product.name}: {len(entries)} pages in scope")
            for version, count in sorted(by_version.items()):
                print(f"  {version:>14}  {count:>6} pages")


async def run_crawl(config: CrawlerConfig, only: str | None, limit: int | None, force: bool) -> int:
    state = CrawlState(config.state_db, config.output_dir)
    exit_code = 0

    # First Ctrl+C: stop pulling new pages, let in-flight requests finish, then
    # print stats as usual. Second Ctrl+C: exit immediately (state stays
    # consistent — each page commits independently).
    stop_event = asyncio.Event()
    loop = asyncio.get_running_loop()

    def on_sigint() -> None:
        if stop_event.is_set():
            print("\nsecond interrupt, exiting now", file=sys.stderr)
            os._exit(130)
        print("\ninterrupted: finishing in-flight requests (Ctrl+C again to exit now)", file=sys.stderr)
        stop_event.set()

    loop.add_signal_handler(signal.SIGINT, on_sigint)
    try:
        async with Fetcher(config.user_agent, config.rate_limit_per_sec, config.timeout_seconds, config.retries) as fetcher:
            for product in enabled_products(config, only):
                if stop_event.is_set():
                    break
                stats = await crawl_product(
                    config, product, fetcher, state, limit=limit, force=force, stop_event=stop_event
                )
                print(
                    f"\n{product.name}: {stats.selected} in scope, "
                    f"{stats.skipped_fresh} fresh, {stats.fetched} fetched, "
                    f"{stats.written} written, {len(stats.errors)} errors"
                )
                if stats.errors:
                    exit_code = 1
                    for line in stats.errors[:20]:
                        print(f"  ERROR {line}")
                    if len(stats.errors) > 20:
                        print(f"  ... and {len(stats.errors) - 20} more")
    finally:
        loop.remove_signal_handler(signal.SIGINT)
        state.close()
    if stop_event.is_set():
        print("stopped by interrupt; re-run to resume where this left off", file=sys.stderr)
        exit_code = 130
    return exit_code


async def run_prune(config: CrawlerConfig, only: str | None, apply: bool) -> int:
    """Delete output files that are no longer in sitemap scope.

    Pages leave scope when a version ages past min_version, a page is removed
    from the product sitemap, or the config narrows. Only directories of
    enabled products are touched, so disabling a product never deletes its
    files. Dry-run by default; --apply deletes.
    """
    state = CrawlState(config.state_db, config.output_dir)
    stale_total = 0
    try:
        async with Fetcher(config.user_agent, config.rate_limit_per_sec, config.timeout_seconds, config.retries) as fetcher:
            for product in enabled_products(config, only):
                product_dir = config.output_dir / product.name
                if not product_dir.exists():
                    continue
                entries = await load_sitemap_entries(fetcher, product)
                expected = {output_path(config.output_dir, e, product) for e in entries}
                on_disk = sorted(product_dir.rglob("*.md"))
                stale = [p for p in on_disk if p not in expected]
                stale_total += len(stale)
                print(f"{product.name}: {len(stale)} stale of {len(on_disk)} files on disk")
                if not apply:
                    for p in stale[:10]:
                        print(f"  would delete {p.relative_to(config.output_dir)}")
                    if len(stale) > 10:
                        print(f"  ... and {len(stale) - 10} more")
                    continue
                for p in stale:
                    p.unlink()
                forgotten = state.forget_files([str(p.relative_to(config.output_dir)) for p in stale])
                # Prune directories emptied by the deletions, deepest first.
                for d in sorted(product_dir.rglob("*"), reverse=True):
                    if d.is_dir() and not any(d.iterdir()):
                        d.rmdir()
                print(f"{product.name}: deleted {len(stale)} files, forgot {forgotten} state rows")
    finally:
        state.close()
    if stale_total and not apply:
        print("\ndry run: re-run with --apply to delete")
    return 0


def run_status(config: CrawlerConfig) -> None:
    if not config.state_db.exists():
        print("no crawl state yet")
        return
    state = CrawlState(config.state_db, config.output_dir)
    try:
        rows = state.summary()
        if not rows:
            print("no pages recorded yet")
            return
        print(f"{'product':<30} {'version':>10} {'pages':>7} {'ok':>7}  last fetch")
        for row in rows:
            print(
                f"{row['product']:<30} {row['version'] or '-':>10} "
                f"{row['pages']:>7} {row['ok']:>7}  {row['last_fetch']}"
            )
    finally:
        state.close()


def main() -> None:
    parser = argparse.ArgumentParser(prog="splunk-docs-crawler", description=__doc__)
    parser.add_argument("--config", type=Path, default=DEFAULT_CONFIG, help="path to config.yaml")
    parser.add_argument("-v", "--verbose", action="store_true")
    sub = parser.add_subparsers(dest="command", required=True)

    plan = sub.add_parser("plan", help="show pages in scope per product/version without fetching them")
    plan.add_argument("--product", help="limit to one configured product")

    crawl = sub.add_parser("crawl", help="crawl enabled products incrementally")
    crawl.add_argument("--product", help="limit to one configured product")
    crawl.add_argument("--limit", type=int, help="max pages to fetch (smoke testing)")
    crawl.add_argument("--force", action="store_true", help="re-fetch even if lastmod is unchanged")

    prune = sub.add_parser("prune", help="delete output files no longer in sitemap scope (dry-run without --apply)")
    prune.add_argument("--product", help="limit to one configured product")
    prune.add_argument("--apply", action="store_true", help="actually delete (default is a dry run)")

    sub.add_parser("status", help="summarize crawl state")

    args = parser.parse_args()
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(message)s",
        datefmt="%H:%M:%S",
    )
    logging.getLogger("httpx").setLevel(logging.WARNING)

    config = load_config(args.config)
    if args.command == "plan":
        asyncio.run(run_plan(config, args.product))
    elif args.command == "crawl":
        sys.exit(asyncio.run(run_crawl(config, args.product, args.limit, args.force)))
    elif args.command == "prune":
        sys.exit(asyncio.run(run_prune(config, args.product, args.apply)))
    elif args.command == "status":
        run_status(config)


if __name__ == "__main__":
    main()
