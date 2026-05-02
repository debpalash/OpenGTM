import json
import os
import asyncio
import aiohttp
import time
import urllib.parse
from typing import List, Dict, Optional
import logfire
from playwright.async_api import async_playwright
from apps.api.services.log_stream import manager

# Mirror Configurations
MIRRORS = {
    "1337x": ["https://1337x.to", "https://www.1337x.tw"],
    "tpb": ["https://thepiratebay.org", "https://apibay.org/"],
    "torrentgalaxy": ["https://torrentgalaxy.to"],
    "limetorrents": ["https://www.limetorrents.info"],
}


class MirrorValidator:
    STATE_FILE = "mirror_state.json"

    def __init__(self):
        self.healthy_mirrors: Dict[str, List[str]] = {k: [] for k in MIRRORS.keys()}
        self.last_check = 0
        self.check_interval = 3600  # Check every hour
        self._lock = asyncio.Lock()
        self.load_state()

    def load_state(self):
        """Load healthy mirrors from disk."""
        if os.path.exists(self.STATE_FILE):
            try:
                with open(self.STATE_FILE, "r") as f:
                    data = json.load(f)
                    self.healthy_mirrors = data.get("mirrors", self.healthy_mirrors)
                    self.last_check = data.get("last_check", 0)
                    logfire.info(f"Loaded mirror state. Last check: {self.last_check}")
            except Exception as e:
                logfire.error(f"Failed to load mirror state: {e}")

    def save_state(self):
        """Save healthy mirrors to disk."""
        try:
            with open(self.STATE_FILE, "w") as f:
                json.dump(
                    {
                        "mirrors": self.healthy_mirrors,
                        "last_check": self.last_check,
                    },
                    f,
                )
        except Exception as e:
            logfire.error(f"Failed to save mirror state: {e}")

    async def start_validation_loop(self):
        """Run validation loop in background."""
        while True:
            logfire.info("Starting scheduled mirror validation...")
            try:
                await self._validate_all()
            except Exception as e:
                logfire.error(f"Mirror validation failed: {e}")

            # Sleep for 6 hours
            await asyncio.sleep(6 * 3600)

    async def get_healthy_mirrors(self, source: str) -> List[str]:
        """Get cached list of healthy mirrors."""
        # Purely non-blocking read
        return self.healthy_mirrors.get(source) or MIRRORS.get(source, [])

    async def _validate_all(self):
        """Validate all mirrors concurrently."""
        logfire.info("Validating torrent mirrors...")
        tasks = []
        for source, urls in MIRRORS.items():
            for url in urls:
                tasks.append(self._check_mirror(source, url))

        results = await asyncio.gather(*tasks, return_exceptions=True)

        # Reset and populate
        self.healthy_mirrors = {k: [] for k in MIRRORS.keys()}

        # Sort results by latency
        valid_results = [r for r in results if isinstance(r, dict) and r.get("healthy")]
        valid_results.sort(key=lambda x: x["latency"])

        for r in valid_results:
            self.healthy_mirrors[r["source"]].append(r["url"])

        self.last_check = time.time()
        self.save_state()  # Persist state

        logfire.info(
            f"Mirror validation complete. Status: { {k: len(v) for k, v in self.healthy_mirrors.items()} }"
        )
        await manager.emit_log(
            f"Mirror validation complete. Active: { {k: len(v) for k, v in self.healthy_mirrors.items()} }",
            "success",
            "system",
        )

    # ... _check_mirror remains same ...
    async def _check_mirror(self, source: str, url: str) -> Dict:
        """Check a single mirror's health and latency."""
        await manager.emit_log(f"Checking {source} mirror: {url}...", "info", "system")
        start = time.time()
        try:
            async with aiohttp.ClientSession() as session:
                async with session.get(url, timeout=5) as response:
                    if response.status == 200:
                        latency = time.time() - start
                        return {
                            "source": source,
                            "url": url,
                            "healthy": True,
                            "latency": latency,
                        }
        except Exception:
            pass
        return {"source": source, "url": url, "healthy": False, "latency": 999}


