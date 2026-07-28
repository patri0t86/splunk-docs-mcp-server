"""Playwright-based crawler for splunkui.splunk.com.

The Splunk Design System site is a pure client-side SPA (single JS bundle, no
server-rendered HTML). All pages are crawled by launching a headless Chromium
browser via Playwright, which executes the JS bundle and produces the fully
rendered DOM for extraction.

Routes are hardcoded from the bundle's React Router config (there is no
sitemap). The page list is stable; bump ROUTES when new pages are added.
"""

from __future__ import annotations

import asyncio
import logging
import re
from datetime import datetime, timezone
from pathlib import Path

from .config import CrawlerConfig, ProductConfig
from .convert import content_hash, render_document, to_markdown
from .crawl import CrawlStats, output_path
from .extract import ExtractError, PageContent
from .sitemap import SitemapEntry
from .state import CrawlState

log = logging.getLogger(__name__)

BASE_URL = "https://splunkui.splunk.com"
CONTENT_SELECTORS = (
    "section[id^='markdown-content-']",
    "#react-docs-layout-main",
    "main",
)
MIN_CONTENT_TEXT_LENGTH = 100
MAX_BROWSER_CONCURRENCY = 4
PAGE_TIMEOUT_MS = 60_000

# Complete list of documentation routes extracted from the JS bundle's
# React Router config (all unversioned, no sitemap available).
ROUTES: list[str] = [
    # Design System
    "/DesignSystem/Overview",
    "/DesignSystem/GettingStarted",
    "/DesignSystem/Foundations",
    "/DesignSystem/DesignPrinciples",
    "/DesignSystem/DesignLevers",
    # Accessibility (public pages only)
    "/DesignSystem/Accessibility/Overview",
    "/DesignSystem/Accessibility/Color",
    "/DesignSystem/Accessibility/DataViz",
    "/DesignSystem/Accessibility/Keyboard",
    "/DesignSystem/Accessibility/NonTextContent",
    # Blueprints
    "/DesignSystem/Blueprints/CRUD/Overview",
    "/DesignSystem/Blueprints/CRUD/Create",
    "/DesignSystem/Blueprints/CRUD/Read",
    "/DesignSystem/Blueprints/CRUD/Update",
    "/DesignSystem/Blueprints/CRUD/Delete",
    # Packages – react-ui components
    "/Packages/react-ui/Overview",
    "/Packages/react-ui/Button",
    "/Packages/react-ui/Card",
    "/Packages/react-ui/Link",
    "/Packages/react-ui/List",
    "/Packages/react-ui/Menu",
    "/Packages/react-ui/Message",
    "/Packages/react-ui/MessageBar",
    "/Packages/react-ui/Modal",
    "/Packages/react-ui/Popover",
    "/Packages/react-ui/Progress",
    "/Packages/react-ui/Stepbar",
    "/Packages/react-ui/Table",
    # Packages – themes & tokens
    "/Packages/themes/TokenUsage",
    "/Packages/themes/Variables",
    "/Packages/themes/Fonts",
    # Packages – other
    "/Packages/react-icons/Icons",
    "/Packages/create",
    "/Packages/create/TodoList",
    "/Packages/dashboard-docs",
    "/Packages/visualizations",
    "/Packages/splunk-ui-mcp",
    # Toolkits (DesignToolkits is internal)
    "/Toolkits/SUIT/Overview",
    "/Toolkits/SUIT/Backbone",
    "/Toolkits/SUIT/ExamplesGallery",
]

# Display name overrides for URL path segments (camelCase / abbreviations).
_SEGMENT_NAMES: dict[str, str] = {
    "DesignSystem": "Design System",
    "GettingStarted": "Getting started",
    "WorkingWithUs": "Working with us",
    "WorkingWithAI": "Working with AI",
    "DesignPrinciples": "Design principles",
    "DesignLevers": "Design levers",
    "Accessibility": "Accessibility",
    "Blueprints": "Blueprints",
    "CRUD": "CRUD",
    "Contribution": "Contribution",
    "FAQs": "FAQs",
    "Foundations": "Foundations",
    "Overview": "Overview",
    "Guidelines": "Guidelines",
    "Tenets": "Tenets",
    "Packages": "Packages",
    "react-ui": "React UI",
    "themes": "Themes",
    "react-icons": "React Icons",
    "create": "Create",
    "dashboard-docs": "Dashboard Docs",
    "visualizations": "Visualizations",
    "splunk-ui-mcp": "Splunk UI MCP",
    "Toolkits": "Toolkits",
    "SUIT": "SUIT",
    "DesignToolkits": "Design Toolkits",
    "ExamplesGallery": "Examples Gallery",
    "Backbone": "Backbone",
    "TokenUsage": "Token Usage",
    "Variables": "Variables",
    "Fonts": "Fonts",
    "Icons": "Icons",
    "TodoList": "Todo List",
}

