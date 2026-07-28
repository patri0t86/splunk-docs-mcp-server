import unittest
import json

from splunk_docs_crawler.extract_dev_splunk import (
    _parse_rsc_chunks,
    _find_compiled_mdx,
    _find_title,
    _find_in_nav,
    _build_breadcrumbs,
    _mdx_to_markdown,
    extract_dev_splunk,
)
from splunk_docs_crawler.extract import ExtractError


def _make_rsc_html(*chunks: str) -> str:
    """Build a minimal HTML page containing the given RSC push strings."""
    scripts = "\n".join(
        f'<script>self.__next_f.push([1,{json.dumps(c)}])</script>'
        for c in chunks
    )
    return f"<html><body>{scripts}</body></html>"


# Minimal compiled MDX source that produces one paragraph.
_MINIMAL_MDX = """\
"use strict";
const {Fragment: _Fragment, jsx: _jsx, jsxs: _jsxs} = arguments[0];
const {useMDXComponents: _provideComponents} = arguments[0];
function _createMdxContent(props) {
  const _components = {p: "p", h2: "h2", ..._provideComponents(), ...props.components};
  return _jsxs(_Fragment, {
    children: [
      _jsx(_components.p, {children: "Hello world."}),
      "\\n",
      _jsx(_components.h2, {id: "details", children: "Details"}),
      "\\n",
      _jsx(_components.p, {children: "More text here."})
    ]
  });
}
function MDXContent(props = {}) {
  const {wrapper: MDXLayout} = {..._provideComponents(), ...props.components};
  return _createMdxContent(props);
}
export default MDXContent;
"""

_TITLE_CHUNK = '16:{"title":{"title":"Test Page Title","headingLevel":1},"toc":[]}'

_NAV_CHUNK = (
    '6:{"currentPath":"/docs/testpage","navigationItems":['
    '{"path":"/welcome","title":"Welcome","order":1,"children":null},'
    '{"path":"/docs","title":"Documentation","order":2,"children":['
    '{"path":"/docs/testpage","title":"Test Page","order":1,"children":null}'
    ']}]}'
)


class TestParseRscChunks(unittest.TestCase):
    def test_extracts_chunks(self) -> None:
        html = _make_rsc_html("chunk one", "chunk two")
        chunks = _parse_rsc_chunks(html)
        self.assertEqual(chunks, ["chunk one", "chunk two"])

    def test_skips_malformed_json(self) -> None:
        html = "<html><body><script>self.__next_f.push([1,bad])</script></body></html>"
        chunks = _parse_rsc_chunks(html)
        self.assertEqual(chunks, [])


class TestFindCompiledMdx(unittest.TestCase):
    def test_finds_mdx_source(self) -> None:
        chunks = [_TITLE_CHUNK, _MINIMAL_MDX]
        result = _find_compiled_mdx(chunks)
        self.assertIsNotNone(result)
        self.assertIn("_createMdxContent", result)

    def test_returns_none_when_absent(self) -> None:
        self.assertIsNone(_find_compiled_mdx([_TITLE_CHUNK, _NAV_CHUNK]))


class TestFindTitle(unittest.TestCase):
    def test_extracts_title(self) -> None:
        title = _find_title([_NAV_CHUNK, _TITLE_CHUNK])
        self.assertEqual(title, "Test Page Title")

    def test_returns_empty_when_absent(self) -> None:
        self.assertEqual(_find_title(["no title here"]), "")


class TestFindInNav(unittest.TestCase):
    nav = [
        {"path": "/welcome", "title": "Welcome", "children": None},
        {
            "path": "/docs",
            "title": "Documentation",
            "children": [
                {"path": "/docs/testpage", "title": "Test Page", "children": None},
                {"path": "/docs/other", "title": "Other Page", "children": None},
            ],
        },
    ]

    def test_finds_nested_path(self) -> None:
        result = _find_in_nav(self.nav, "/docs/testpage")
        self.assertEqual(result, ["Documentation", "Test Page"])

    def test_finds_top_level_path(self) -> None:
        result = _find_in_nav(self.nav, "/welcome")
        self.assertEqual(result, ["Welcome"])

    def test_returns_none_for_missing_path(self) -> None:
        self.assertIsNone(_find_in_nav(self.nav, "/missing"))


class TestBuildBreadcrumbs(unittest.TestCase):
    nav = [
        {
            "path": "/developapps",
            "title": "Develop Apps",
            "children": [
                {"path": "/developapps/createapps", "title": "Create apps", "children": [
                    {"path": "/developapps/createapps/appanatomy", "title": "Anatomy of a Splunk app", "children": None}
                ]},
            ],
        },
    ]

    def test_builds_from_nav_tree(self) -> None:
        crumbs = _build_breadcrumbs(
            self.nav,
            "/developapps/createapps/appanatomy",
            "https://dev.splunk.com/enterprise/docs/developapps/createapps/appanatomy/",
        )
        self.assertEqual(crumbs, [
            "Splunk Developer Program",
            "Enterprise",
            "Develop Apps",
            "Create apps",
            "Anatomy of a Splunk app",
        ])

    def test_fallback_when_nav_empty(self) -> None:
        crumbs = _build_breadcrumbs(
            [],
            "",
            "https://dev.splunk.com/observability/docs/signalflow/",
        )
        self.assertIn("Splunk Developer Program", crumbs)
        self.assertIn("Observability Cloud", crumbs)


class TestMdxToMarkdown(unittest.TestCase):
    def test_basic_conversion(self) -> None:
        md = _mdx_to_markdown(_MINIMAL_MDX)
        self.assertIn("Hello world.", md)
        self.assertIn("## Details", md)
        self.assertIn("More text here.", md)

    def test_raises_on_bad_source(self) -> None:
        with self.assertRaises(ExtractError):
            _mdx_to_markdown("this is not valid MDX at all }{}{}{")


class TestExtractDevSplunk(unittest.TestCase):
    def test_full_extraction(self) -> None:
        html = _make_rsc_html(_MINIMAL_MDX, _TITLE_CHUNK, _NAV_CHUNK)
        page, markdown = extract_dev_splunk(
            html, "https://dev.splunk.com/enterprise/docs/testpage/"
        )
        self.assertEqual(page.title, "Test Page Title")
        self.assertIn("Hello world.", markdown)
        self.assertIn("## Details", markdown)
        self.assertIn("Splunk Developer Program", page.breadcrumbs)

    def test_raises_when_no_mdx(self) -> None:
        html = _make_rsc_html(_TITLE_CHUNK, _NAV_CHUNK)
        with self.assertRaises(ExtractError):
            extract_dev_splunk(html, "https://dev.splunk.com/enterprise/")
