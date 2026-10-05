"""Company domain -> firmographics, written with the OpenGTM Python SDK."""

from opengtm_sdk import Retryable, provider, run


def size_band(employees: int) -> str:
    for limit, name in ((10, "1-10"), (50, "11-50"), (200, "51-200"), (1000, "201-1000")):
        if employees <= limit:
            return name
    return "1000+"


@provider
def enrich(ctx, domain: str):
    r = ctx.fetch(
        "https://api.acme-data.example/v1/companies",
        params={"domain": domain},
        headers={"X-Api-Key": ctx.secret("ACME_DATA_API_KEY")},
    )
    if r.status == 404:
        return None  # the API does not know this domain: a normal "no result"
    body = r.json()
    if body.get("error"):  # the vendor reports quota problems inside a 200
        raise Retryable(body.get("error_message", "vendor error"))
    company = body["company"]
    return {
        "company_size": company["employees"],
        "size_band": size_band(company["employees"]),
        "industry": company["industry"],
        "linkedin_url": f"https://www.linkedin.com/company/{company['linkedin_handle']}",
    }


if __name__ == "__main__":
    run()
