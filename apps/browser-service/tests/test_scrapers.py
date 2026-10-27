import pytest
from conftest import AUTH

from browser_service.scrapers import google_maps, justdial


class Recorder:
    def __init__(self, results=None, error=None):
        self.calls = []
        self.results = (
            results
            if results is not None
            else [
                {
                    "title": "Acme Staffing | LinkedIn",
                    "href": "https://www.linkedin.com/company/acme",
                    "body": "500 employees",
                },
            ]
        )
        self.error = error

    async def __call__(self, deps, method, search, max_results, deadline):
        self.calls.append((method, search, max_results))
        if self.error is not None:
            raise self.error
        return self.results


async def _post(make_client, path, body, rec=None):
    rec = rec or Recorder()
    c = await make_client(search_fn=rec)
    r = await c.post(path, headers=AUTH, json={"delay_s": 0, **body})
    return r, rec


async def test_linkedin_companies_uses_legacy_search_strings(make_client):
    r, rec = await _post(
        make_client, "/v1/scrape/linkedin", {"queries": ["staffing company", "HR agency"], "cities": ["Pune", "Delhi"]}
    )
    assert r.status_code == 200, r.text
    assert [c[1] for c in rec.calls] == [
        'site:linkedin.com/company "staffing company" "Pune" India',
        'site:linkedin.com/company "HR agency" "Pune" India',
        'site:linkedin.com/company "staffing company" "Delhi" India',
        'site:linkedin.com/company "HR agency" "Delhi" India',
    ]
    assert all(c[0] == "text" and c[2] == 10 for c in rec.calls)
    run = r.json()["runs"][0]
    assert run["variant"] == "companies" and run["query"] == "staffing company" and run["city"] == "Pune"
    assert run["hits"][0] == {
        "title": "Acme Staffing | LinkedIn",
        "url": "https://www.linkedin.com/company/acme",
        "body": "500 employees",
        "date": "",
        "source": "",
    }
    assert run["error"] is None and r.json()["partial"] is False


async def test_linkedin_people_defaults(make_client):
    r, rec = await _post(make_client, "/v1/scrape/linkedin", {"mode": "people", "company_name": "Acme"})
    assert r.status_code == 200
    assert [c[1] for c in rec.calls][0] == 'site:linkedin.com/in "Acme" "CEO" India'
    assert len(rec.calls) == 6 and all(c[2] == 2 for c in rec.calls)
    assert r.json()["runs"][1]["term"] == "CTO"
    r, _ = await _post(make_client, "/v1/scrape/linkedin", {"mode": "people"})
    assert r.status_code == 400


@pytest.mark.parametrize(
    "board,expected,n",
    [
        ("naukri", 'site:naukri.com "q" "c" company reviews hiring', 15),
        ("indeed", 'site:indeed.co.in/cmp "q" "c"', 15),
        ("foundit", 'site:foundit.in "q" "c" company', 10),
    ],
)
async def test_job_boards(make_client, board, expected, n):
    r, rec = await _post(make_client, "/v1/scrape/job_boards", {"board": board, "queries": ["q"], "cities": ["c"]})
    assert r.status_code == 200 and rec.calls == [("text", expected, n)]


async def test_crunchbase(make_client):
    r, rec = await _post(make_client, "/v1/scrape/crunchbase", {"queries": ["q"], "cities": ["c"], "max_results": 3})
    assert rec.calls == [("text", "q c site:crunchbase.com/organization", 3)]


@pytest.mark.parametrize(
    "site,body,expected",
    [
        ("clutch", {}, ['site:clutch.co "q" India staffing HR']),
        ("goodfirms", {}, ['site:goodfirms.co "q" India']),
        ("g2", {}, ['site:g2.com "q" India staffing recruitment']),
        (
            "ambitionbox",
            {"cities": ["c1", "c2"]},
            ['site:ambitionbox.com "q" "c1" reviews', 'site:ambitionbox.com "q" "c2" reviews'],
        ),
    ],
)
async def test_review_directories(make_client, site, body, expected):
    r, rec = await _post(make_client, "/v1/scrape/review_directories", {"site": site, "queries": ["q"], **body})
    assert r.status_code == 200 and [c[1] for c in rec.calls] == expected


async def test_review_directories_ambitionbox_needs_cities(make_client):
    r, _ = await _post(make_client, "/v1/scrape/review_directories", {"site": "ambitionbox", "queries": ["q"]})
    assert r.status_code == 400


async def test_web_directories_keeps_legacy_order(make_client):
    r, rec = await _post(
        make_client,
        "/v1/scrape/web_directories",
        {"queries": ["q"], "cities": ["c"], "sites": ["indiamart", "justdial"]},
    )
    assert [c[1] for c in rec.calls] == ["q c site:justdial.com", "q c site:indiamart.com"]
    assert [run["variant"] for run in r.json()["runs"]] == ["justdial", "indiamart"]


async def test_google_search_web_and_news(make_client):
    r, rec = await _post(make_client, "/v1/scrape/google_search", {"queries": ["q"], "cities": ["c"]})
    assert rec.calls == [("text", '"q" "c" India contact email phone', 15)]
    news = Recorder(
        results=[
            {
                "title": "T",
                "url": "https://n.example/a",
                "body": "B",
                "date": "2026-01-01",
                "source": "Wire",
                "image": "x",
            }
        ]
    )
    r, rec = await _post(make_client, "/v1/scrape/google_search", {"mode": "news", "queries": ["q"]}, news)
    assert rec.calls == [("news", '"q" India funding OR expansion OR launched OR partnership 2025 2026', 20)]
    assert r.json()["runs"][0]["hits"][0] == {
        "title": "T",
        "url": "https://n.example/a",
        "body": "B",
        "date": "2026-01-01",
        "source": "Wire",
    }


