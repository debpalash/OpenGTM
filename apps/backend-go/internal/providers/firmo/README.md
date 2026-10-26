# providers/firmo

Go ports of the firmographic and registry HTTP providers (see register.go).

## Egress notes
- Every vendor request goes through the caller's egress client (dial-time IP check).
  gleif, sec_edgar, ats_hiring, jsonld_firmographics and tech_stack walk redirects
  themselves (`guardedGet`): each hop is URL-checked, carries the provider's headers,
  limiter and body cap, and is bounded (httpx's 20 hops; jsonld 6 and tech_stack 3 as in Python).
  A 3xx without `Location` is returned as is (a miss), as httpx does; tech_stack alone ends
  with "too many redirects", as its Python `break` does.
- ipinfo resolves the lead's domain with the system resolver (`lookupIPv4`, like Python's
  `socket.gethostbyname`). This is a DNS query only, outside the egress guard; the resulting
  IP is sent to ipinfo.io as a path segment, never connected to.
- SEC company_tickers.json (about 1 MB) is read with a 4 MiB cap (`providers.Req.MaxBody`);
  all other bodies keep the 1 MiB default.
