import asyncio
import unittest

from splunk_docs_crawler.crawl_splunk_ui import (
    CONTENT_SELECTORS,
    MIN_CONTENT_TEXT_LENGTH,
    PAGE_TIMEOUT_MS,
    _extract_content,
    _render_page,
)


class SplunkUiExtractionTests(unittest.TestCase):
    def test_extracts_package_markdown_section(self) -> None:
        html = """
        <html>
          <head><title>@splunk/react-ui - 5.12.0</title></head>
          <body>
            <nav aria-label="Secondary">Package navigation</nav>
            <main id="react-docs-layout-main">
              <section id="markdown-content-generated-id">
                <h1>Button</h1>
                <p>Buttons trigger actions.</p>
              </section>
            </main>
          </body>
        </html>
        """
        page, markdown = _extract_content(
            html, "https://splunkui.splunk.com/Packages/react-ui/Button"
        )
        self.assertEqual(page.title, "Button")
        self.assertIn("Buttons trigger actions.", markdown)
        self.assertNotIn("Package navigation", markdown)


class _FakePage:
    def __init__(self) -> None:
        self.closed = False
        self.goto_args = None
        self.wait_args = None

    async def goto(self, *args, **kwargs) -> None:
        self.goto_args = (args, kwargs)

    async def wait_for_function(self, *args, **kwargs) -> None:
        self.wait_args = (args, kwargs)

    async def title(self) -> str:
        return "Rendered title"

    async def content(self) -> str:
        return "<main><h1>Rendered title</h1></main>"

    async def close(self) -> None:
        self.closed = True


class _FakeContext:
    def __init__(self, page: _FakePage) -> None:
        self.page = page

    async def new_page(self) -> _FakePage:
        return self.page


class SplunkUiRenderingTests(unittest.IsolatedAsyncioTestCase):
    async def test_waits_for_meaningful_content_before_capture(self) -> None:
        page = _FakePage()
        title, html = await _render_page(
            _FakeContext(page),
            "https://splunkui.splunk.com/Packages/react-ui/Button",
            asyncio.Semaphore(1),
        )
        self.assertEqual(title, "Rendered title")
        self.assertIn("Rendered title", html)
        self.assertEqual(page.goto_args[1]["wait_until"], "domcontentloaded")
        self.assertEqual(page.goto_args[1]["timeout"], PAGE_TIMEOUT_MS)
        self.assertEqual(
            page.wait_args[1]["arg"],
            {
                "selectors": CONTENT_SELECTORS,
                "minTextLength": MIN_CONTENT_TEXT_LENGTH,
            },
        )
        self.assertEqual(page.wait_args[1]["timeout"], PAGE_TIMEOUT_MS)
        self.assertTrue(page.closed)
