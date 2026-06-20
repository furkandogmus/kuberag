import unittest
from unittest.mock import MagicMock, patch
from pathlib import Path
import os
import sys
import requests

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from rag_worker.sources import (
    _normalize_web_url,
    _parse_html,
    _read_pdf,
    _read_pdf_bytes,
    fetch_web,
)


class TestSources(unittest.TestCase):
    def setUp(self):
        resolver = patch(
            "rag_worker.sources._resolve_web_addresses",
            return_value={"93.184.216.34"},
        )
        self.mock_resolver = resolver.start()
        self.addCleanup(resolver.stop)

    @patch("pypdf.PdfReader")
    def test_read_pdf(self, mock_pdf_reader):
        # Setup mock reader & pages
        mock_reader = MagicMock()
        mock_page1 = MagicMock()
        mock_page1.extract_text.return_value = "Hello World"
        
        mock_page2 = MagicMock()
        mock_page2.extract_text.return_value = "Page Two"
        
        mock_reader.pages = [mock_page1, mock_page2]
        mock_pdf_reader.return_value = mock_reader
        
        text = _read_pdf(Path("dummy.pdf"))
        self.assertEqual(text, "Hello World\nPage Two")
        mock_pdf_reader.assert_called_once_with(Path("dummy.pdf"))

    @patch("pypdf.PdfReader")
    def test_read_pdf_bytes(self, mock_pdf_reader):
        # Setup mock reader & pages
        mock_reader = MagicMock()
        mock_page1 = MagicMock()
        mock_page1.extract_text.return_value = "Hello from bytes"
        
        mock_reader.pages = [mock_page1]
        mock_pdf_reader.return_value = mock_reader
        
        text = _read_pdf_bytes(b"dummy bytes")
        self.assertEqual(text, "Hello from bytes")
        
        # Verify it was called with a BytesIO stream
        args, kwargs = mock_pdf_reader.call_args
        self.assertTrue(len(args) > 0)
        import io
        self.assertIsInstance(args[0], io.BytesIO)
        self.assertEqual(args[0].getvalue(), b"dummy bytes")

    def test_parse_html_extracts_visible_text_links_and_base(self):
        text, links, base_href = _parse_html(
            """
            <html><head><base href="/docs/"><style>hidden css</style></head>
            <body><h1>RAG &amp; Kubernetes</h1>
            <script>hidden script</script><a href="guide.html#intro">Guide</a></body></html>
            """
        )

        self.assertEqual(text, "RAG & Kubernetes Guide")
        self.assertEqual(links, ["guide.html#intro"])
        self.assertEqual(base_href, "/docs/")

    def test_normalize_web_url_removes_fragment_userinfo_and_default_port(self):
        self.assertEqual(
            _normalize_web_url("HTTPS://user:pass@Example.COM:443/docs?q=1#section"),
            "https://example.com/docs?q=1",
        )
        self.assertIsNone(_normalize_web_url("javascript:alert(1)"))

    @patch("requests.get")
    def test_fetch_web_normalizes_and_deduplicates_links(self, mock_get):
        first = MagicMock(
            status_code=200,
            url="https://example.com/start",
            text=(
                '<base href="/docs/"><h1>Start</h1>'
                '<a href="guide.html#one">one</a>'
                '<a href="https://EXAMPLE.com:443/docs/guide.html#two">two</a>'
                '<a href="https://other.example/out">external</a>'
            ),
        )
        first.headers = {"content-type": "text/html; charset=utf-8"}
        second = MagicMock(
            status_code=200,
            url="https://example.com/docs/guide.html",
            text="<h1>Guide</h1>",
        )
        second.headers = {"content-type": "text/html"}
        mock_get.side_effect = [first, second]

        result = fetch_web({
            "name": "site",
            "type": "web",
            "web": {
                "urls": ["https://EXAMPLE.com:443/start#top"],
                "maxDepth": 1,
                "sameDomainOnly": True,
                "maxPages": 10,
            },
        })

        self.assertEqual(
            result.docs,
            [
                ("https://example.com/start", "Start one two external"),
                ("https://example.com/docs/guide.html", "Guide"),
            ],
        )
        self.assertEqual(mock_get.call_count, 2)

    @patch("requests.get")
    def test_fetch_web_rejects_cross_domain_redirect(self, mock_get):
        response = MagicMock(
            status_code=200,
            url="https://other.example/redirected",
            text="<h1>Other site</h1>",
        )
        response.headers = {"content-type": "text/html"}
        mock_get.return_value = response

        with self.assertRaisesRegex(RuntimeError, "redirected outside allowed domains"):
            fetch_web({
                "name": "site",
                "type": "web",
                "web": {
                    "urls": ["https://example.com/start"],
                    "maxDepth": 0,
                    "sameDomainOnly": True,
                    "maxPages": 10,
                },
            })

    @patch("requests.get")
    def test_fetch_web_max_pages_limits_requests_not_only_documents(self, mock_get):
        first = MagicMock(
            status_code=200,
            url="https://example.com/",
            text='<h1>Home</h1><a href="/empty">Empty</a><a href="/never">Never</a>',
        )
        first.headers = {"content-type": "text/html"}
        empty = MagicMock(status_code=200, url="https://example.com/empty", text="<script>empty</script>")
        empty.headers = {"content-type": "text/html"}
        mock_get.side_effect = [first, empty]

        result = fetch_web({
            "name": "site",
            "type": "web",
            "web": {
                "urls": ["https://example.com/"],
                "maxDepth": 1,
                "sameDomainOnly": True,
                "maxPages": 2,
            },
        })

        self.assertEqual(result.docs, [("https://example.com/", "Home Empty Never")])
        self.assertEqual(mock_get.call_count, 2)

    @patch("requests.get")
    def test_fetch_web_fails_when_seed_is_unreachable(self, mock_get):
        mock_get.side_effect = requests.ConnectionError("connection refused")

        with self.assertRaisesRegex(RuntimeError, "web crawl request failed"):
            fetch_web({
                "name": "site",
                "type": "web",
                "web": {
                    "urls": ["https://example.com/"],
                    "maxDepth": 0,
                    "sameDomainOnly": True,
                    "maxPages": 10,
                },
            })

    @patch("requests.get")
    def test_fetch_web_fails_on_retryable_discovered_page_error(self, mock_get):
        first = MagicMock(
            status_code=200,
            url="https://example.com/",
            text='<h1>Home</h1><a href="/docs">Docs</a>',
        )
        first.headers = {"content-type": "text/html"}
        unavailable = MagicMock(status_code=503, url="https://example.com/docs", text="")
        unavailable.headers = {"content-type": "text/html"}
        mock_get.side_effect = [first, unavailable]

        with self.assertRaisesRegex(RuntimeError, "HTTP 503"):
            fetch_web({
                "name": "site",
                "type": "web",
                "web": {
                    "urls": ["https://example.com/"],
                    "maxDepth": 1,
                    "sameDomainOnly": True,
                    "maxPages": 10,
                },
            })

    @patch("requests.get")
    def test_fetch_web_ignores_missing_discovered_page(self, mock_get):
        first = MagicMock(
            status_code=200,
            url="https://example.com/",
            text='<h1>Home</h1><a href="/removed">Removed</a>',
        )
        first.headers = {"content-type": "text/html"}
        missing = MagicMock(status_code=404, url="https://example.com/removed", text="")
        missing.headers = {"content-type": "text/html"}
        mock_get.side_effect = [first, missing]

        result = fetch_web({
            "name": "site",
            "type": "web",
            "web": {
                "urls": ["https://example.com/"],
                "maxDepth": 1,
                "sameDomainOnly": True,
                "maxPages": 10,
            },
        })

        self.assertEqual(result.docs, [("https://example.com/", "Home Removed")])

    @patch("requests.get")
    def test_fetch_web_blocks_private_address(self, mock_get):
        self.mock_resolver.return_value = {"169.254.169.254"}

        with self.assertRaisesRegex(RuntimeError, "non-public address"):
            fetch_web({
                "name": "site",
                "type": "web",
                "web": {
                    "urls": ["http://metadata.internal/"],
                    "maxDepth": 0,
                    "maxPages": 1,
                },
            })
        mock_get.assert_not_called()

    @patch("requests.get")
    def test_fetch_web_allows_private_address_with_explicit_opt_in(self, mock_get):
        self.mock_resolver.return_value = {"10.0.0.10"}
        response = MagicMock(status_code=200, url="http://docs.internal/", text="<h1>Internal docs</h1>")
        response.headers = {"content-type": "text/html"}
        mock_get.return_value = response

        result = fetch_web({
            "name": "site",
            "type": "web",
            "web": {
                "urls": ["http://docs.internal/"],
                "maxDepth": 0,
                "maxPages": 1,
                "allowPrivateNetworks": True,
            },
        })

        self.assertEqual(result.docs, [("http://docs.internal/", "Internal docs")])

    @patch("requests.get")
    def test_fetch_web_blocks_redirect_to_private_address_before_request(self, mock_get):
        self.mock_resolver.side_effect = [
            {"93.184.216.34"},
            {"169.254.169.254"},
        ]
        redirect = MagicMock(status_code=302, url="https://example.com/start", text="")
        redirect.headers = {"location": "http://metadata.internal/latest"}
        mock_get.return_value = redirect

        with self.assertRaisesRegex(RuntimeError, "non-public address"):
            fetch_web({
                "name": "site",
                "type": "web",
                "web": {
                    "urls": ["https://example.com/start"],
                    "maxDepth": 0,
                    "maxPages": 1,
                },
            })

        self.assertEqual(mock_get.call_count, 1)


if __name__ == "__main__":
    unittest.main()
