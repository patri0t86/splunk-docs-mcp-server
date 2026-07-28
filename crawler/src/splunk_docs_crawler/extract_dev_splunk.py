"""Extract documentation from dev.splunk.com (Next.js App Router with RSC/MDX).

dev.splunk.com is a Next.js App Router site whose content is delivered as
React Server Components (RSC). Actual article text lives in a compiled MDX
function (``function _createMdxContent(...)``) embedded in the RSC stream via
``self.__next_f.push([1, "..."])`` script tags — not in ordinary HTML.

This module:
1. Parses RSC stream chunks out of the raw HTML.
2. Locates the compiled MDX source, the page title, and the navigation tree.
3. Evaluates the compiled MDX via a Node.js subprocess using a shim that
   renders JSX to markdown text.
4. Builds breadcrumbs from the nav tree and URL path.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import tempfile
from urllib.parse import urlsplit

from .extract import ExtractError, PageContent

# ---------------------------------------------------------------------------
# Node.js shim: evaluates the compiled MDX source and writes markdown to stdout.
# The compiled source uses ``arguments[0]`` to receive {Fragment, jsx, jsxs,
# useMDXComponents}. We patch out those two destructuring lines and pass
# our shims directly as function parameters to ``new Function(...)``.
# ---------------------------------------------------------------------------
_NODE_SHIM = r"""
'use strict';
process.stdin.setEncoding('utf8');
let src = '';
process.stdin.on('data', d => { src += d; });
process.stdin.on('end', () => {
  const Fragment = Symbol('Fragment');
  const LI_SEP = '\uE000';

  function extractText(v) {
    if (v === null || v === undefined) return '';
    if (typeof v === 'string') return v;
    if (Array.isArray(v)) return v.map(extractText).join('');
    if (typeof v === 'object' && '__text' in v) return v.__text;
    return '';
  }

  function makeEl(type, props) {
    const ch = extractText(props && props.children);
    switch (type) {
      case 'h1': return '# ' + ch.trim() + '\n\n';
      case 'h2': return '## ' + ch.trim() + '\n\n';
      case 'h3': return '### ' + ch.trim() + '\n\n';
      case 'h4': return '#### ' + ch.trim() + '\n\n';
      case 'h5': return '##### ' + ch.trim() + '\n\n';
      case 'p':  return ch.trim() ? ch.trim() + '\n\n' : '';
      case 'li': return LI_SEP + ch.trim() + LI_SEP;
      case 'ul': {
        const items = ch.split(LI_SEP).filter((_, i) => i % 2 === 1);
        if (!items.length) return ch;
        return items.map(s => '- ' + s.replace(/\n/g, '\n  ')).join('\n') + '\n\n';
      }
      case 'ol': {
        const items = ch.split(LI_SEP).filter((_, i) => i % 2 === 1);
        if (!items.length) return ch;
        return items.map((s, i) => (i + 1) + '. ' + s.replace(/\n/g, '\n   ')).join('\n') + '\n\n';
      }
      case 'a': {
        const href = (props && props.href) || '';
        return '[' + (ch || href) + '](' + href + ')';
      }
      case 'em': case 'i':         return '*' + ch + '*';
      case 'strong': case 'b':     return '**' + ch + '**';
      case 'del': case 's':        return '~~' + ch + '~~';
      case 'code': {
        const lang = ((props && props.className) || '').replace(/language-|code-/g, '').trim();
        if (ch.includes('\n') || lang) return '```' + lang + '\n' + ch.trimEnd() + '\n```\n\n';
        return '`' + ch + '`';
      }
      case 'pre':    return ch;
      case 'blockquote':
        return '> ' + ch.trim().replace(/\n/g, '\n> ') + '\n\n';
      case 'img': {
        const alt = (props && props.alt) || '';
        const imgSrc = (props && props.src) || '';
        return '![' + alt + '](' + imgSrc + ')\n\n';
      }
      case 'table': return ch.trim() + '\n\n';
      case 'thead': case 'tbody': return ch;
      case 'tr':    return ch.trim() + ' |\n';
      case 'th': case 'td': return '| ' + extractText(props && props.children) + ' ';
      case 'hr':    return '---\n\n';
      case 'br':    return '\n';
      default:      return ch;
    }
  }

  const jsx = (type, props) => {
    if (type === Fragment) return { __text: extractText(props && props.children) };
    if (typeof type === 'string') return { __text: makeEl(type, props) };
    // Functions: call them (handles both _createMdxContent and custom components
    // like CardLayout/PreviousNextWidget which are stubs that return children).
    if (typeof type === 'function') {
      try { return type(props || {}); } catch (_) { return { __text: '' }; }
    }
    return { __text: extractText(props && props.children) };
  };
  const jsxs = jsx;

  // Stub for any custom component: returns a no-op function that renders children.
  const stubComponent = (props) => ({ __text: extractText(props && props.children) });
  const _provideComponents = () => ({});

  // Patch compiled source: remove the two arguments[0] destructuring lines
  // and the ESM export so we can run it with new Function().
  // Production builds return { default: MDXContent } instead of using
  // `export default MDXContent;`, so we must handle both forms.
  // Also neutralise _missingMdxReference guards — custom components like
  // CardLayout and PreviousNextWidget aren't provided by _provideComponents,
  // so MDX would throw before reaching the jsx() call. We replace the guard
  // function with a no-op so those components fall back to children-only output.
  const patched = src
    .replace(/^"use strict";\s*/m, '')
    .replace(/const \{Fragment:\s*_Fragment,\s*jsx:\s*_jsx,\s*jsxs:\s*_jsxs\}\s*=\s*arguments\[0\];\s*/m, '')
    .replace(/const \{useMDXComponents:\s*_provideComponents\}\s*=\s*arguments\[0\];\s*/m, '')
    .replace(/export default (\w+);\s*$/m, 'return $1;')
    .replace(/function _missingMdxReference\s*\([^)]*\)\s*\{[\s\S]*?\}/m, 'function _missingMdxReference() {}');

  try {
    const fn = new Function('_Fragment', '_jsx', '_jsxs', '_provideComponents', patched);
    const rawResult = fn(Fragment, jsx, jsxs, _provideComponents);
    .replace(/export default (\w+);\s*$/m, 'return $1;')
    .replace(/function _missingMdxReference\s*\([^)]*\)\s*\{[\s\S]*?\}/m, 'function _missingMdxReference() {}');

  try {
    const fn = new Function('_Fragment', '_jsx', '_jsxs', '_provideComponents', patched);
    const rawResult = fn(Fragment, jsx, jsxs, _provideComponents);
    // Unwrap CJS-style `return { default: MDXContent }` produced by some bundlers.
    const MDXContent = (rawResult && typeof rawResult === 'object' && typeof rawResult.default === 'function')
      ? rawResult.default
      : rawResult;
    if (!MDXContent || typeof MDXContent !== 'function') {
      process.stderr.write('MDXContent is not a function\n');
      process.exit(1);
    }
    const result = MDXContent({});
    const text = (result && result.__text) ? result.__text : '';
    process.stdout.write(text);
  } catch (e) {
    process.stderr.write('Error evaluating MDX: ' + e.toString() + '\n');
    process.exit(1);
  }
});
"""

# Maps the first URL path segment to a human-readable section title.
_SECTION_TITLES: dict[str, str] = {
    "enterprise": "Enterprise",
    "observability": "Observability Cloud",
    "search": "Search",
}


# ---------------------------------------------------------------------------
# RSC stream parsing
# ---------------------------------------------------------------------------

def _parse_rsc_chunks(html: str) -> list[str]:
    """Return the decoded string payloads from all ``self.__next_f.push([1, "..."])``
    calls in the page HTML.  Each returned string is a multi-line RSC chunk
    (lines of ``ID:data``).
    """
    raw_matches = re.findall(
        r'self\.__next_f\.push\(\[1,(\".*?\")\]\)</script>',
        html,
        re.DOTALL,
    )
    chunks = []
    for raw in raw_matches:
        try:
            chunks.append(json.loads(raw))
        except json.JSONDecodeError:
            pass
    return chunks


def _find_compiled_mdx(chunks: list[str]) -> str | None:
    """Return the compiled MDX source string, or None if not present."""
    for chunk in chunks:
        if "function _createMdxContent" in chunk:
            return chunk
    return None


def _find_title(chunks: list[str]) -> str:
    """Return the page title from RSC metadata, or ''."""
    for chunk in chunks:
        m = re.search(r'"title":\{"title":"([^"]+)"', chunk)
        if m:
            return m.group(1)
    # Fallback: <title> in HTML head
    for chunk in chunks:
        m = re.search(r'"children":"([^"]+) \| ', chunk)
        if m:
            return m.group(1)
    return ""


def _find_nav_data(chunks: list[str]) -> tuple[list[dict], str]:
    """Return (navigationItems, currentPath) from the RSC nav chunk."""
    for chunk in chunks:
        if '"currentPath"' not in chunk or '"navigationItems"' not in chunk:
            continue
        m = re.search(r'"currentPath":"([^"]+)"', chunk)
        current_path = m.group(1) if m else ""
        # Extract navigationItems JSON array — may be large; find its boundaries
        idx = chunk.find('"navigationItems":[')
        if idx < 0:
            continue
        bracket_start = chunk.index("[", idx)
        # Walk to find the matching closing bracket
        depth = 0
        for i, ch in enumerate(chunk[bracket_start:], bracket_start):
            if ch == "[":
                depth += 1
            elif ch == "]":
                depth -= 1
                if depth == 0:
                    raw_nav = chunk[bracket_start : i + 1]
                    try:
                        nav_items = json.loads(raw_nav)
                    except json.JSONDecodeError:
                        nav_items = []
                    return nav_items, current_path
    return [], ""


def _find_in_nav(items: list[dict], target: str) -> list[str] | None:
    """DFS over the nav tree; return list of titles from root to the matching
    node, or None if not found.
    """
    for item in items:
        if not isinstance(item, dict):
            continue
        if item.get("path") == target:
            return [item.get("title", "")]
        children = item.get("children")
        if isinstance(children, list):
            sub = _find_in_nav(children, target)
            if sub is not None:
                return [item.get("title", "")] + sub
    return None


def _build_breadcrumbs(nav_items: list[dict], current_path: str, page_url: str) -> list[str]:
    """Build a breadcrumb list for a dev.splunk.com page."""
    url_parts = urlsplit(page_url).path.strip("/").split("/")
    section = url_parts[0] if url_parts else ""
    section_title = _SECTION_TITLES.get(section, section.title())

    nav_crumbs = _find_in_nav(nav_items, current_path) if current_path else None

    if nav_crumbs:
        return ["Splunk Developer Program", section_title] + nav_crumbs
    # Fallback: humanize URL path segments
    crumbs = ["Splunk Developer Program", section_title]
    for seg in url_parts[2:]:  # skip product and 'docs'/'reference' prefix
        crumbs.append(seg.replace("-", " ").replace("_", " ").title())
    return crumbs


# ---------------------------------------------------------------------------
# Node.js invocation
# ---------------------------------------------------------------------------

def _mdx_to_markdown(compiled_source: str) -> str:
    """Evaluate the compiled MDX source via Node.js and return markdown text."""
    fd, shim_path = tempfile.mkstemp(suffix=".cjs")
    try:
        with os.fdopen(fd, "w") as f:
            f.write(_NODE_SHIM)
        result = subprocess.run(
            ["node", shim_path],
            input=compiled_source,
            capture_output=True,
            text=True,
            timeout=30,
        )
        if result.returncode != 0:
            raise ExtractError(
                f"MDX→markdown conversion failed: {result.stderr[:300]}"
            )
        return result.stdout
    finally:
        os.unlink(shim_path)


# ---------------------------------------------------------------------------
# Public entry point
# ---------------------------------------------------------------------------

def extract_dev_splunk(html: str, page_url: str) -> tuple[PageContent, str]:
    """Parse a dev.splunk.com page and return (PageContent, markdown).

    Raises ExtractError for pages that carry no article content (category,
    search, and other navigation-only pages).
    """
    chunks = _parse_rsc_chunks(html)

    compiled_source = _find_compiled_mdx(chunks)
    if compiled_source is None:
        raise ExtractError(f"no MDX article content found in {page_url}")

    title = _find_title(chunks)
    if not title:
        title = page_url

    nav_items, current_path = _find_nav_data(chunks)
    breadcrumbs = _build_breadcrumbs(nav_items, current_path, page_url)

    markdown = _mdx_to_markdown(compiled_source)
    if not markdown.strip():
        raise ExtractError(f"MDX conversion produced empty output for {page_url}")

    # PageContent.content_html is only used downstream by to_markdown(); since
    # we bypass that step for dev.splunk.com, the field value does not matter.
    page = PageContent(title=title, breadcrumbs=breadcrumbs, content_html="")
    return page, markdown
