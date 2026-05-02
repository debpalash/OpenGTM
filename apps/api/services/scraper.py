import httpx
from bs4 import BeautifulSoup
from playwright.async_api import async_playwright
import logfire
import re
import asyncio


class UniversalScraper:
    def __init__(self):
        self.headers = {
            "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
        }
        self.email_pattern = r"[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}"

    async def scrape(self, url: str, screenshot_callback=None):
        # Specific Handle for Scribd: Always use robust path with scrolling
        if "scribd.com/document" in url:
            logfire.info(f"Scribd URL detected, forcing Playwright: {url}")
            return await self._scrape_with_playwright(url, screenshot_callback)

        # If Live Preview is requested, we MUST use Playwright to get screenshots
        if screenshot_callback:
            logfire.info(f"Live Preview requested, forcing Playwright for {url}")
            return await self._scrape_with_playwright(url, screenshot_callback)

        # 1. Fast Path: HTTPX
        try:
            logfire.info(f"Attempting fast scrape for {url}")
            async with httpx.AsyncClient(
                follow_redirects=True, timeout=10.0, verify=False
            ) as client:
                response = await client.get(url, headers=self.headers)
                if response.status_code == 200:
                    text_lower = response.text.lower()
                    if (
                        "enable javascript" in text_lower
                        or "please wait..." in text_lower
                        and len(response.text) < 2000
                    ):
                        logfire.info(
                            "Fast scrape detected JS wall, switching to robust"
                        )
                    else:
                        return self._parse_html(response.text, url, method="fast")
        except Exception as e:
            logfire.warn(f"Fast scrape failed for {url}: {e}")

        # 2. Robust Path: Playwright
        logfire.info(f"Falling back to Playwright for {url}")
        return await self._scrape_with_playwright(url)

    async def _scrape_with_playwright(self, url: str, screenshot_callback=None):
        import os
        async with async_playwright() as p:
            browser = await p.chromium.launch(headless=True)
            context = await browser.new_context(
                user_agent=self.headers["User-Agent"],
                viewport={"width": 1280, "height": 800},
            )

            # Inject LinkedIn cookie if available and the URL is linkedin
            if "linkedin.com" in url:
                li_at = os.environ.get("LINKEDIN_LI_AT_COOKIE")
                if li_at:
                    logfire.info("Injecting LinkedIn li_at cookie into browser context")
                    await context.add_cookies([
                        {
                            "name": "li_at",
                            "value": li_at,
                            "domain": ".linkedin.com",
                            "path": "/",
                            "secure": True,
                        }
                    ])

            page = await context.new_page()
            try:
                if screenshot_callback:

                    async def stream_screenshots():
                        logfire.info("Screenshot stream started")
                        frame_count = 0
                        while not page.is_closed():
                            try:
                                # Playwright 'scale' expects 'css' or 'device' (string), not a float.
                                # To keep size small, we rely on 'quality' and 'type' for JPEG.
                                screenshot = await page.screenshot(
                                    type="jpeg", quality=30
                                )
                                await screenshot_callback(screenshot)
                                frame_count += 1
                                await asyncio.sleep(0.5)
                            except Exception as e:
                                # Check if it's just the page closing
                                if not page.is_closed():
                                    logfire.error(f"Screenshot error: {e}")
                                break

                    # Store the task so it can be managed
                    screenshot_task = asyncio.create_task(stream_screenshots())

                # Use domcontentloaded for speed, networkidle is too flaky on heavy sites
                await page.goto(url, wait_until="domcontentloaded", timeout=45000)

                # Check for Scribd text layer specifically
                try:
                    if "scribd.com/document" in url:
                        logfire.info("Waiting for Scribd text layer...")
                        await page.wait_for_selector(".text_layer", timeout=10000)
                except Exception:
                    logfire.warn(
                        "Scribd text layer not found immediately, continuing..."
                    )

                # Auto-Scroll to bottom to trigger lazy loading
                try:
                    last_height = await page.evaluate("document.body.scrollHeight")
                    for i in range(5):
                        await page.evaluate(
                            "window.scrollTo(0, document.body.scrollHeight)"
                        )
                        await page.wait_for_timeout(2000)
                        
                        # Use try-except here as well just in case navigation happens during sleep
                        try:
                            new_height = await page.evaluate("document.body.scrollHeight")
                            if new_height == last_height:
                                break
                            last_height = new_height
                        except Exception as e:
                            logfire.warn(f"Navigation interrupted scrolling: {e}")
                            break
                except Exception as e:
                    logfire.warn(f"Failed to auto-scroll (perhaps redirected): {e}")

                content = await page.content()
                return self._parse_html(content, url, method="robust")
            except Exception as e:
                logfire.error(f"Playwright failed for {url}: {e}")
                raise e
            finally:
                if screenshot_callback and "screenshot_task" in locals():
                    screenshot_task.cancel()
                await browser.close()

    def _parse_html(self, html: str, url: str, method: str):
        soup = BeautifulSoup(html, "lxml")

        # Cleanup
        for script in soup(["script", "style", "noscript", "iframe", "svg"]):
            script.decompose()

        # Intelligent Text Extraction
        # Use markdownify to preserve structure (headers, lists, tables) better than get_text
        from markdownify import markdownify

        # If it's Scribd, we might want to target specific containers first if they exist
        # .text_layer usually contains the actual text of the pages
        scribd_text_layers = soup.select(".text_layer")
        if scribd_text_layers:
            # Combine all text layers
            html_to_process = "\n\n".join([str(layer) for layer in scribd_text_layers])
        else:
            html_to_process = str(soup.body) if soup.body else html

        text_content = markdownify(
            html_to_process, heading_style="ATX", strip=["a", "img"]
        )

        # Clean up excessive newlines
        text_content = re.sub(r"\n{3,}", "\n\n", text_content).strip()

        title = soup.title.string if soup.title else url

        # Meta description
        meta_desc = ""
        meta_tag = soup.find("meta", attrs={"name": "description"}) or soup.find(
            "meta", attrs={"property": "og:description"}
        )
        if meta_tag:
            meta_desc = meta_tag.get("content", "")

        # Extract Emails from the MARKDOWN/TEXT content, which preserves whitespace better for regex
        emails = list(set(re.findall(self.email_pattern, text_content)))

        return {
            "status": "success",
            "url": url,
            "title": title,
            "description": meta_desc,
            "method": method,
            "extracted_emails": emails,
            "word_count": len(text_content.split()),
            "preview_text": text_content[:2000],  # Increased preview
            "html_content": html,
        }
