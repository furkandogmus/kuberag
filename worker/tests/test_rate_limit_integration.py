import os
import sys
import unittest
import uuid

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from retriever.rate_limit import RedisRateLimiter


@unittest.skipUnless(
    os.environ.get("REDIS_RATE_LIMIT_URL"),
    "REDIS_RATE_LIMIT_URL not configured",
)
class TestRedisRateLimiterIntegration(unittest.TestCase):
    def test_atomic_bucket_is_shared_between_limiter_instances(self):
        url = os.environ["REDIS_RATE_LIMIT_URL"]
        prefix = f"kuberag:test:{uuid.uuid4().hex}"
        first = RedisRateLimiter(url, prefix, requests_per_minute=60, burst=2)
        second = RedisRateLimiter(url, prefix, requests_per_minute=60, burst=2)
        try:
            self.assertEqual(first.consume("same-client"), (True, 0))
            self.assertEqual(second.consume("same-client"), (True, 0))
            allowed, retry_after = first.consume("same-client")
            self.assertFalse(allowed)
            self.assertGreaterEqual(retry_after, 1)
            keys = first.client.scan_iter(f"{prefix}:*")
            rendered = [key.decode() if isinstance(key, bytes) else key for key in keys]
            self.assertEqual(len(rendered), 1)
            self.assertNotIn("same-client", rendered[0])
        finally:
            for key in first.client.scan_iter(f"{prefix}:*"):
                first.client.delete(key)
            first.close()
            second.close()


if __name__ == "__main__":
    unittest.main()
