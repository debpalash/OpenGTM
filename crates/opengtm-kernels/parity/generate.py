"""Generate parity fixtures for the Rust normalization kernels.

Feeds a hand-written corpus plus a seeded random corpus through the Python
functions the API uses today and writes ``fixtures.json`` next to this file.
Rust (``cargo test``) and Go (``go test ./internal/kernels``) both assert exact
equality against it.

Run from the repository root:

    uv run python crates/opengtm-kernels/parity/generate.py

Python references (see ../README.md for why these and not the variants):

    domain       apps.api.services.dedup.normalize_domain
    email        apps.api.services.entities.people._email_key
    phone        apps.api.services.dedup.normalize_phone
    person_name  apps.api.services.workbook.people_search._row_data's
                 name.strip() + _split_name(name)

Python's "" (no value) is written as null.
"""

from __future__ import annotations

import json
import random
import sys
import unicodedata
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
sys.path.insert(0, str(ROOT))

from apps.api.services import dedup  # noqa: E402
from apps.api.services.entities import people  # noqa: E402
from apps.api.services.workbook import people_search  # noqa: E402

SEED = 20261005
RANDOM_CASES = 1500


def ref_domain(value: str):
    return dedup.normalize_domain(value) or None


def ref_email(value: str):
    return people._email_key(value) or None


def ref_phone(value: str):
    return dedup.normalize_phone(value) or None


def ref_person_name(value: str):
    name = (value or "").strip()
    first, last = people_search._split_name(name)
    return {"full": name, "first": first, "last": last}


DOMAINS = [
    "", " ", "\t\n", "example.com", "Example.COM", "www.example.com", "WWW.Example.com",
    "http://example.com", "https://example.com", "HTTPS://WWW.EXAMPLE.COM/", "https://www.example.com/about?x=1#top",
    "example.com/path/to/page", "example.com?q=1", "example.com#frag", "example.com:8080", "https://example.com:443/",
    "http://user:pass@example.com:8080/x", "user@example.com", "mailto:user@example.com", "ftp://files.example.com",
    "//example.com", "  https://example.com  ", " example.com ", "　example.com", "\x1cexample.com\x1f",
    "exa\tmple.com", "exa\nmple.com", "exa\rmple.com", "exa mple.com", "example.com.", "sub.domain.example.co.uk",
    "www.www.example.com", "wwwexample.com", "www.", "www", "http://www.", "localhost", "127.0.0.1", "http://127.0.0.1:8000",
    "[::1]", "http://[::1]:8080/", "[::1", "::1]", "http://[fe80::1%25eth0]/", "http://[fe80::1%eth0]/", "[v1.fe]",
    "[v1.]", "[vz.a]", "[1.2.3.4]", "[::ffff:1.2.3.4]", "a[::1]", "[::1]x", "[::1]:80", "[1:2:3:4:5:6:7:8:9]", "[1::2::3]",
    "[12345::]", "[::]", "[:1:2:3:4:5:6:7]", "[]", "[www.example.com]", "bücher.de", "https://Bücher.DE/über",
    "bücher.de", "xn--bcher-kva.de", "münchen.de", "例え.jp", "пример.рф", "ΣΊΣΥΦΟΣ.gr", "İstanbul.com.tr",
    "a℀b.com", "a／b.com", "a＠b.com", "a﹕b.com", "a⁇b.com", "a：b.com", "ex%41mple.com",
    "http://example.com\\path", "example.com;params", "example.com,other.com", "@example.com", "example.com@", ":8080",
    "http://:8080", "http://", "https://", "http:/example.com", "http:example.com", "htp://example.com",
    "javascript:alert(1)", "data:text/html,hi", "HTTP://EXAMPLE.COM", "hTtPs://ExAmPlE.cOm", "github.com/acme",
    "linkedin.com/company/acme", "https://acme.example/careers/", "acme.io/", "/relative/path", "?only=query",
    "#only-fragment", "123", "-example.com", "example-.com", "exa_mple.com", "e​xample.com", "e­xample.com",
    "😀.example.com", "example.com/😀", "İ.com", "x" * 300 + ".com",
    # Uppercase letters added after Unicode 15.1: Python 3.13 does not lowercase them.
    "ᲉꟋꟌ꟎꟒꟔ꟚꟜ.com", "\U00010d50\U00010d65.example", "\U00016ea0\U00016eb8.io",
    "ΑΣꟋ.gr", "ꟋΣΑ.gr",
]

