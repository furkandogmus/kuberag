"""Rate-limit backends shared by every Retriever replica."""
from __future__ import annotations

import hashlib
import math


_TOKEN_BUCKET_SCRIPT = """
local now = redis.call("TIME")
local now_ms = (tonumber(now[1]) * 1000) + math.floor(tonumber(now[2]) / 1000)
local rate_per_ms = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local ttl_seconds = tonumber(ARGV[3])

local state = redis.call("HMGET", KEYS[1], "tokens", "timestamp")
local tokens = tonumber(state[1])
local timestamp = tonumber(state[2])
if tokens == nil or timestamp == nil then
  tokens = burst
  timestamp = now_ms
end

local elapsed = math.max(0, now_ms - timestamp)
tokens = math.min(burst, tokens + (elapsed * rate_per_ms))

local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end

redis.call("HSET", KEYS[1], "tokens", tokens, "timestamp", now_ms)
redis.call("EXPIRE", KEYS[1], ttl_seconds)

local retry_after = 0
if allowed == 0 then
  retry_after = math.max(1, math.ceil((1 - tokens) / (rate_per_ms * 1000)))
end
return {allowed, retry_after}
"""


class RedisRateLimiter:
    """Atomic Redis token bucket using Redis server time."""

    def __init__(
        self,
        url: str,
        key_prefix: str,
        requests_per_minute: int,
        burst: int,
        *,
        client=None,
    ) -> None:
        if not url:
            raise ValueError("RATE_LIMIT_REDIS_URL is required for redis backend")
        if client is None:
            import redis

            client = redis.Redis.from_url(
                url,
                protocol=2,
                socket_connect_timeout=2,
                socket_timeout=2,
                health_check_interval=30,
            )
        self.client = client
        self.key_prefix = key_prefix.rstrip(":")
        self.rate_per_ms = requests_per_minute / 60_000.0
        self.burst = burst
        refill_seconds = math.ceil(burst / (requests_per_minute / 60.0))
        self.ttl_seconds = max(60, refill_seconds * 2)

    def consume(self, client_id: str) -> tuple[bool, int]:
        digest = hashlib.sha256(client_id.encode()).hexdigest()[:32]
        key = f"{self.key_prefix}:{digest}"
        result = self.client.eval(
            _TOKEN_BUCKET_SCRIPT,
            1,
            key,
            self.rate_per_ms,
            self.burst,
            self.ttl_seconds,
        )
        return bool(int(result[0])), max(0, int(result[1]))

    def close(self) -> None:
        close = getattr(self.client, "close", None)
        if close is not None:
            close()

    def ping(self) -> bool:
        return bool(self.client.ping())
