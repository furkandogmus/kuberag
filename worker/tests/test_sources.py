import unittest
from unittest.mock import MagicMock, patch
from pathlib import Path
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from rag_worker.sources import _read_pdf, _read_pdf_bytes


class TestSources(unittest.TestCase):
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


if __name__ == "__main__":
    unittest.main()
