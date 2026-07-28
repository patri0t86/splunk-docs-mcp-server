import unittest
from pathlib import Path

from splunk_docs_crawler.config import ProductConfig
from splunk_docs_crawler.crawl import output_path
from splunk_docs_crawler.sitemap import SitemapEntry, extract_version


class ExtractVersionTests(unittest.TestCase):
    def test_returns_standalone_version(self) -> None:
        self.assertEqual(extract_version("/en/splunk-enterprise/get-started/10.4/about"), "10.4")

    def test_returns_patch_version_from_topic_slug(self) -> None:
        path = (
            "/en/splunk-enterprise/administer/admin-manual/10.0/"
            "configuration-file-reference/10.0.1-configuration-file-reference/restmap.conf"
        )
        self.assertEqual(extract_version(path), "10.0.1")

    def test_ignores_non_version_numeric_prefixes(self) -> None:
        self.assertIsNone(extract_version("/en/splunk-enterprise/404-errors/about"))


class OutputPathTests(unittest.TestCase):
    product = ProductConfig(
        name="splunk-enterprise",
        sitemap="https://help.splunk.com/en/splunk-enterprise/sitemap.xml",
        path_prefix="/en/splunk-enterprise/",
    )

    def path_for(self, url: str, version: str) -> str:
        entry = SitemapEntry(url=url, lastmod=None, product=self.product.name, version=version)
        return str(output_path(Path("/data"), entry, self.product))

    def test_strips_single_version_segment(self) -> None:
        self.assertEqual(
            self.path_for("https://help.splunk.com/en/splunk-enterprise/get-started/10.4/about", "10.4"),
            "/data/splunk-enterprise/10.4/get-started/about.md",
        )

    def test_strips_every_pure_version_segment_on_patch_pages(self) -> None:
        url = (
            "https://help.splunk.com/en/splunk-enterprise/administer/admin-manual/10.4/"
            "configuration-file-reference/10.4.1-configuration-file-reference/authorize.conf"
        )
        self.assertEqual(
            self.path_for(url, "10.4.1"),
            "/data/splunk-enterprise/10.4.1/administer/admin-manual/"
            "configuration-file-reference/10.4.1-configuration-file-reference/authorize.conf.md",
        )

    def test_uses_index_name_when_path_is_empty(self) -> None:
        product = ProductConfig(
            name="splunk-lantern",
            sitemap="https://lantern.splunk.com/sitemap.xml",
            path_prefix="/",
            include_unversioned=True,
        )
        entry = SitemapEntry(url="https://lantern.splunk.com/", lastmod=None, product=product.name, version=None)
        self.assertEqual(
            str(output_path(Path("/data"), entry, product)),
            "/data/splunk-lantern/_unversioned/index.md",
        )