class TorrentSmartEngine:
    def __init__(self):
        self.validator = MirrorValidator()
        self.browser_context = None

    async def start_background_tasks(self):
        """Start background maintenance tasks."""
        asyncio.create_task(self.validator.start_validation_loop())

    # ... _get_context remains same ...
    async def _get_context(self, p):
        browser = await p.chromium.launch(
            headless=True,
            args=[
                "--disable-blink-features=AutomationControlled",
                "--no-sandbox",
                "--disable-setuid-sandbox",
                "--disable-infobars",
                "--window-position=0,0",
                "--ignore-certifcate-errors",
                "--ignore-certifcate-errors-spki-list",
                "--user-agent=Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
            ],
        )
        context = await browser.new_context(
            user_agent="Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
            viewport={"width": 1920, "height": 1080},
            locale="en-US",
            timezone_id="America/New_York",
            has_touch=True,
            is_mobile=False,
            device_scale_factor=1,
            color_scheme="dark",
            permissions=["geolocation"],
        )
        # Injection to hide webdriver
        await context.add_init_script(
            "Object.defineProperty(navigator, 'webdriver', {get: () => undefined})"
        )
        return browser, context

    def _is_safe_content(self, name: str) -> bool:
        """Check if content is safe (no adult content)."""
        forbidden = [
            "xxx",
            "porn",
            "adult",
            "sex",
            "nsfw",
            "18+",
            "uncensored",
            "hentai",
            "babe",
            "milf",
        ]
        name_lower = name.lower()
        if any(bad in name_lower for bad in forbidden):
            return False
        return True

    async def search(self, query: str, user_role: str = "user") -> List[Dict]:
        """Parallel search across best mirrors of all trackers."""
        # Safety Filter (Bypass for superadmin)
        if user_role != "superadmin" and not self._is_safe_content(query):
            await manager.emit_log(
                f"Search query rejected by safety filter: {query}", "error", "system"
            )
            return []

        async with async_playwright() as p:
            browser, context = await self._get_context(p)
            try:
                await manager.emit_log(
                    f"Starting parallel search for: '{query}'", "info", "system"
                )
                tasks = [
                    self._search_source(context, "1337x", query),
                    self._search_source(context, "tpb", query),
                    self._search_source(context, "torrentgalaxy", query),
                    self._search_source(context, "limetorrents", query),
                ]
                results_list = await asyncio.gather(*tasks, return_exceptions=True)

                total_found = 0
                all_results = []

                for res in results_list:
                    if isinstance(res, list):
                        # Filter results unless superadmin
                        if user_role == "superadmin":
                            safe_res = res
                        else:
                            safe_res = [
                                r
                                for r in res
                                if self._is_safe_content(r.get("name", ""))
                            ]

                        total_found += len(safe_res)
                        all_results.extend(safe_res)

                await manager.emit_log(
                    f"Search completed. Found {total_found} verified results"
                    + (
                        " (Safety Filter Active)."
                        if user_role != "superadmin"
                        else " (Superadmin Mode)."
                    ),
                    "success" if total_found > 0 else "warning",
                    "system",
                )

                # Sort by seeds
                all_results.sort(
                    key=lambda x: self._parse_int(x.get("seeds", 0)), reverse=True
                )
                return all_results

            finally:
                await browser.close()

    async def _search_source(self, context, source: str, query: str) -> List[Dict]:
        """Search a specific source using its healthy mirrors."""
        mirrors = await self.validator.get_healthy_mirrors(source)

        # Try up to 3 mirrors in parallel? Or sequential failover?
        # Parallel is faster but heavier. Let's do sequential failover on top 3 healthy mirrors.
        # But wait, user requested "parallel methods to faster the process".
        # So we should pick the BEST mirror and use it. If it fails, try next.
        # OR launch 2 best mirrors in parallel and take first success.

        target_mirrors = mirrors[:2]  # Try top 2

        for url in target_mirrors:
            try:
                if source == "1337x":
                    return await self._scrape_1337x(context, url, query)
                elif source == "tpb":
                    return await self._scrape_tpb(context, url, query)
                elif source == "torrentgalaxy":
                    return await self._scrape_torrentgalaxy(context, url, query)
                elif source == "limetorrents":
                    return await self._scrape_limetorrents(context, url, query)
            except Exception as e:
                logfire.warning(f"Failed to scrape {source} at {url}: {e}")
                continue

        return []

    async def _scrape_1337x(self, context, base_url: str, query: str) -> List[Dict]:
        search_url = f"{base_url}/search/{urllib.parse.quote(query)}/1/"
        page = await context.new_page()
        results = []
        try:
            await page.goto(search_url, timeout=15000, wait_until="domcontentloaded")

            # Check for blocking
            content = await page.content()
            if "blocked" in content.lower():
                raise Exception("Blocked")

            # Selector strategy
            rows = await page.locator("table.table-list tr").all()

            # If no rows found, maybe just no results or selector change
            if not rows:
                return []

            for row in rows:
                cols = await row.locator("td").all()
                if len(cols) < 6:
                    continue

                name_el = cols[0].locator("a:nth-of-type(2)")
                if await name_el.count() == 0:
                    continue

                name = await name_el.inner_text()
                href = await name_el.get_attribute("href")
                seeds = await cols[1].inner_text()
                leechers = await cols[2].inner_text()
                size = (await cols[4].inner_text()).split("seed")[0]
                uploader = await cols[5].inner_text()

                # Resolve full URL
                full_url = f"{base_url}{href}" if href.startswith("/") else href

                results.append(
                    {
                        "name": name.strip(),
                        "seeds": seeds.strip(),
                        "leechers": leechers.strip(),
                        "size": size.strip(),
                        "url": full_url,
                        "source": "1337x",
                        "uploader": uploader.strip(),
                    }
                )

            return results
        finally:
            await page.close()

    async def _scrape_tpb(self, context, base_url: str, query: str) -> List[Dict]:
        # TPB URL structures vary, but search query param is common
        if "search.php" in base_url:
            search_url = f"{base_url}?q={urllib.parse.quote(query)}"
        else:
            search_url = f"{base_url}/search/{urllib.parse.quote(query)}/1/99/0"

        page = await context.new_page()
        results = []
        try:
            await page.goto(search_url, timeout=15000, wait_until="domcontentloaded")

            rows = await page.locator("#searchResult tr").all()
            # If fewer than 2 rows (header + results), might be empty
            if await page.locator("#searchResult").count() == 0:
                pass

            for row in rows:
                cols = await row.locator("td").all()
                if len(cols) < 2:
                    continue  # Header or malformed

                # Check if it's a valid row
                name_loc = row.locator("div.detName a")
                if await name_loc.count() == 0:
                    continue

                name = await name_loc.inner_text()
                href = await name_loc.get_attribute("href")

                magnet_loc = row.locator("a[href^='magnet:?']")
                magnet = (
                    await magnet_loc.get_attribute("href")
                    if await magnet_loc.count() > 0
                    else None
                )

                # Seeds/Leechers usually in last columns
                seeds = await cols[-2].inner_text()
                leechers = await cols[-1].inner_text()

                desc_text = await cols[1].inner_text()
                size = "N/A"
                if "Size" in desc_text:
                    try:
                        # "Uploaded 02-28 2013, Size 1.39 GiB, ULed by ..."
                        parts = desc_text.split("Size")[1].split(",")
                        size = parts[0].strip()
                    except:
                        pass

                full_url = (
                    f"{base_url}{href}" if href and href.startswith("/") else href
                )

                results.append(
                    {
                        "name": name.strip(),
                        "seeds": seeds.strip(),
                        "leechers": leechers.strip(),
                        "size": size,
                        "url": full_url,
                        "magnet": magnet,
                        "source": "TPB",
                        "uploader": "Anonymous",
                    }
                )
            return results
        finally:
            await page.close()

    async def _scrape_torrentgalaxy(
        self, context, base_url: str, query: str
    ) -> List[Dict]:
        search_url = f"{base_url}/torrents.php?search={urllib.parse.quote(query)}"
        page = await context.new_page()
        results = []
        try:
            await page.goto(search_url, timeout=15000, wait_until="domcontentloaded")

            # Selector for rows (TGx uses divs mostly)
            # Row class usually "tgxtablerow"
            rows = await page.locator("div.tgxtablerow").all()

            for row in rows:
                try:
                    # Cells are in child divs
                    cells = await row.locator("div.tgxtablecell").all()
                    if len(cells) < 5:
                        continue

                    # Name and Link
                    name_el = cells[3].locator("a").first
                    if await name_el.count() == 0:
                        name_el = cells[3].locator("b a").first  # Sometimes wrapped

                    if await name_el.count() == 0:
                        continue

                    name = await name_el.inner_text()
                    href = await name_el.get_attribute("href")
                    full_url = f"{base_url}{href}" if href.startswith("/") else href

                    # Magnet is usually in cells[4]
                    magnet_el = cells[4].locator("a[href^='magnet:?']").first
                    magnet = (
                        await magnet_el.get_attribute("href")
                        if await magnet_el.count() > 0
                        else None
                    )

                    # Size, seeds, leechers
                    # Helper to find text safely
                    size = await cells[7].inner_text()
                    seeds = (
                        await cells[10].locator("b font").inner_text()
                    )  # TGx often colors seeds green
                    leechers = (
                        await cells[10].locator("b font").nth(1).inner_text()
                        if await cells[10].locator("b font").count() > 1
                        else "0"
                    )

                    results.append(
                        {
                            "name": name.strip(),
                            "seeds": seeds.strip(),
                            "leechers": leechers.strip(),
                            "size": size.strip(),
                            "url": full_url,
                            "magnet": magnet,
                            "source": "TorrentGalaxy",
                            "uploader": await cells[9].inner_text(),
                        }
                    )
                except Exception:
                    continue
            return results
        finally:
            await page.close()

    async def _scrape_limetorrents(
        self, context, base_url: str, query: str
    ) -> List[Dict]:
        search_url = f"{base_url}/search/all/{urllib.parse.quote(query)}/"
        page = await context.new_page()
        results = []
        try:
            await page.goto(search_url, timeout=15000, wait_until="domcontentloaded")

            rows = await page.locator("table.table2 tr").all()

            for row in rows:
                # Skip header
                if await row.locator("th").count() > 0:
                    continue

                cols = await row.locator("td").all()
                if len(cols) < 4:
                    continue

                try:
                    name_el = (
                        cols[0].locator("div.tt-name a").nth(1)
                    )  # first is usually unseen/icon
                    if await name_el.count() == 0:
                        name_el = cols[0].locator("a").nth(0)

                    name = await name_el.inner_text()
                    href = await name_el.get_attribute("href")
                    full_url = f"{base_url}{href}" if href.startswith("/") else href

                    seeds = await cols[3].inner_text()
                    leechers = await cols[4].inner_text()
                    size = await cols[2].inner_text()

                    results.append(
                        {
                            "name": name.strip(),
                            "seeds": seeds.strip(),
                            "leechers": leechers.strip(),
                            "size": size.strip(),
                            "url": full_url,
                            "magnet": None,  # LimeTorrents pages usually need a visit for magnet
                            "source": "LimeTorrents",
                            "uploader": "Anonymous",
                        }
                    )
                except Exception:
                    continue
            return results
        finally:
            await page.close()

    async def get_details(self, url: str) -> Dict:
        """Get torrent details including file list."""
        async with async_playwright() as p:
            browser, context = await self._get_context(p)
            try:
                page = await context.new_page()
                await page.goto(url, timeout=15000, wait_until="domcontentloaded")

                files = []
                magnet = None

                # 1337x specific parsing
                if "1337x" in url:
                    # Get Magnet
                    magnet_el = page.locator("a[href^='magnet:?']").first
                    if await magnet_el.count() > 0:
                        magnet = await magnet_el.get_attribute("href")

                    # Get Files
                    # Usually hidden in a tab "Files" or just present
                    # Check for "Files" tab
                    files_tab = page.locator("a:has-text('Files')")
                    if await files_tab.count() > 0:
                        try:
                            # 1337x usually puts files in a separate tab that might need clicking or #files hash
                            # But often it's loaded in DOM. Let's check the files list container.
                            # selector: #files
                            # If it's not visible, we might need to click the tab
                            if not await page.locator("#files").is_visible():
                                await files_tab.click()
                                await page.wait_for_selector("#files", timeout=2000)
                        except Exception:
                            pass

                    file_rows = await page.locator("#files ul li, .file-list li").all()
                    for row in file_rows:
                        files.append(await row.inner_text())

                # TPB specific parsing
                elif "piratebay" in url or "tpb" in url:
                    magnet_el = page.locator("a[href^='magnet:?']").first
                    if await magnet_el.count() > 0:
                        magnet = await magnet_el.get_attribute("href")

                return {
                    "url": url,
                    "magnet": magnet,
                    "files": files,
                    "file_count": len(files),
                }

            except Exception as e:
                logfire.error(f"Error getting details for {url}: {e}")
                return {}
            finally:
                await browser.close()

    def _parse_int(self, val):
        try:
            return int(str(val).replace(",", ""))
        except Exception:
            return 0


# Global Instance
smart_torrent_engine = TorrentSmartEngine()
