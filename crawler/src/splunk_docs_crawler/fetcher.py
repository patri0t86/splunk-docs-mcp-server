"""Rate-limited, retrying HTTP fetcher with robots.txt compliance."""

from __future__ import annotations

import asyncio
import logging
import time
import urllib.robotparser
from urllib.parse import urlsplit

import httpx

log = logging.getLogger(__name__)

RETRYABLE_STATUSES = {429, 500, 502, 503, 504}


class RateLimiter:
    """Enforces a minimum interval between requests across all workers."""

    def __init__(self, per_second: float):
        self._interval = 1.0 / per_second
        self._lock = asyncio.Lock()
        self._next_slot = 0.0

    async def wait(self) -> None:
        async with self._lock:
            now = time.monotonic()
            if now < self._next_slot:
                await asyncio.sleep(self._next_slot - now)
                now = time.monotonic()
            self._next_slot = max(now, self._next_slot) + self._interval


class Fetcher:
    def __init__(self, user_agent: str, rate_limit_per_sec: float, timeout: float, retries: int):
        self.user_agent = user_agent
        self.retries = retries
        self._limiter = RateLimiter(rate_limit_per_sec)
        self._client = httpx.AsyncClient(
            headers={"User-Agent": user_agent},
            timeout=timeout,
            follow_redirects=True,
        )
        self._robots: dict[str, urllib.robotparser.RobotFileParser] = {}

    async def __aenter__(self) -> "Fetcher":
        return self

    async def __aexit__(self, *exc) -> None:
        await self._client.aclose()

    async def allowed_by_robots(self, url: str) -> bool:
        parts = urlsplit(url)
        origin = f"{parts.scheme}://{parts.netloc}"
        parser = self._robots.get(origin)
        if parser is None:
            parser = urllib.robotparser.RobotFileParser()
            try:
                response = await self._client.get(f"{origin}/robots.txt")
                if response.status_code == 200:
                    parser.parse(response.text.splitlines())
                else:
                    parser.parse([])  # no robots.txt -> everything allowed
            except httpx.HTTPError:
                log.warning("could not fetch robots.txt for %s; assuming allowed", origin)
                parser.parse([])
            self._robots[origin] = parser
        return parser.can_fetch(self.user_agent, url)

    async def get(self, url: str) -> httpx.Response:
        """GET with rate limiting and exponential backoff on transient failures."""
        last_error: Exception | None = None
        for attempt in range(self.retries + 1):
            await self._limiter.wait()
            try:
                response = await self._client.get(url)
            except httpx.HTTPError as exc:
                last_error = exc
                delay = 2.0**attempt
                log.warning("%s: %s (retry in %.0fs)", url, exc, delay)
                await asyncio.sleep(delay)
                continue

            if response.status_code in RETRYABLE_STATUSES and attempt < self.retries:
                retry_after = response.headers.get("Retry-After")
                delay = float(retry_after) if retry_after and retry_after.isdigit() else 2.0**attempt
                log.warning("%s: HTTP %d (retry in %.0fs)", url, response.status_code, delay)
                await asyncio.sleep(delay)
                continue
            return response

        raise RuntimeError(f"failed to fetch {url} after {self.retries + 1} attempts") from last_error
