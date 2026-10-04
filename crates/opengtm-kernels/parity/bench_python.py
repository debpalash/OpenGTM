"""Python baseline for the kernel microbenchmarks (see ../BENCHMARKS.md).

Times the Python functions the kernels replace, on the same inputs as the Go
benchmarks (apps/server/internal/kernels/bench_test.go):

* normalizers: the parity reference functions over testdata/bench_corpus.json
* extraction: the team-page fixture with BeautifulSoup, which is what the
  Python scrapers use today (``html.parser`` in leadgen scrapers, ``lxml`` in
  services/scraper.py)

In-process microbenchmarks only: no network, no database.

    uv run python crates/opengtm-kernels/parity/bench_python.py
    uv run python crates/opengtm-kernels/parity/bench_python.py --write-corpus
"""

from __future__ import annotations

import argparse
import json
import platform
import random
import re
import statistics
import sys
import time
from pathlib import Path
from urllib.parse import urljoin

HERE = Path(__file__).resolve().parent
CRATE = HERE.parent
ROOT = HERE.parents[2]
sys.path.insert(0, str(ROOT))

CORPUS = CRATE / "testdata" / "bench_corpus.json"

WORDS = ["acme", "globex", "initech", "umbrella", "stark", "wayne", "hooli", "vandelay", "soylent",
         "cyberdyne", "tyrell", "wonka", "aperture", "monarch", "oscorp", "pied", "piper", "blue", "sun",
         "north", "data", "cloud", "labs", "robotics", "health", "foods", "logistics", "capital", "tech"]
TLDS = ["com", "io", "co", "ai", "de", "in", "co.uk", "com.au", "co.in", "net", "org", "fr", "nl"]
PATHS = ["", "/", "/about", "/about-us/team", "/contact?ref=dir", "/careers#open-roles", "/en/index.html"]
FIRST = ["Ada", "Alan", "Grace", "Katherine", "Linus", "Margaret", "Priya", "Rahul", "Sofia", "Wei",
         "José", "Zoë", "Søren", "Nguyễn", "Fatima", "Olu", "Hiroshi", "Anna-Maria"]
LAST = ["Lovelace", "Turing", "Hopper", "Johnson", "Torvalds", "Hamilton", "Sharma", "Gupta", "García",
        "Chen", "Müller", "O'Brien", "van der Berg", "de la Cruz", "Okafor", "Tanaka", "Smith Jr."]
TITLES = ["", "Dr. ", "Mr. ", "Ms. ", "Prof. "]


def make_corpus(seed: int = 7, n: int = 1000) -> dict:
    rng = random.Random(seed)
    domains, emails, phones, names = [], [], [], []
    for _ in range(n):
        host = "-".join(rng.sample(WORDS, rng.choice([1, 1, 2]))) + "." + rng.choice(TLDS)
        scheme = rng.choice(["", "", "http://", "https://", "HTTPS://"])
        www = rng.choice(["", "", "www.", "WWW."])
        value = f"{scheme}{www}{host}{rng.choice(PATHS)}"
        if rng.random() < 0.2:
            value = value.upper() if rng.random() < 0.5 else f"  {value} "
        domains.append(value)
        first, last = rng.choice(FIRST), rng.choice(LAST)
        local = rng.choice([first.lower(), f"{first}.{last}".lower().replace(" ", ""), f"{first[0]}{last}".lower()])
        email = f"{local}{rng.choice(['', '', '+news'])}@{host}"
        emails.append(rng.choice([email, email.upper(), f" {email} ", email.split("@")[0]]))
        phones.append(rng.choice([
            f"+1 ({rng.randint(200, 999)}) {rng.randint(200, 999)}-{rng.randint(0, 9999):04d}",
            f"+91 {rng.randint(70000, 99999)} {rng.randint(10000, 99999)}",
            f"0{rng.randint(20, 99)} {rng.randint(1000, 9999)} {rng.randint(1000, 9999)}",
            f"+44 20 {rng.randint(1000, 9999)} {rng.randint(1000, 9999)}",
            f"{rng.randint(200, 999)}.{rng.randint(200, 999)}.{rng.randint(1000, 9999)}",
        ]))
        names.append(f"{rng.choice(TITLES)}{first} {last}" + rng.choice(["", "", " PhD", "  "]))
    return {"seed": seed, "domain": domains, "email": emails, "phone": phones, "person_name": names}


