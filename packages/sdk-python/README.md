# opengtm-sdk

Write [OpenGTM](https://github.com/debpalash/OpenGTM) provider, scraper, function
and tool plugins in Python. No runtime dependencies; Python 3.10 or newer; the
host runs your code in a separate, supervised process (Linux and macOS).

```python
from opengtm_sdk import provider, run


@provider
def enrich(ctx, domain: str):
    r = ctx.fetch("https://api.acme-data.example/v1/companies", params={"domain": domain},
                  headers={"X-Api-Key": ctx.secret("ACME_DATA_API_KEY")})
    if r.status == 404:
        return None                      # no answer for this domain: not a failure
    return {"company_size": r.json()["company"]["employees"]}


if __name__ == "__main__":
    run()
```

```bash
opengtm plugin new provider acme_enricher --runtime process   # manifest, main.py, fixtures
pip install opengtm-sdk
opengtm plugin test acme_enricher                             # offline, against recorded fixtures
```

- `@provider`, `@scraper` (sync or async generators of dicts or `Record`),
  `@function`, `@tool`.
- `ctx.fetch` / `ctx.afetch` reach the network only through the host's guarded
  client (declared hosts, SSRF guard, robots.txt, rate limits); `ctx.secret`
  returns only declared secrets; `ctx.log`, `ctx.progress`, `ctx.add_cost`,
  `ctx.cancelled`.
- `Retryable`, `RateLimited`, `InvalidInput`, `AuthFailed`, `Permanent`,
  `NoResult` report structured failures; the queue retries only the transient
  ones.

Guide: [docs/plugins/python.md](../../docs/plugins/python.md). Wire protocol:
[docs/plugins/process-abi.md](../../docs/plugins/process-abi.md), with a JSON
Schema in `packages/contracts/plugin-process.v1.schema.json`.

## Developing the SDK

```bash
uv run --no-project --with pytest --with jsonschema pytest packages/sdk-python/tests
```

The tests drive the SDK against a fake host over a socket pair and validate every
frame in both directions against the contract schema. The Go supervisor's tests
(`apps/server/internal/plugin/process`) run the same SDK against the real host.