EMAILS = [
    "", " ", "jane@example.com", "Jane.Doe@Example.COM", "  jane@example.com  ", " jane@example.com ",
    "jane+tag@example.com", "jane.doe+news@gmail.com", "JANE@EXAMPLE.CO.UK", "jane@localhost", "jane@", "@example.com",
    "@.", "a@b.c", "jane", "jane.example.com", "jane@@example.com", "a@b@c.com", "a@b.c@d", "jane@example.", "jane@.com",
    "jane@exa mple.com", "jane doe@example.com", "mailto:jane@example.com", "<jane@example.com>",
    "Jane Doe <jane@example.com>", "josé@bücher.de", "JOSÉ@BÜCHER.DE", "用户@例子.广告", "δοκιμή@παράδειγμα.δοκιμή",
    "ΣΊΣΥΦΟΣ@x.gr", "İnan@x.com.tr", "jane@xn--bcher-kva.de", "\x1cjane@example.com\x1f", "jane@example.com\n",
    "jane\t@example.com", "\"quoted local\"@example.com", "jane@[127.0.0.1]", "jane@[ipv6:::1]", "jane(comment)@example.com",
    "x" * 70 + "@example.com", "jane@example.com.", "jane@ex_ample.com", "jane@-example.com", "😀@emoji.example",
    "ǅane@example.com", "ﬃ@example.com", "STRASSE@straße.de", "ꟋX\U00010d50@EXAMPLE.COM",
]

PHONES = [
    "", " ", "+1 (415) 555-0132", "415.555.0132", "4155550132", "+14155550132", "001 415 555 0132", "0044 20 7946 0958",
    "+44 20 7946 0958", "+91 98765 43210", "098765 43210", "+91-98765-43210", "(022) 2345 6789", "+49 30 123456",
    "+33 1 23 45 67 89", "+81 3-1234-5678", "+86 10 1234 5678", "+61 2 1234 5678", "+55 11 91234-5678",
    "+971 4 123 4567", "+1-800-FLOWERS", "1-800-356-9377", "tel:+14155550132", "Phone: +1 415 555 0132 ext. 12",
    "+1 415 555 0132 x123", "555-0132", "12", "1", "0", "abc", "N/A", "n/a", "+", "()- ", "١٢٣٤٥٦٧٨٩٠",
    "+٩٧١ ٤ ١٢٣ ٤٥٦٧", "०९८७६ ५४३२१", "１２３４５６７８９０", "①②③", "²³", "𝟏𝟐𝟑𝟒𝟓𝟔𝟕𝟖𝟗𝟎", "+1 (415) 555-0132; +1 (415) 555-0199",
    "99999999999999999999", "+0000000000", " +1 415 555 0132", "4155550132\n", "+1\t415\t5550132",
]

PHONE_REGIONS = ["US", "IN", "GB", "DE", ""]

