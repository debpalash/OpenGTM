import urllib.parse
import logfire
import asyncio
from playwright.async_api import async_playwright


class TorrentService:
    def __init__(self):
        self.browser_context = None
        self._lock = asyncio.Lock()

    async def _get_page(self, playwright):
        browser = await playwright.chromium.launch(headless=True)
        context = await browser.new_context(
            user_agent="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36"
        )
        return browser, context

    async def search(self, query: str):
        """
        Search multiple indexers using Playwright.
        Returns a list of dicts: {name, seeds, leechers, size, magnet, url, source}
        """
        async with async_playwright() as p:
            browser, context = await self._get_page(p)
            try:
                # Search 1337x and TPB in parallel
                results_1337x, results_tpb = await asyncio.gather(
                    self._search_1337x(context, query),
                    self._search_tpb(context, query),
                    return_exceptions=True,
                )

                all_results = []
                if isinstance(results_1337x, list):
                    all_results.extend(results_1337x)
                if isinstance(results_tpb, list):
                    all_results.extend(results_tpb)

                # Sort by seeds desc
                def parse_seeds(s):
                    try:
                        return int(str(s).replace(",", ""))
                    except:
                        return 0

                all_results.sort(
                    key=lambda x: parse_seeds(x.get("seeds", 0)), reverse=True
                )
                return all_results[:40]
            finally:
                await browser.close()

    async def _search_1337x(self, context, query):
        mirrors = [
            f"https://1337x.to/search/{urllib.parse.quote(query)}/1/",
            f"https://1337x.st/search/{urllib.parse.quote(query)}/1/",
            f"https://x1337x.se/search/{urllib.parse.quote(query)}/1/",
            f"https://1337x.so/search/{urllib.parse.quote(query)}/1/",
        ]

        for url in mirrors:
            page = await context.new_page()
            try:
                logfire.info(f"1337x: Trying mirror {url}")
                await page.set_extra_http_headers({"Accept-Language": "en-US,en;q=0.9"})
                # Use load instead of networkidle for speed if possible
                await page.goto(url, timeout=30000, wait_until="domcontentloaded")
                content = await page.content()

                if (
                    "blocked" in content.lower()
                    or "ministry of electronics" in content.lower()
                ):
                    await page.close()
                    continue

                try:
                    await page.wait_for_selector(
                        "table.table-list, div.no-results", timeout=10000
                    )
                except:
                    if "table-list" not in content:
                        await page.close()
                        continue

                rows = await page.query_selector_all("table.table-list tr")
                results = []
                for row in rows:
                    cols = await row.query_selector_all("td")
                    if len(cols) < 6:
                        continue

                    name_link = await cols[0].query_selector("a:nth-of-type(2)")
                    if not name_link:
                        continue

                    name = await name_link.inner_text()
                    href = await name_link.get_attribute("href")

                    base = "/".join(url.split("/")[:3])
                    results.append(
                        {
                            "name": name.strip(),
                            "seeds": (await cols[1].inner_text()).strip(),
                            "leechers": (await cols[2].inner_text()).strip(),
                            "size": (await cols[4].inner_text())
                            .split("seed")[0]
                            .strip(),
                            "url": f"{base}{href}",
                            "source": "1337x",
                            "uploader": (await cols[5].inner_text()).strip(),
                        }
                    )
                if results:
                    logfire.info(f"1337x: Found {len(results)} results on {url}")
                    return results
            except Exception:
                pass
            finally:
                if not page.is_closed():
                    await page.close()
        return []

    async def _search_tpb(self, context, query):
        mirrors = [
            f"https://tpb.party/search/{urllib.parse.quote(query)}/1/99/0",
            f"https://thepiratebay.org/search.php?q={urllib.parse.quote(query)}&all=on&search=Pirate+Search&page=0&orderby=",
            f"https://thepiratebay.zone/search/{urllib.parse.quote(query)}/1/99/0",
            f"https://piratebay.party/search/{urllib.parse.quote(query)}/1/99/0",
        ]

        for url in mirrors:
            page = await context.new_page()
            try:
                logfire.info(f"TPB: Trying mirror {url}")
                await page.set_extra_http_headers({"Accept-Language": "en-US,en;q=0.9"})
                await page.goto(url, timeout=30000, wait_until="domcontentloaded")
                content = await page.content()

                if (
                    "blocked" in content.lower()
                    or "ministry of electronics" in content.lower()
                ):
                    await page.close()
                    continue

                try:
                    await page.wait_for_selector(
                        "#searchResult, #st, table#searchResult", timeout=10000
                    )
                except:
                    pass

                rows = await page.query_selector_all("#searchResult tr, #st tr")
                results = []
                for row in rows:
                    cols = await row.query_selector_all("td")
                    if len(cols) < 4:
                        continue

                    name_link = await cols[1].query_selector("div.detName a, a")
                    if not name_link:
                        continue

                    name = await name_link.inner_text()
                    href = await name_link.get_attribute("href")

                    magnet_link = await cols[1].query_selector("a[href^='magnet:?']")
                    magnet = (
                        await magnet_link.get_attribute("href") if magnet_link else None
                    )

                    seeds = "0"
                    leechers = "0"
                    if len(cols) >= 6:
                        # Indices for SE and LE
                        seeds = (await cols[len(cols) - 2].inner_text()).strip()
                        leechers = (await cols[len(cols) - 1].inner_text()).strip()

                    info_text = await cols[1].inner_text()
                    size = "N/A"
                    if "Size " in info_text:
                        size = info_text.split("Size ")[1].split(",")[0].strip()

                    results.append(
                        {
                            "name": name.strip(),
                            "seeds": seeds,
                            "leechers": leechers,
                            "size": size,
                            "url": href
                            if href.startswith("http")
                            else f"{'/'.join(url.split('/')[:3])}{href}",
                            "magnet": magnet,
                            "source": "TPB",
                            "uploader": "Anonymous",
                        }
                    )
                if results:
                    logfire.info(f"TPB: Found {len(results)} results on {url}")
                    return results
            except Exception:
                pass
            finally:
                if not page.is_closed():
                    await page.close()
        return []

    async def get_magnet(self, url: str):
        """
        Extract magnet link via Playwright.
        """
        async with async_playwright() as p:
            browser, context = await self._get_page(p)
            page = await context.new_page()
            try:
                await page.goto(url, timeout=30000, wait_until="domcontentloaded")
                magnet_link = await page.query_selector("a[href^='magnet:?']")
                if magnet_link:
                    return await magnet_link.get_attribute("href")
                return None
            except Exception as e:
                logfire.error(f"Failed to get magnet: {e}")
                return None
            finally:
                await browser.close()


torrent_service = TorrentService()
