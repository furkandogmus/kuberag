import importlib.util
import sys
import unittest
from pathlib import Path
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[2] / "hack" / "load-test.py"
SPEC = importlib.util.spec_from_file_location("load_test", SCRIPT)
load_test = importlib.util.module_from_spec(SPEC)
sys.modules["load_test"] = load_test
SPEC.loader.exec_module(load_test)


class TestLoadTest(unittest.TestCase):
    def test_concurrent_load_summary(self):
        with patch.object(load_test, "request_once", return_value=(200, 0.01)):
            result = load_test.run_load(
                "http://retriever/query",
                "hello",
                requests=20,
                concurrency=4,
            )
        self.assertEqual(result["statusCounts"], {"200": 20})
        self.assertEqual(result["errorRate"], 0)
        self.assertGreater(result["requestsPerSecond"], 0)
        self.assertGreaterEqual(result["latencyMillis"]["p99"], 0)

    def test_percentile_uses_nearest_rank(self):
        self.assertEqual(load_test.percentile([1, 2, 3, 4], 95), 4)


if __name__ == "__main__":
    unittest.main()
