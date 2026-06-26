#!/usr/bin/env python3
"""Reproducibly build the bundled tech-fingerprint DB from a PERMISSIVE upstream.

Why this exists
---------------
The website tech detector (``providers/tech_stack_provider.py``) matches a
bundled fingerprint DB (``enrichment/data/tech_fingerprints.json``). That blob
must be (a) license-clean and (b) auditable / re-buildable — never an
unattributed scrape.

License decision (see the co-located NOTICE):
  * The original Wappalyzer dataset was relicensed to **GPL-3.0** and later went
    commercial; the popular continuation forks (``enthec/webappanalyzer``,
    ``dochne/wappalyzer``, ``tunetheweb/wappalyzer``) are all **GPL-3.0**.
    GPL-3.0 is copyleft and unsafe to bundle in Yupcha, so we do NOT use them.
  * We pin **developit/wappalyzer**, which carries the original **MIT** license
    ("Copyright 2008 Wappalyzer"), at a fixed commit SHA. MIT is permissive and
    safe to redistribute with attribution.

This script downloads the pinned upstream ``src/categories.json`` +
``src/technologies/*.json``, compiles them to this provider's flat format, and
writes both ``tech_fingerprints.json`` and the ``tech_fingerprints.NOTICE``
attribution file. Run it to regenerate / re-audit the bundle:

    uv run python scripts/build_tech_fingerprints.py

It is dev-only (manual / CI), never imported at runtime, and performs the only
network access in this feature outside the guarded homepage fetch.
"""

from __future__ import annotations

import json
import re
import sys
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

# ── Pinned, provably-permissive upstream ─────────────────────────────────────
UPSTREAM_REPO = "developit/wappalyzer"
# developit/wappalyzer @ master, 2021-09-14 (the original MIT-licensed lineage).
UPSTREAM_SHA = "b502633885dfea2221ae0e87a275a8120072afa7"
UPSTREAM_LICENSE = "MIT"
RAW = f"https://raw.githubusercontent.com/{UPSTREAM_REPO}/{UPSTREAM_SHA}"

_OUT_DIR = Path(__file__).resolve().parent.parent / (
    "apps/api/services/leadgen/enrichment/data"
)
_OUT_JSON = _OUT_DIR / "tech_fingerprints.json"
_OUT_NOTICE = _OUT_DIR / "tech_fingerprints.NOTICE"

_TECH_FILES = ["_"] + [chr(c) for c in range(ord("a"), ord("z") + 1)]


def _fetch_json(path: str) -> dict:
    url = f"{RAW}/{path}"
    with urllib.request.urlopen(url, timeout=30) as resp:  # noqa: S310 (pinned host)
        return json.loads(resp.read().decode("utf-8"))


def _as_list(v) -> list:
    if v is None:
        return []
    return v if isinstance(v, list) else [v]


def _clean_pattern(p: str) -> str:
    """Drop Wappalyzer's ``\\;version:..`` / ``\\;confidence:..`` tag suffix."""
    return str(p).split("\\;", 1)[0]


def _valid_regex(p: str) -> bool:
    try:
        re.compile(p, re.I)
        return True
    except re.error:
        return False


def build() -> dict:
    categories = _fetch_json("src/categories.json")
    cat_name = {str(k): (v.get("name") or "Other") for k, v in categories.items()}

    techs: dict = {}
    for f in _TECH_FILES:
        techs.update(_fetch_json(f"src/technologies/{f}.json"))

    out: dict = {}
    for name, spec in techs.items():
        if not isinstance(spec, dict):
            continue
        cats = spec.get("cats") or []
        cat = cat_name.get(str(cats[0]), "Other") if cats else "Other"

        # html signal = html + scripts + css regexes (all match against the
        # lowercased response body the provider scans). dom/js/text/url/dns need
        # a JS engine or a different surface — the provider can't use them.
        html = []
        for key in ("html", "scripts", "css"):
            for p in _as_list(spec.get(key)):
                cp = _clean_pattern(p)
                if cp and _valid_regex(cp):
                    html.append(cp)

        headers = {}
        for hk, hv in (spec.get("headers") or {}).items():
            cp = _clean_pattern(hv) or "."
            if _valid_regex(cp):
                headers[hk] = cp

        meta = {}
        for mk, mv in (spec.get("meta") or {}).items():
            cp = _clean_pattern(mv) or "."
            if _valid_regex(cp):
                meta[mk] = cp

        # provider only checks cookie-NAME presence → keep keys, drop patterns.
        cookies = [str(c) for c in (spec.get("cookies") or {}).keys()]

        implies = [_clean_pattern(i) for i in _as_list(spec.get("implies"))]
        implies = [i for i in implies if i]

        if not (html or headers or meta or cookies or implies):
            continue  # un-detectable, un-referenced noise

        entry: dict = {"cat": cat}
        if implies:
            entry["implies"] = implies
        if headers:
            entry["headers"] = headers
        if html:
            entry["html"] = html
        if meta:
            entry["meta"] = meta
        if cookies:
            entry["cookies"] = cookies
        out[name] = entry

    return out


def write_notice(entry_count: int) -> None:
    today = datetime.now(timezone.utc).strftime("%Y-%m-%d")
    _OUT_NOTICE.write_text(
        f"""tech_fingerprints.json — attribution & provenance
==================================================

This file is a COMPILED DERIVATIVE of the Wappalyzer technology-fingerprint
dataset, reduced to the flat schema consumed by
``apps/api/services/leadgen/enrichment/providers/tech_stack_provider.py``.

Upstream source : https://github.com/{UPSTREAM_REPO}
Pinned commit   : {UPSTREAM_SHA}
Upstream license: {UPSTREAM_LICENSE} ("Copyright 2008 Wappalyzer")
Retrieved       : {today}
Entries         : {entry_count}
Regenerate with : uv run python scripts/build_tech_fingerprints.py

License note
------------
developit/wappalyzer preserves the ORIGINAL MIT-licensed Wappalyzer dataset.
MIT is permissive and safe to redistribute with this attribution. We
deliberately do NOT use the newer/larger continuation forks
(enthec/webappanalyzer, dochne/wappalyzer, tunetheweb/wappalyzer) because those
are GPL-3.0 (copyleft) and unsafe to bundle in Yupcha.

Transform applied by the regen script
-------------------------------------
  * cats[0] -> readable category name (via upstream src/categories.json)
  * html = html + scripts + css regexes (the surfaces the provider scans)
  * headers / meta kept as {{name: regex}}; cookies reduced to name list
  * Wappalyzer "\\;version:.." / "\\;confidence:.." pattern tags stripped
  * regexes that fail to compile are dropped individually
  * js / dom / text / url / dns / certIssuer signals dropped (need a JS engine
    or a surface this passive provider does not fetch)

MIT LICENSE (developit/wappalyzer)
----------------------------------
Copyright 2008 Wappalyzer

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies
of the Software, and to permit persons to whom the Software is furnished to do
so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
""",
        encoding="utf-8",
    )


def main() -> int:
    print(f"Building fingerprints from {UPSTREAM_REPO}@{UPSTREAM_SHA[:10]} ...")
    db = build()
    _OUT_DIR.mkdir(parents=True, exist_ok=True)
    _OUT_JSON.write_text(
        json.dumps(db, ensure_ascii=False, separators=(",", ":"), sort_keys=True),
        encoding="utf-8",
    )
    write_notice(len(db))
    print(f"Wrote {len(db)} entries -> {_OUT_JSON}")
    print(f"Wrote attribution -> {_OUT_NOTICE}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
