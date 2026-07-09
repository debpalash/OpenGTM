# CRM integrations — HubSpot, Salesforce, etc.
"""Shared CRM HTTP helpers.

`request_with_retry` gives both adapters the same bounded 429 handling: honor
Retry-After when the CRM sends it (capped so a hostile/buggy header can't hang
a run), exponential fallback otherwise, and give up after `max_retries` extra
attempts — returning the last response so callers surface a clean per-run
error instead of raising.
"""

import asyncio

# Never sleep longer than this per retry, whatever Retry-After says.
MAX_RETRY_SLEEP_SECONDS = 15.0


async def request_with_retry(client, method: str, url: str, *,
                             max_retries: int = 3, **kwargs):
    """Issue an httpx request, retrying (bounded) on HTTP 429.

    Returns the final ``httpx.Response`` (possibly still a 429 after the
    retries are exhausted). Network errors propagate to the caller, which
    already wraps CRM calls in try/except and reports per-run errors.
    """
    resp = None
    for attempt in range(max_retries + 1):
        resp = await client.request(method, url, **kwargs)
        if resp.status_code != 429 or attempt == max_retries:
            return resp
        retry_after = resp.headers.get("Retry-After", "")
        try:
            delay = float(retry_after)
        except (TypeError, ValueError):
            delay = float(2 ** attempt)
        await asyncio.sleep(min(max(delay, 0.0), MAX_RETRY_SLEEP_SECONDS))
    return resp
