"""Tests for the retriever FastAPI server (request validation, metadata filtering, and conversational prompts)."""
import os
import sys
import unittest
from unittest.mock import MagicMock, patch

# Add worker directory to path
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

# Set dummy environment variables to satisfy setup imports
os.environ["VECTORSTORE_TYPE"] = "qdrant"
os.environ["VECTORSTORE_ENDPOINT"] = "http://localhost:6333"
os.environ["EMBEDDING_MODEL"] = "all-MiniLM-L6-v2"
os.environ["GEN_ENABLED"] = "true"
os.environ["GEN_MODEL"] = "gpt-4"

from fastapi.testclient import TestClient
import retriever.server as server


class TestRetrieverServer(unittest.TestCase):
    def setUp(self):
        self.client = TestClient(server.app)

        # Mock dependencies in server
        self.mock_embedder = MagicMock()
        self.mock_store = MagicMock()
        self.mock_gen_client = MagicMock()

        self.patcher_ensure = patch("retriever.server._ensure", lambda: None)
        self.patcher_ensure.start()

        # Inject mocks
        server._embedder = self.mock_embedder
        server._store = self.mock_store
        server._gen_client = self.mock_gen_client
        server._GEN_ENABLED = True
        server._RERANK = False

    def tearDown(self):
        self.patcher_ensure.stop()
        server._embedder = None
        server._store = None
        server._gen_client = None

    def test_query_with_source_filter(self):
        # Mock embedder output
        self.mock_embedder.embed_query.return_value = [0.1, 0.2, 0.3]

        # Mock store search output
        self.mock_store.search.return_value = [
            {
                "score": 0.9,
                "payload": {
                    "text": "Antigravity is code-name for a developer agent.",
                    "source": "docs",
                    "doc_path": "docs/intro.md",
                }
            }
        ]

        # Mock generator output
        mock_choice = MagicMock()
        mock_choice.message.content = "Antigravity is a developer agent."
        self.mock_gen_client.chat.completions.create.return_value.choices = [mock_choice]

        # Post query request with a source filter
        payload = {
            "query": "What is Antigravity?",
            "topK": 3,
            "source": "docs"
        }
        resp = self.client.post("/query", json=payload)
        self.assertEqual(resp.status_code, 200)

        # Verify output structure
        data = resp.json()
        self.assertEqual(data["query"], "What is Antigravity?")
        self.assertEqual(len(data["results"]), 1)
        self.assertEqual(data["results"][0]["source"], "docs")
        self.assertEqual(data["answer"], "Antigravity is a developer agent.")

        # Verify search filter propagation
        self.mock_store.search.assert_called_once_with([0.1, 0.2, 0.3], 3, source="docs")

    def test_query_with_history(self):
        self.mock_embedder.embed_query.return_value = [0.1, 0.2, 0.3]
        self.mock_store.search.return_value = [
            {
                "score": 0.85,
                "payload": {
                    "text": "It can edit files and run commands.",
                    "source": "docs",
                    "doc_path": "docs/features.md",
                }
            }
        ]

        mock_choice = MagicMock()
        mock_choice.message.content = "It edits files and runs commands."
        self.mock_gen_client.chat.completions.create.return_value.choices = [mock_choice]

        # Post query request with history
        payload = {
            "query": "What can it do?",
            "history": [
                {"role": "user", "content": "What is Antigravity?"},
                {"role": "assistant", "content": "It is a developer agent."}
            ]
        }
        resp = self.client.post("/query", json=payload)
        self.assertEqual(resp.status_code, 200)

        # Verify chat completion messages structure contains the history
        self.mock_gen_client.chat.completions.create.assert_called_once()
        kwargs = self.mock_gen_client.chat.completions.create.call_args[1]
        messages = kwargs["messages"]

        # Expecting: system prompt, user turn 1, assistant turn 1, and current user question with context
        self.assertEqual(len(messages), 4)
        self.assertEqual(messages[0]["role"], "system")
        self.assertEqual(messages[1], {"role": "user", "content": "What is Antigravity?"})
        self.assertEqual(messages[2], {"role": "assistant", "content": "It is a developer agent."})
        self.assertTrue("Context:\n[docs/features.md]" in messages[3]["content"])
        self.assertTrue("Question: What can it do?" in messages[3]["content"])


if __name__ == "__main__":
    unittest.main()
