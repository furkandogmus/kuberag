import os
import sys
import unittest
from unittest.mock import MagicMock

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from retriever.rate_limit import RedisRateLimiter


class TestRedisRateLimiter(unittest.TestCase):
    def test_uses_hashed_client_key_and_atomic_script(self):
        client = MagicMock()
        client.eval.return_value = [1, 0]
        limiter = RedisRateLimiter(
            "redis://unused",
            "tenant:retriever",
            120,
            30,
            client=client,
        )

        allowed, retry_after = limiter.consume("203.0.113.10")

        self.assertTrue(allowed)
        self.assertEqual(retry_after, 0)
        args = client.eval.call_args.args
        self.assertIn('redis.call("TIME")', args[0])
        self.assertEqual(args[1], 1)
        self.assertTrue(args[2].startswith("tenant:retriever:"))
        self.assertNotIn("203.0.113.10", args[2])
        self.assertAlmostEqual(args[3], 120 / 60_000)
        self.assertEqual(args[4], 30)
        self.assertGreaterEqual(args[5], 60)

    def test_rejection_returns_backend_retry_after(self):
        client = MagicMock()
        client.eval.return_value = [0, 4]
        limiter = RedisRateLimiter(
            "redis://unused",
            "kuberag:ratelimit",
            60,
            1,
            client=client,
        )

        self.assertEqual(limiter.consume("client"), (False, 4))

    def test_requires_url(self):
        with self.assertRaisesRegex(ValueError, "RATE_LIMIT_REDIS_URL"):
            RedisRateLimiter("", "prefix", 60, 20)


if __name__ == "__main__":
    unittest.main()
