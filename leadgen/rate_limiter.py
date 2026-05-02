"""
Rate Limiter — Per-domain token bucket with circuit breaker.

Prevents getting blocked by throttling requests per target domain.
Circuit breaker pauses all requests to a domain after consecutive failures.
"""

import asyncio
import time
from collections import defaultdict
from typing import Optional


# Default rate limits per domain (requests per minute)
DOMAIN_LIMITS = {
    "google.com": 3,
    "google.co.in": 3,
    "maps.google.com": 3,
    "linkedin.com": 2,
    "yelp.com": 5,
    "glassdoor.com": 3,
    "naukri.com": 5,
    "indeed.com": 5,
    "clutch.co": 5,
    "g2.com": 5,
}

DEFAULT_RPM = 10
CIRCUIT_BREAKER_THRESHOLD = 5       # consecutive failures to trip
CIRCUIT_BREAKER_RECOVERY = 900      # seconds (15 min)


class _DomainBucket:
    """Token bucket for a single domain."""

    __slots__ = ("rpm", "tokens", "last_refill", "failures", "tripped_until")

    def __init__(self, rpm: int):
        self.rpm = rpm
        self.tokens = rpm
        self.last_refill = time.monotonic()
        self.failures = 0
        self.tripped_until: Optional[float] = None

    def _refill(self):
        now = time.monotonic()
        elapsed = now - self.last_refill
        refill = elapsed * (self.rpm / 60.0)
        self.tokens = min(self.rpm, self.tokens + refill)
        self.last_refill = now

    def is_tripped(self) -> bool:
        if self.tripped_until is None:
            return False
        if time.monotonic() > self.tripped_until:
            # Recovery
            self.tripped_until = None
            self.failures = 0
            return False
        return True

    def wait_time(self) -> float:
        """Seconds to wait before next request is allowed."""
        if self.is_tripped():
            return max(0, self.tripped_until - time.monotonic())
        self._refill()
        if self.tokens >= 1:
            return 0
        return (1 - self.tokens) / (self.rpm / 60.0)

    def consume(self) -> bool:
        """Try to consume a token. Returns True if allowed."""
        if self.is_tripped():
            return False
        self._refill()
        if self.tokens >= 1:
            self.tokens -= 1
            return True
        return False


class RateLimiter:
    """Per-domain rate limiter with circuit breaker."""

    def __init__(self, domain_limits: Optional[dict] = None):
        self._limits = {**DOMAIN_LIMITS, **(domain_limits or {})}
        self._buckets: dict[str, _DomainBucket] = {}

    def _get_bucket(self, domain: str) -> _DomainBucket:
        if domain not in self._buckets:
            # Find matching limit — check if domain ends with a known key
            rpm = DEFAULT_RPM
            for pattern, limit in self._limits.items():
                if domain == pattern or domain.endswith("." + pattern):
                    rpm = limit
                    break
            self._buckets[domain] = _DomainBucket(rpm)
        return self._buckets[domain]

    async def acquire(self, domain: str):
        """Wait until a request to this domain is allowed."""
        bucket = self._get_bucket(domain)
        while True:
            if bucket.consume():
                return
            wait = bucket.wait_time()
            if wait > 0:
                await asyncio.sleep(min(wait, 5.0))

    def try_acquire(self, domain: str) -> bool:
        """Non-blocking attempt to acquire a token."""
        return self._get_bucket(domain).consume()

    def report_failure(self, domain: str):
        """Report a failed request — may trip circuit breaker."""
        bucket = self._get_bucket(domain)
        bucket.failures += 1
        if bucket.failures >= CIRCUIT_BREAKER_THRESHOLD:
            bucket.tripped_until = time.monotonic() + CIRCUIT_BREAKER_RECOVERY
            bucket.failures = 0

    def report_success(self, domain: str):
        """Report a successful request — resets failure counter."""
        bucket = self._get_bucket(domain)
        bucket.failures = 0

    def is_blocked(self, domain: str) -> bool:
        """Check if circuit breaker is tripped for this domain."""
        return self._get_bucket(domain).is_tripped()

    def stats(self) -> dict:
        """Return limiter statistics."""
        tripped = [d for d, b in self._buckets.items() if b.is_tripped()]
        return {
            "tracked_domains": len(self._buckets),
            "tripped_domains": tripped,
        }
