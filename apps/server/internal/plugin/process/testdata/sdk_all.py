"""One plugin file exercising every SDK entry point; the manifest kind picks the handler."""

import asyncio
import time

from opengtm_sdk import NoResult, Record, Retryable, function, provider, run, scraper, tool


@provider
def enrich(ctx, domain: str, mode: str = "ok"):
    ctx.log(f"looking up {domain}")
    if mode == "ok":
        r = ctx.fetch(
            "https://api.example.com/v1/companies",
            params={"domain": domain},
            headers={"X-Api-Key": ctx.secret("API_KEY")},
        )
        ctx.progress(pages=1, message="fetched")
        ctx.add_cost(0.01)
        return {"status": r.status, "domain": domain, "body": r.json()}
    if mode == "none":
        return None
    if mode == "noresult":
        raise NoResult("not_found")
    if mode == "retryable":
        raise Retryable("upstream is down")
    if mode == "boom":
        raise ValueError("kaput")
    if mode == "denied":
        ctx.fetch("https://evil.example.net/")
    if mode == "timeout_fetch":
        ctx.fetch("https://api.example.com/slow")
    if mode == "badjson":
        return {"x": object()}
    if mode == "sleep":
        ctx.log("ready")
        time.sleep(60)
    if mode == "spin":
        ctx.log("ready")
        while True:  # pure-Python busy loop: only an interrupt can stop it
            pass
    if mode == "secret":
        return {"has_secret": bool(ctx.secret("API_KEY")), "inputs": dict(ctx.inputs)}
    return {"mode": mode}


@scraper
async def scrape(ctx, n: int = 3, pad: int = 0, mode: str = "ok"):
    if mode == "asleep":
        ctx.log("ready")
        await asyncio.sleep(60)
    for i in range(n):
        ctx.check_cancelled()
        if i == 0:
            await ctx.afetch("https://api.example.com/v1/page")
        yield Record({"i": i, "pad": "x" * pad}, evidence=ctx.evidence("https://api.example.com/v1/page", index=i))
        if i % 100 == 0:
            ctx.progress(records=i)
    ctx.stopped = "done"


@function
def double(ctx, x: int):
    return x * 2


@tool
def lookup(ctx, q: str):
    return {"answer": q.upper()}


if __name__ == "__main__":
    run()
