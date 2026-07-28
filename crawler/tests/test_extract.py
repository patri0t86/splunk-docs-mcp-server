import unittest

from splunk_docs_crawler.extract import extract


class ExtractTests(unittest.TestCase):
    def test_extracts_lantern_content_and_breadcrumbs(self) -> None:
        html = """
        <html><body>
          <ol class="mt-breadcrumbs">
            <li><a href="/">Home</a></li>
            <li><a href="/Security_Use_Cases">Security Use Cases</a></li>
            <li class="mt-breadcrumbs-current-page">Triaging Crowdstrike malware data</li>
          </ol>
          <article id="elm-main-content">
            <aside class="mt-content-side">sidebar links</aside>
            <section class="mt-content-container">
              <h1>Triaging Crowdstrike malware data</h1>
              <p>Analysts need faster triage.</p>
            </section>
          </article>
        </body></html>
        """
        page = extract(
            html,
            "https://lantern.splunk.com/Security_Use_Cases/Advanced_Threat_Detection/Triaging_Crowdstrike_malware_data",
        )
        self.assertEqual(page.title, "Triaging Crowdstrike malware data")
        self.assertEqual(
            page.breadcrumbs,
            ["Home", "Security Use Cases", "Triaging Crowdstrike malware data"],
        )
        self.assertIn("Analysts need faster triage.", page.content_html)
        self.assertNotIn("sidebar links", page.content_html)
