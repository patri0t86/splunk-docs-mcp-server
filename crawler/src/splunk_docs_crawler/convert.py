"""HTML -> Markdown conversion and document serialization."""

from __future__ import annotations

import hashlib
import re
from datetime import datetime, timezone

import yaml
from markdownify import MarkdownConverter

from .extract import PageContent
from .sitemap import SitemapEntry


# DITA <pre class="codeblock <lang>"> carries the language; these classes are
# presentation-only, anything else is a language hint.
_NON_LANG_CLASSES = {"code-block-pre", "pre", "codeblock", "normalize-space", "screen"}
_LANG_MAP = {"search": "spl"}


def _code_language(pre) -> str:
    for cls in pre.get("class") or []:
        if cls not in _NON_LANG_CLASSES:
            return _LANG_MAP.get(cls, cls)
    return ""


class DocConverter(MarkdownConverter):
    """ATX headings, dash bullets, fenced code blocks with language tags."""

    class Options(MarkdownConverter.Options):
        heading_style = "atx"
        bullets = "-"
        escape_underscores = False
        escape_asterisks = False


_converter = DocConverter(code_language_callback=_code_language)

# Collapse runs of 3+ blank lines left behind by decomposed layout markup.
_BLANK_RUNS = re.compile(r"\n{3,}")


def to_markdown(content_html: str) -> str:
    markdown = _converter.convert(content_html)
    markdown = _BLANK_RUNS.sub("\n\n", markdown)
    return markdown.strip() + "\n"


def content_hash(markdown: str) -> str:
    return "sha256:" + hashlib.sha256(markdown.encode()).hexdigest()


def render_document(entry: SitemapEntry, page: PageContent, markdown: str) -> str:
    """Full .md file: YAML frontmatter (chunker metadata) + article body."""
    frontmatter = {
        "url": entry.url,
        "title": page.title,
        "product": entry.product,
        "version": entry.version,
        "breadcrumbs": page.breadcrumbs,
        "last_modified": entry.lastmod,
        "fetched_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "content_hash": content_hash(markdown),
    }
    header = yaml.safe_dump(frontmatter, sort_keys=False, allow_unicode=True, width=1000)
    return f"---\n{header}---\n\n{markdown}"
