"""Browser check of the Plugins > Runs list: server-side paging, infinite
scrolling, the Load more button and row virtualization, against the built SPA.

The SPA is served from apps/web/dist by the small static server of the web
benchmark (benchmarks/web.py), and the API is answered from fixtures with the
same cursor contract as the Go server (400 runs, 50 per page). Skipped when
apps/web has not been built (`bun run build`) or no Chromium is installed
(`uv run playwright install chromium`).
"""

import re

import pytest

pytest.importorskip("playwright.sync_api")
from playwright.sync_api import sync_playwright  # noqa: E402

from benchmarks import web  # noqa: E402

DIST = web.WEB / "dist"
pytestmark = pytest.mark.skipif(not (DIST / ".vite" / "manifest.json").exists(), reason="apps/web is not built (bun run build)")

TABLE = 'table[aria-label="Plugin runs"]'
ROWS = f"{TABLE} tbody tr[data-run-id]"


@pytest.fixture(scope="module")
def browser():
    with web.Server(DIST) as srv, sync_playwright() as pw:
        try:
            b = pw.chromium.launch()
        except Exception as e:  # no browser installed
            pytest.skip(f"Chromium is not available: {e}")
        b.base = srv.base  # type: ignore[attr-defined]
        yield b
        b.close()


@pytest.fixture()
def page(browser):
    ctx = browser.new_context(viewport={"width": 1366, "height": 800})
    ctx.add_init_script(web.INIT_SCRIPT)
    web.install_api(ctx, web.fixtures())
    pg = ctx.new_page()
    pg.requests = []  # type: ignore[attr-defined]
    pg.on("request", lambda r: pg.requests.append(r.url.split("//", 1)[1].split("/", 1)[1]) if "/api/v2/plugin-runs" in r.url else None)
    yield pg
    ctx.close()


def open_runs(page, browser):
    # Chromium aborts in-flight requests (ERR_NETWORK_CHANGED, even on loopback)
    # when the host's network interfaces change, e.g. a docker bridge flapping;
    # retry the page load instead of failing on that.
    for attempt in range(3):
        page.requests.clear()
        page.goto(browser.base + "/plugins?tab=runs")
        try:
            page.wait_for_selector(ROWS, timeout=10_000)
            break
        except Exception:
            if attempt == 2:
                raise
    page.wait_for_timeout(500)


def scroll_to_end(page):
    page.locator('[role="region"][aria-label="Plugin runs"]').evaluate("e => e.scrollTo(0, e.scrollHeight)")
    page.wait_for_timeout(400)


def status(page) -> str:
    return " ".join(t for t in page.locator("[role=status]").all_inner_texts() if "runs" in t or "run" in t)


def test_first_page_only_and_rows_are_virtualized(page, browser):
    open_runs(page, browser)
    assert page.requests == ["api/v2/plugin-runs?limit=50"]  # the first page carries no cursor
    assert "50+ runs" in status(page)  # more exist
    # 50 loaded, far fewer rows in the DOM.
    assert 5 < page.locator(ROWS).count() < 40
    assert page.get_by_role("button", name="Load more").count() == 1


def test_scrolling_loads_every_page_in_cursor_order_then_stops(page, browser):
    open_runs(page, browser)
    for _ in range(20):
        if page.get_by_text("That is every run.").count():
            break
        scroll_to_end(page)
    assert page.get_by_text("That is every run.").count() == 1
    assert "400 runs" in status(page)
    # Each request continued from the previous page's cursor: 0, 50, ... 350.
    cursors = [re.search(r"cursor=(\d+)", r).group(1) if "cursor=" in r else None for r in page.requests]
    first_seven = cursors[:8]
    assert first_seven == [None, "50", "100", "150", "200", "250", "300", "350"], page.requests
    assert page.get_by_role("button", name="Load more").count() == 0
    assert page.locator(ROWS).count() < 40  # still virtualized with all 400 loaded


def test_load_more_button_loads_exactly_one_page(page, browser):
    open_runs(page, browser)
    before = len(page.requests)
    page.get_by_role("button", name="Load more").click()
    page.wait_for_function("() => document.body.innerText.includes('100+ runs')", timeout=10_000)
    assert len(page.requests) == before + 1
    assert page.requests[before].endswith("cursor=50")


def test_status_filter_does_not_auto_page_through_history(page, browser):
    open_runs(page, browser)
    page.locator('select[aria-label="Status"]').select_option("cancelled")
    page.wait_for_timeout(300)
    before = len(page.requests)
    scroll_to_end(page)
    scroll_to_end(page)
    assert len(page.requests) == before  # filtering never triggers scroll loading
    assert "of 50+ loaded runs" in status(page)
    page.get_by_role("button", name="Load more").click()  # but the button still does
    page.wait_for_function("() => document.body.innerText.includes('of 100+ loaded runs')", timeout=10_000)


def test_a_failed_page_keeps_the_list_and_offers_a_retry(page, browser):
    open_runs(page, browser)
    state = {"fail": True}

    def flaky(route):
        if "cursor=" in route.request.url and state["fail"]:
            return route.fulfill(status=500, json={"detail": "Could not list plugin runs"})
        return route.fallback()

    page.context.route("**/api/v2/plugin-runs**", flaky)
    scroll_to_end(page)
    page.wait_for_selector("text=Could not load more runs.", timeout=10_000)
    failed_requests = len(page.requests)
    scroll_to_end(page)
    scroll_to_end(page)
    assert len(page.requests) == failed_requests  # scrolling does not hammer a failing endpoint
    assert page.locator(ROWS).count() > 5  # the loaded runs are still there
    state["fail"] = False
    page.get_by_role("button", name="Try again").click()
    page.wait_for_function("() => document.body.innerText.includes('100+ runs')", timeout=10_000)
