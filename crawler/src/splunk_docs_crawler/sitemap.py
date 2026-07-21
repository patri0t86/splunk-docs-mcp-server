"""Sitemap parsing and URL selection.

help.splunk.com publishes one sitemap per product (the root sitemap.xml is an
index of them). Product sitemaps are flat <urlset> documents with <loc> and
<lastmod> per page, and the doc version appears as a path segment, e.g.

    /en/splunk-enterprise/get-started/overview/10.4/about-.../about-...

so selecting a product+version scope is pure URL filtering — no link crawling.
"""

from __future__ import annotations

import xml.etree.ElementTree as ET
from dataclasses import dataclass
from urllib.parse import urlsplit

from .config import VERSION_PREFIX_RE, ProductConfig

SITEMAP_NS = "{http://www.sitemaps.org/schemas/sitemap/0.9}"


@dataclass(frozen=True)
class SitemapEntry:
    url: str
    lastmod: str | None
    product: str
    version: str | None


def parse_sitemap(xml_bytes: bytes) -> tuple[str, list[tuple[str, str | None]]]:
    """Parse a sitemap document.

    Returns ("index", [(child_sitemap_url, None), ...]) for a sitemapindex, or
    ("urlset", [(page_url, lastmod), ...]) for a regular sitemap.
    """
    root = ET.fromstring(xml_bytes)
    kind = "index" if root.tag == f"{SITEMAP_NS}sitemapindex" else "urlset"
    entries: list[tuple[str, str | None]] = []
    child_tag = f"{SITEMAP_NS}sitemap" if kind == "index" else f"{SITEMAP_NS}url"
    for child in root.iter(child_tag):
        loc = child.find(f"{SITEMAP_NS}loc")
        if loc is None or not loc.text:
            continue
        lastmod = child.find(f"{SITEMAP_NS}lastmod")
        entries.append((loc.text.strip(), lastmod.text.strip() if lastmod is not None and lastmod.text else None))
    return kind, entries


def extract_version(url_path: str) -> str | None:
    """Return the most specific version marker from a documentation URL."""
    version = None
    for segment in url_path.split("/"):
        match = VERSION_PREFIX_RE.match(segment)
        if match:
            version = match.group(1)
    return version


def select_entries(
    entries: list[tuple[str, str | None]], product: ProductConfig
) -> list[SitemapEntry]:
    """Filter raw sitemap entries down to the configured product/version scope."""
    selected = []
    for url, lastmod in entries:
        path = urlsplit(url).path
        if not path.startswith(product.path_prefix):
            continue
        version = extract_version(path)
        if not product.accepts_version(version):
            continue
        selected.append(SitemapEntry(url=url, lastmod=lastmod, product=product.name, version=version))
    return selected