NAMES = [
    "", " ", "Jane", "Jane Doe", "  Jane   Doe  ", "jane doe", "JANE DOE", "Jane Q. Doe", "Jane Quinn Public Doe",
    "Dr. Jane Doe", "Dr Jane Doe PhD", "Mr. John Smith Jr.", "John Smith III", "Mrs. Jane Doe-Smith", "Prof. Dr. Hans Müller",
    "Sir Patrick Stewart", "Ms. Jane O'Brien", "Jean-Luc Picard", "Ludwig van Beethoven", "Vincent van Gogh",
    "Leonardo da Vinci", "Charles de Gaulle", "Johannes von Neumann", "Juan de la Cruz", "Ana María García López",
    "Smith, John", "Doe, Jane, MD", "Shri Ramesh Kumar", "Sri Lakshmi Narayanan", "Smt. Sunita Devi", "A. P. J. Abdul Kalam",
    "Rajesh K", "R. Kumar", "Mohammed bin Salman", "Ali ibn Abi Talib", "王小明", "王 小明", "Nguyễn Văn An", "José Ñúñez",
    "Zoë Saldaña", "Björk", "Søren Kierkegaard", "Ólafur Arnalds", "Ἀριστοτέλης", "Ivan Petrov", "Ivan Petrov",
    "Ivan　Petrov", "\x1cIvan\x1dPetrov\x1e", "Ivan​Petrov", "Ivan\tPetrov\n", "Ivan\r\nPetrov", "  \t  ",
    "Anne-Marie  Dupont", "😀 Emoji Person", "O’Neil", "McDonald", "de la Rosa", "X Æ A-12", "-", ".", "Mr.",
    "Jane Doe (she/her)", "Jane Doe | CEO", "Jane Doe - CTO at Acme", "jane@example.com",
]

ALPHABET = (
    list("abcxyzABCXYZ0123456789.-_:/?#@[]%+ ()")
    + ["www.", "http://", "https://", "WWW.", "::", "Σ", "ς", "İ", "ß", "é", "é", "ü", "例", "р", "😀",
       "\t", "\n", "\r", "\x0b", "\x0c", "\x1c", "\x1f", "\x85", " ", " ", " ", "　", "​",
       "٠", "٩", "०", "０", "９", "²", "①", "\U0001d7ce",
       "℀", "／", "＠", "﹕", "⁇", ".com", ".co.uk", "@", "@x.io", "v1.", "fe80", "%25",
       "Ɤ", "\U00010d50"]
)


def random_corpus(rng: random.Random, n: int) -> list[str]:
    out = []
    for _ in range(n):
        length = rng.randint(0, 14)
        out.append("".join(rng.choice(ALPHABET) for _ in range(length)))
    return out


def dedupe(values):
    return list(dict.fromkeys(values))


def main() -> None:
    rng = random.Random(SEED)
    rand = random_corpus(rng, RANDOM_CASES)

    domains = dedupe(DOMAINS + EMAILS[:10] + rand)
    emails = dedupe(EMAILS + DOMAINS[:20] + rand)
    phones = dedupe(PHONES + rand)
    names = dedupe(NAMES + rand)

    phone_cases = [{"input": v, "expected": ref_phone(v)} for v in phones]
    # default_region is accepted but has no Python counterpart; it must not change results.
    phone_cases += [
        {"input": v, "default_region": region, "expected": ref_phone(v)}
        for v in PHONES[:12] for region in PHONE_REGIONS
    ]

    fixtures = {
        "meta": {
            "generated_by": "crates/opengtm-kernels/parity/generate.py",
            "python": sys.version.split()[0],
            "unicode": unicodedata.unidata_version,
            "seed": SEED,
            "references": {
                "domain": "apps/api/services/dedup.py::normalize_domain",
                "email": "apps/api/services/entities/people.py::_email_key",
                "phone": "apps/api/services/dedup.py::normalize_phone",
                "person_name": "apps/api/services/workbook/people_search.py::_split_name (+ name.strip())",
            },
        },
        "domain": [{"input": v, "expected": ref_domain(v)} for v in domains],
        "email": [{"input": v, "expected": ref_email(v)} for v in emails],
        "phone": phone_cases,
        "person_name": [{"input": v, "expected": ref_person_name(v)} for v in names],
    }
    path = HERE / "fixtures.json"
    path.write_text(json.dumps(fixtures, ensure_ascii=False, indent=1) + "\n", encoding="utf-8")
    counts = {k: len(v) for k, v in fixtures.items() if k != "meta"}
    print(f"wrote {path.relative_to(ROOT)}: {counts}")


if __name__ == "__main__":
    main()
