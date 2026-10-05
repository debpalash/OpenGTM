"""Smallest SDK plugin; also used to check the missing-SDK diagnostic."""

from opengtm_sdk import provider, run


@provider
def enrich(ctx, mode: str = "ok"):
    return {"mode": mode}


if __name__ == "__main__":
    run()