class RatelimitException(Exception):
    pass


@pytest.mark.parametrize(
    "error,code",
    [
        (RatelimitException("202 Ratelimit at https://x secret"), "rate_limited"),
        (Exception("No results found."), "no_results"),
        (RuntimeError("boom secret-detail"), "failed"),
        (TimeoutError(), "timeout"),
    ],
)
async def test_search_failures_are_classified_without_text(make_client, error, code):
    r, _ = await _post(make_client, "/v1/scrape/crunchbase", {"queries": ["q"], "cities": ["c"]}, Recorder(error=error))
    assert r.status_code == 200
    run = r.json()["runs"][0]
    assert run["error"] == code and run["hits"] == []
    assert "secret" not in r.text


async def test_empty_results_are_no_results(make_client):
    r, _ = await _post(make_client, "/v1/scrape/crunchbase", {"queries": ["q"], "cities": ["c"]}, Recorder(results=[]))
    assert r.json()["runs"][0]["error"] == "no_results"


async def test_search_count_is_capped(make_client):
    r, rec = await _post(
        make_client,
        "/v1/scrape/web_directories",
        {"queries": [f"q{i}" for i in range(4)], "cities": [f"c{i}" for i in range(3)]},
    )
    assert r.status_code == 400 and not rec.calls  # 4*3*3 = 36 > 30


async def test_hit_strings_are_truncated(make_client):
    r, _ = await _post(
        make_client,
        "/v1/scrape/crunchbase",
        {"queries": ["q"], "cities": ["c"]},
        Recorder(results=[{"title": "t" * 900, "href": "u", "body": "b" * 9000}]),
    )
    hit = r.json()["runs"][0]["hits"][0]
    assert len(hit["title"]) == 500 and len(hit["body"]) == 2000


async def test_list_bounds(make_client):
    r, _ = await _post(make_client, "/v1/scrape/crunchbase", {"queries": ["q"] * 11, "cities": ["c"]})
    assert r.status_code == 400
    r, _ = await _post(make_client, "/v1/scrape/crunchbase", {"queries": ["x" * 201], "cities": ["c"]})
    assert r.status_code == 400


# ── google maps ──────────────────────────────────────────────────────


def test_maps_search_url_is_encoded():
    assert (
        google_maps.search_url("HR & staffing", "New Delhi")
        == "https://www.google.com/maps/search/HR+%26+staffing+in+New+Delhi"
    )


class Loc:
    def __init__(self, items):
        self.items = items

    async def count(self):
        return len(self.items)

    @property
    def first(self):
        return self.items[0]

    async def all(self):
        return self.items


class El:
    def __init__(self, text="", attrs=None):
        self.text, self.attrs = text, attrs or {}

    async def text_content(self, timeout=None):
        return self.text

    async def get_attribute(self, name, timeout=None):
        return self.attrs.get(name)


class MapsPage:
    def __init__(self, table):
        self.table = table

    def locator(self, sel):
        return Loc(self.table.get(sel, []))


async def test_extract_place_fallback_selectors():
    page = MapsPage(
        {
            "h1": [El(" Acme Staffing ")],
            '[aria-label*="Phone"]': [El(attrs={"aria-label": "Phone: +91 98450 12345 "})],
            'a[data-item-id="authority"]': [El(attrs={"href": "https://acme.example"})],
            '[aria-label*="Address"]': [El(attrs={"aria-label": "Address: MG Road"})],
            '[class*="fontBodyMedium"] button': [El("Employment agency")],
            '[class*="fontDisplayLarge"]': [El("4.5")],
        }
    )
    place = await google_maps.extract_place(page)
    assert place.model_dump() == {
        "name": "Acme Staffing",
        "address": "Address: MG Road",
        "phone": "+91 98450 12345",
        "website": "https://acme.example",
        "rating": "4.5",
        "category": "Employment agency",
    }


async def test_extract_place_tel_link():
    page = MapsPage({"h1": [El("X")], 'a[href^="tel:"]': [El(attrs={"href": "tel:+1 555 0100"})]})
    assert (await google_maps.extract_place(page)).phone == "+1 555 0100"


# ── justdial ─────────────────────────────────────────────────────────

JD_HTML = """
<div class="resultbox_info"><div class="resultbox_title"><a>Acme Staffing</a></div>
<div class="resultbox_phone"> 080 1234 5678 </div><div class="resultbox_address">MG Road, Bengaluru</div></div>
<div class="store-details"><span class="store-name">Beta HR</span></div>
<div class="jsx-card"><p>no name</p></div>
"""


def test_justdial_parse_listings():
    got = [x.model_dump() for x in justdial.parse_listings(JD_HTML)]
    assert got == [
        {"name": "Acme Staffing", "phone_text": "080 1234 5678", "address": "MG Road, Bengaluru"},
        {"name": "Beta HR", "phone_text": "", "address": ""},
    ]


async def test_justdial_page_only_accepts_justdial_hosts(make_client, pool):
    c = await make_client()
    r = await c.post("/v1/scrape/justdial_page", headers=AUTH, json={"url": "https://example.com/x"})
    assert r.status_code == 400
    pool.html = JD_HTML
    r = await c.post("/v1/scrape/justdial_page", headers=AUTH, json={"url": "https://www.justdial.com/Pune/HR"})
    assert r.status_code == 200 and r.json()["listings"][0]["name"] == "Acme Staffing"
