"""OpenGTM Python plugin SDK.

A provider in a handful of lines::

    from opengtm_sdk import provider, run

    @provider
    def enrich(ctx, domain: str):
        r = ctx.fetch("https://api.example.com/v1/company", params={"domain": domain},
                      headers={"X-Api-Key": ctx.secret("EXAMPLE_API_KEY")})
        if r.status == 404:
            return None                       # no answer for this domain
        return {"company_size": r.json()["employees"]}

    if __name__ == "__main__":
        run()

See docs/plugins/python.md.
"""

from .context import Context
from .errors import (
    AuthFailed,
    Cancelled,
    FetchError,
    InvalidInput,
    NoResult,
    Permanent,
    PluginError,
    RateLimited,
    Retryable,
)
from .runtime import __version__, function, provider, run, scraper, serve, tool
from .types import Limits, Record, Response

__all__ = [
    "AuthFailed",
    "Cancelled",
    "Context",
    "FetchError",
    "InvalidInput",
    "Limits",
    "NoResult",
    "Permanent",
    "PluginError",
    "RateLimited",
    "Record",
    "Response",
    "Retryable",
    "__version__",
    "function",
    "provider",
    "run",
    "scraper",
    "serve",
    "tool",
]
