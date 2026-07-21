"""Extract the documentation article from a help.splunk.com page.

Pages are server-rendered from a DITA CCMS (Heretto). The layout that matters:

    <div id="main-content-wrapper" class="content-body">
      <nav class="breadcrumbs"> <a>...</a> ... </nav>
      <div class="content-area">
        <select id="version-select">...</select>
        <div class="dita-content-container">
          <main role="main">
            <article role="article">
              <h1 class="title">...</h1>
              ... sections, nested <article class="topic"> with h2/h3 ...
"""

from __future__ import annotations

from dataclasses import dataclass
from urllib.parse import urljoin

from bs4 import BeautifulSoup

# Elements that are chrome/noise, never article content.
NOISE_SELECTORS = [
    "script",
    "style",
    "noscript",
    "nav",
    "select",
    "button",
    ".feedback",
    "[data-testid='version-dropdown']",
    # Copy-to-clipboard widget around code blocks: the header carries a
    # "CODE" label and the textarea duplicates the <pre> content.
    ".code-block-header",
    "textarea.code-raw-content",
]


class ExtractError(Exception):
    pass


@dataclass
class PageContent:
    title: str
    breadcrumbs: list[str]
    content_html: str


def extract(html: str, page_url: str) -> PageContent:
    soup = BeautifulSoup(html, "lxml")

    breadcrumbs = [
        a.get_text(strip=True)
        for a in soup.select("nav.breadcrumbs a")
        if a.get_text(strip=True)
    ]

    main = (
        soup.select_one("div.dita-content-container main")
        or soup.select_one("article[role='article']")
        or soup.select_one("#main-content-wrapper")
    )
    if main is None:
        raise ExtractError(f"no article content found in {page_url}")

    for selector in NOISE_SELECTORS:
        for node in main.select(selector):
            node.decompose()

    title_node = main.find("h1")
    title = title_node.get_text(strip=True) if title_node else (breadcrumbs[-1] if breadcrumbs else page_url)

    # Make links and images absolute so the markdown is usable out of context.
    for attr, tag in (("href", "a"), ("src", "img")):
        for node in main.find_all(tag):
            value = node.get(attr)
            if value and not value.startswith(("#", "mailto:", "data:")):
                node[attr] = urljoin(page_url, value)

    return PageContent(title=title, breadcrumbs=breadcrumbs, content_html=str(main))
