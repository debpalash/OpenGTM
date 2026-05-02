import requests
import json
import os
import logging
from bs4 import BeautifulSoup

logger = logging.getLogger(__name__)


class ScribdClient:
    BASE_URL = "https://www.scribd.com"
    HEADERS = {
        "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
        "Accept-Language": "en-US,en;q=0.9",
    }

    def _get_thumbnail_url(self, doc_id):
        """
        Constructs a predictable thumbnail URL for a Scribd document.
        Uses the 'original' resolution which is most reliable without a hash.
        """
        return f"https://imgv2-1-f.scribdassets.com/img/document/{doc_id}/original/216x287/1?v=1"

    def _get_cookies(self) -> dict:
        """
        Load Scribd session cookies from environment variables.
        Falls back to empty dict if not configured.
        """
        cookie_str = os.environ.get("SCRIBD_COOKIES", "")
        if not cookie_str:
            # Try individual cookie vars
            ubtc = os.environ.get("SCRIBD_UBTC", "")
            session = os.environ.get("SCRIBD_SESSION", "")
            if ubtc or session:
                parts = []
                if ubtc:
                    parts.append(f"scribd_ubtc={ubtc}")
                if session:
                    parts.append(f"_scribd_session={session}")
                cookie_str = "; ".join(parts)

        return cookie_str

    def _get_page_state(self, url):
        """
        Fetches a Scribd page and extracts the hidden Redux/Hydration state.
        This state contains the raw data used by the web app.
        """
        try:
            resp = requests.get(url, headers=self.HEADERS, timeout=15)
            if resp.status_code != 200:
                logger.warning(f"Scribd returned status {resp.status_code}")
                return None

            soup = BeautifulSoup(resp.text, "html.parser")

            # Find all comment blobs and try to parse them
            for comment in soup.find_all(
                string=lambda text: isinstance(text, str)
                and text.strip().startswith("<!--{")
            ):
                try:
                    data_str = comment.strip()[4:-3]  # Remove <!-- and -->
                    data = json.loads(data_str)
                    return data
                except Exception:
                    continue

            return None
        except Exception as e:
            logger.error(f"Error fetching state: {e}")
            return None

    def search(self, query):
        """
        Search for Scribd documents.
        Priority: Direct API → Google proxy → DuckDuckGo fallback
        """
        # Try direct API first (requires valid session cookies)
        results = self._search_direct(query)
        if results:
            return results

        logger.info("Direct search failed/empty, failing over to Google...")
        results = self._search_google(query)
        if not results:
            logger.info("Google search failed/empty, trying DuckDuckGo fallback...")
            results = self._search_ddg(query)

        return results

    def _search_direct(self, query):
        """
        Uses the internal Scribd search API.
        Requires session cookies from env vars.
        """
        cookie_str = self._get_cookies()
        if not cookie_str:
            logger.info("No Scribd cookies configured, skipping direct API")
            return []

        try:
            headers = {
                "User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:146.0) Gecko/20100101 Firefox/146.0",
                "Accept": "application/json",
                "Accept-Language": "en-US,en;q=0.5",
                "Referer": f"https://www.scribd.com/search?query={query}&verbatim=true",
                "X-Requested-With": "XMLHttpRequest",
                "Cookie": cookie_str,
            }

            url = f"https://www.scribd.com/search/query?query={query}&verbatim=true"
            resp = requests.get(url, headers=headers, timeout=10)

            if resp.status_code != 200:
                logger.warning(f"Direct API returned {resp.status_code}")
                return []

            data = resp.json()

            results = []
            if "results" in data and "documents" in data["results"]:
                doc_root = data["results"]["documents"]
                if "content" in doc_root:
                    content = doc_root["content"]
                    items = []
                    if isinstance(content, dict) and "documents" in content:
                        if "items" in content["documents"]:
                            items = content["documents"]["items"]
                        elif isinstance(content["documents"], list):
                            items = content["documents"]
                    elif isinstance(content, list):
                        items = content

                    for item in items:
                        try:
                            desc = item.get("description", "") or ""

                            results.append(
                                {
                                    "id": str(item.get("id")),
                                    "title": item.get("title"),
                                    "url": f"https://www.scribd.com/document/{item.get('id')}/{item.get('title', 'doc').replace(' ', '-')}",
                                    "thumbnail": item.get("thumbnailUrl"),
                                    "description": desc[:200] + "..."
                                    if len(desc) > 200
                                    else desc,
                                    "source": "scribd",
                                    "page_count": item.get("pageCount"),
                                    "author": item.get("author", {}).get("name")
                                    if isinstance(item.get("author"), dict)
                                    else None,
                                }
                            )
                        except Exception:
                            continue

            logger.info(f"Direct API found {len(results)} results")
            return results

        except Exception as e:
            logger.error(f"Direct API Error: {e}")
            return []

    def _search_google(self, query):
        try:
            search_url = (
                f"https://www.google.com/search?q=site:scribd.com/doc+{query}&num=20"
            )
            resp = requests.get(search_url, headers=self.HEADERS, timeout=10)

            if resp.status_code != 200:
                logger.warning(f"Google returned {resp.status_code}")
                return []

            soup = BeautifulSoup(resp.text, "html.parser")
            results = []
            seen_ids = set()

            for a in soup.find_all("a", href=True):
                href = a["href"]

                if "scribd.com/doc/" not in href and "scribd.com/document/" not in href:
                    continue

                full_url = href
                if "/url?q=" in href:
                    full_url = href.split("/url?q=")[1].split("&")[0]

                try:
                    if "/doc/" in full_url:
                        doc_id = full_url.split("/doc/")[1].split("/")[0]
                    else:
                        doc_id = full_url.split("/document/")[1].split("/")[0]
                except Exception:
                    continue

                if doc_id in seen_ids:
                    continue
                seen_ids.add(doc_id)

                title = a.get_text(strip=True)
                h3 = a.find("h3")
                if h3:
                    title = h3.get_text(strip=True)

                title = title.replace(" - Scribd", "").replace(" | Scribd", "")

                results.append(
                    {
                        "id": doc_id,
                        "title": title[:100],
                        "url": full_url,
                        "thumbnail": self._get_thumbnail_url(doc_id),
                        "source": "scribd",
                    }
                )

                if len(results) >= 15:
                    break

            return results
        except Exception as e:
            logger.error(f"Google Proxy Error: {e}")
            return []

    def _search_ddg(self, query):
        """DuckDuckGo fallback using the ddgs library, then raw HTML."""
        # Try the library first
        try:
            from duckduckgo_search import DDGS
            with DDGS() as ddgs:
                raw = list(ddgs.text(f"scribd.com {query}", max_results=20))
                results = []
                seen_ids = set()
                for r in raw:
                    href = r.get("href", "")
                    if "scribd.com" not in href:
                        continue
                    try:
                        if "/doc/" in href:
                            doc_id = href.split("/doc/")[1].split("/")[0]
                        elif "/document/" in href:
                            doc_id = href.split("/document/")[1].split("/")[0]
                        else:
                            continue
                    except Exception:
                        continue
                    if doc_id in seen_ids:
                        continue
                    seen_ids.add(doc_id)
                    title = r.get("title", "").replace(" - Scribd", "").replace(" | Scribd", "")
                    results.append({
                        "id": doc_id,
                        "title": title[:100],
                        "url": href,
                        "thumbnail": self._get_thumbnail_url(doc_id),
                        "description": r.get("body", "")[:200],
                        "source": "scribd",
                    })
                if results:
                    return results
        except Exception as e:
            logger.warning(f"DDG Library Error: {e}")

        # Raw HTML fallback
        try:
            search_url = (
                f"https://html.duckduckgo.com/html/?q=site:scribd.com/doc+{query}"
            )
            resp = requests.get(search_url, headers=self.HEADERS, timeout=10)

            soup = BeautifulSoup(resp.text, "html.parser")
            results = []
            seen_ids = set()

            for a in soup.find_all("a", class_="result__a", href=True):
                href = a["href"]
                if "scribd.com" not in href:
                    continue

                try:
                    if "uddg=" in href:
                        import urllib.parse

                        qs = urllib.parse.parse_qs(urllib.parse.urlparse(href).query)
                        href = qs.get("uddg", [href])[0]

                    if "/doc/" in href:
                        doc_id = href.split("/doc/")[1].split("/")[0]
                    elif "/document/" in href:
                        doc_id = href.split("/document/")[1].split("/")[0]
                    else:
                        continue
                except Exception:
                    continue

                if doc_id in seen_ids:
                    continue
                seen_ids.add(doc_id)

                title = a.get_text(strip=True)
                title = title.replace(" - Scribd", "").replace(" | Scribd", "")

                results.append(
                    {
                        "id": doc_id,
                        "title": title[:100],
                        "url": href,
                        "thumbnail": self._get_thumbnail_url(doc_id),
                        "source": "scribd",
                    }
                )

                if len(results) >= 15:
                    break

            return results
        except Exception as e:
            logger.error(f"DDG Proxy Error: {e}")
            return []

    def get_document_metadata(self, doc_id):
        """
        Fetches metadata for a single document.
        """
        url = f"{self.BASE_URL}/doc/{doc_id}"
        state = self._get_page_state(url)

        if not state:
            return None

        return {}


# Singleton instance
scribd_api = ScribdClient()