_CAMEL_RE = re.compile(r"(?<=[a-z])(?=[A-Z])|(?<=[A-Z])(?=[A-Z][a-z])")


def _segment_label(seg: str) -> str:
    """Convert a URL path segment to a human-readable label."""
    if seg in _SEGMENT_NAMES:
        return _SEGMENT_NAMES[seg]
    return _CAMEL_RE.sub(" ", seg)


def _breadcrumbs_for(path: str) -> list[str]:
    """Derive breadcrumbs from a URL path like /DesignSystem/GettingStarted."""
    segs = [s for s in path.strip("/").split("/") if s]
    return ["Splunk Design System"] + [_segment_label(s) for s in segs]


def splunk_ui_entries(product: ProductConfig) -> list[SitemapEntry]:
    """Return synthetic SitemapEntry objects for all known splunkui routes."""
    return [
        SitemapEntry(
            url=BASE_URL + path,
            lastmod=None,
            product=product.name,
            version=None,
        )
        for path in ROUTES
    ]


async def _render_page(browser_context, url: str, sem: asyncio.Semaphore) -> tuple[str, str]:
    """Navigate to *url* in a new browser tab, wait for content, return (title, html)."""
    async with sem:
        page = await browser_context.new_page()
        try:
            await page.goto(url, wait_until="domcontentloaded", timeout=PAGE_TIMEOUT_MS)
            await page.wait_for_function(
                """config => config.selectors.some(selector => {
                    const element = document.querySelector(selector);
                    return element &&
                        element.textContent.trim().length >= config.minTextLength;
                })""",
                arg={
                    "selectors": CONTENT_SELECTORS,
                    "minTextLength": MIN_CONTENT_TEXT_LENGTH,
                },
                timeout=PAGE_TIMEOUT_MS,
            )
            title = await page.title()
            html = await page.content()
            return title, html
        finally:
            await page.close()


def _extract_content(html: str, url: str) -> tuple[PageContent, str]:
    """Extract PageContent + markdown from a fully-rendered splunkui page."""
    from bs4 import BeautifulSoup

    soup = BeautifulSoup(html, "lxml")

    # Title: prefer h1 over <title> tag (which shows the package name)
    h1 = soup.find("h1")
    title = h1.get_text(strip=True) if h1 else ""
    if not title:
        t = soup.find("title")
        title = t.get_text(strip=True).split(" - ")[0] if t else url.rstrip("/").split("/")[-1]

    # Content: the MDX-rendered section has a stable id prefix; fall back to
    # the layout main area, then the generic <main> element.
    content_section = soup.select_one("section[id^='markdown-content-']")
    if content_section is None:
        content_section = soup.find(id="react-docs-layout-main")
        if content_section:
            # Remove sidebar nav so we don't index navigation links as content
            for aside in content_section.find_all("nav", attrs={"aria-label": "Secondary"}):
                aside.decompose()
    if content_section is None:
        content_section = soup.find("main")
        if content_section:
            for aside in content_section.find_all("nav"):
                aside.decompose()

    if content_section is None:
        raise ExtractError(f"No content section found on {url}")

    content_html = str(content_section)
    markdown = to_markdown(content_html)

    if not markdown.strip():
        raise ExtractError(f"Empty content on {url}")

    # Reject 404-style placeholder pages injected by the SPA router
    if title.lower() in ("page not found", "not found") or "does not seem to exist" in markdown:
        raise ExtractError(f"Page not found (auth-gated or missing): {url}")

    # Breadcrumbs from URL path
    from urllib.parse import urlsplit
    path = urlsplit(url).path
    breadcrumbs = _breadcrumbs_for(path)

    page = PageContent(
        title=title,
        breadcrumbs=breadcrumbs,
        content_html=content_html,
    )
    return page, markdown