def bench(fn, repeat: int = 7, number: int = 1) -> float:
    """Median seconds per call of fn() over `repeat` runs of `number` calls."""
    fn()  # warm up
    runs = []
    for _ in range(repeat):
        start = time.perf_counter()
        for _ in range(number):
            fn()
        runs.append((time.perf_counter() - start) / number)
    return statistics.median(runs)


# ── Python equivalent of the "team page members" extract case ────────────

TEAM_FIELDS_RE = re.compile(r"Phone:\s*([+\d][\d ]{6,}\d)")
_WS = re.compile(r"\s+")


def _text(el) -> str:
    return _WS.sub(" ", el.get_text(" ")).strip() if el is not None else ""


def extract_team(html: str, parser: str, base_url: str) -> list[dict]:
    from bs4 import BeautifulSoup

    soup = BeautifulSoup(html, parser)
    records = []
    for card in soup.select("article.team-member"):
        rec = {}
        if v := _text(card.select_one(".name")):
            rec["full_name"] = v
        if v := _text(card.select_one(".role")):
            rec["title"] = v
        for key, sel, attr in (("linkedin_url", "a[href*='linkedin.com']", "href"),
                               ("avatar", "img.avatar", "src"),
                               ("email", "a[href^='mailto:']", "href")):
            el = card.select_one(sel)
            if el is not None and el.get(attr, "").strip():
                rec[key] = urljoin(base_url, el[attr].strip())
        if card.get("data-id"):
            rec["member_id"] = card["data-id"]
        for script in card.select("script"):
            script.decompose()
        if m := TEAM_FIELDS_RE.search(_text(card)):
            rec["phone"] = m.group(1)
        if v := _text(card.select_one(".bio")):
            rec["bio"] = v
        records.append(rec)
    return records


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--write-corpus", action="store_true", help="regenerate testdata/bench_corpus.json")
    args = ap.parse_args()
    if args.write_corpus:
        CORPUS.write_text(json.dumps(make_corpus(), ensure_ascii=False, indent=0) + "\n", encoding="utf-8")
        print(f"wrote {CORPUS.relative_to(ROOT)}")
        return

    from apps.api.services import dedup
    from apps.api.services.entities import people
    from apps.api.services.workbook import people_search
    import bs4
    import lxml

    corpus = json.loads(CORPUS.read_text(encoding="utf-8"))

    def person_name(v):
        name = (v or "").strip()
        return name, *people_search._split_name(name)

    print(f"python {platform.python_version()} on {platform.machine()} ({platform.processor() or platform.system()})")
    print(f"beautifulsoup4 {bs4.__version__}, lxml {lxml.__version__}")
    print()
    single = "https://www.Example.com/about?x=1"
    t = bench(lambda: dedup.normalize_domain(single), number=20000)
    print(f"normalize_domain single               {t * 1e9:10.0f} ns/op")
    for kind, fn in (("domain", dedup.normalize_domain), ("email", people._email_key),
                     ("phone", dedup.normalize_phone), ("person_name", person_name)):
        values = corpus[kind]
        t = bench(lambda: [fn(v) for v in values], number=20)
        print(f"normalize {kind:<12} 1k loop         {t * 1e9:10.0f} ns/op  {t * 1e9 / len(values):8.0f} ns/value")

    cases = json.loads((CRATE / "testdata" / "extract_cases.json").read_text(encoding="utf-8"))["cases"]
    team = cases[0]
    html = (CRATE / "testdata" / team["document_file"]).read_text(encoding="utf-8")
    base = team["request"]["base_url"]
    for parser in ("html.parser", "lxml"):
        got = extract_team(html, parser, base)
        same = got == team["expected"]["records"]
        t = bench(lambda: extract_team(html, parser, base), number=200)
        print(f"extract team page bs4/{parser:<12}   {t * 1e9:10.0f} ns/op  "
              f"{len(html) / t / 1e6:6.2f} MB/s  output matches kernel fixture: {same}")


if __name__ == "__main__":
    main()