async def crawl_splunk_ui(
    config: CrawlerConfig,
    product: ProductConfig,
    state: CrawlState,
    *,
    limit: int | None = None,
    force: bool = False,
    stop_event: asyncio.Event | None = None,
) -> CrawlStats:
    """Crawl splunkui.splunk.com using Playwright (no sitemap — routes are hardcoded)."""
    from playwright.async_api import async_playwright

    stats = CrawlStats()
    entries = splunk_ui_entries(product)
    stats.selected = len(entries)
    log.info("%s: %d hardcoded routes", product.name, len(entries))

    pending: list[SitemapEntry] = []
    for entry in entries:
        if not force and not state.needs_fetch(entry.url, entry.lastmod):
            stats.skipped_fresh += 1
        else:
            pending.append(entry)
    if limit is not None:
        pending = pending[:limit]
    log.info("%s: %d to render, %d already fresh", product.name, len(pending), stats.skipped_fresh)

    if not pending:
        return stats

    # A t4g.medium cannot reliably render 20 Chromium tabs in parallel. Keep
    # HTTP crawling at the configured concurrency while bounding this SPA.
    sem = asyncio.Semaphore(min(config.concurrency, MAX_BROWSER_CONCURRENCY))

    async with async_playwright() as pw:
        try:
            browser = await pw.chromium.launch()
        except Exception as exc:
            msg = str(exc)
            if "missing dependencies" in msg or "Host system is missing" in msg or "BrowserType.launch" in msg:
                raise RuntimeError(
                    "Playwright cannot launch Chromium — system libraries are missing.\n"
                    "\n"
                    "On Amazon Linux 2023 / RHEL / Fedora (including Graviton/aarch64), install:\n"
                    "\n"
                    "  sudo dnf install -y atk libX11 libXcomposite libXdamage libXext \\\n"
                    "    libXfixes libXrandr mesa-libgbm libxcb libxkbcommon \\\n"
                    "    alsa-lib at-spi2-atk nss nspr libdrm cups-libs \\\n"
                    "    cairo pango\n"
                    "\n"
                    "Then re-download the browser:\n"
                    "  uv run playwright install chromium"
                ) from exc
            raise
        context = await browser.new_context()
        try:
            tasks = [
                asyncio.create_task(_render_and_write(context, entry, config, product, state, stats, sem, stop_event))
                for entry in pending
            ]
            await asyncio.gather(*tasks)
        finally:
            await context.close()
            await browser.close()

    return stats


async def _render_and_write(
    context,
    entry: SitemapEntry,
    config: CrawlerConfig,
    product: ProductConfig,
    state: CrawlState,
    stats: CrawlStats,
    sem: asyncio.Semaphore,
    stop_event: asyncio.Event | None,
) -> None:
    if stop_event is not None and stop_event.is_set():
        return
    try:
        _page_title, html = await _render_page(context, entry.url, sem)
        stats.fetched += 1

        page, markdown = _extract_content(html, entry.url)

        document = render_document(entry, page, markdown)
        path = output_path(config.output_dir, entry, product)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(document)
        stats.written += 1

        state.record(
            entry.url,
            product=entry.product,
            version=entry.version,
            lastmod=entry.lastmod,
            content_hash=content_hash(markdown),
            file_path=str(path.relative_to(config.output_dir)),
            http_status=200,
        )
        log.debug("wrote %s", path.relative_to(config.output_dir))
    except ExtractError as exc:
        stats.errors.append(str(exc))
        log.warning("%s: %s", entry.url, exc)
        state.record(
            entry.url,
            product=entry.product,
            version=entry.version,
            lastmod=None,
            content_hash=None,
            file_path=None,
            http_status=200,
        )
    except Exception as exc:
        stats.errors.append(f"{entry.url}: {exc}")
        log.error("%s: %s", entry.url, exc)
        state.record(
            entry.url,
            product=entry.product,
            version=entry.version,
            lastmod=None,
            content_hash=None,
            file_path=None,
            http_status=0,
        )

    done = stats.written + len(stats.errors)
    if done and done % 10 == 0:
        log.info("%s: %d rendered so far", product.name, done)
