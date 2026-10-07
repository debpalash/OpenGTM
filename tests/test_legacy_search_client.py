"""Legacy pipeline helpers must use the configured native search client."""

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

import pytest

from apps.api.services.leadgen.enrichment import search_enricher, social_finder, web_search
from apps.api.services.leadgen.models import Lead


@pytest.fixture
def search_pool(monkeypatch):
    queries = []

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            query = parse_qs(urlparse(self.path).query)["q"][0]
            queries.append(query)
            if "linkedin.com" in query:
                url = "https://www.linkedin.com/company/acme"
            elif "twitter.com" in query:
                url = "https://x.com/acme"
            else:
                url = "https://acme.test"
            body = json.dumps({"results": [{
                "url": url, "title": "Acme", "content": "Contact sales@acme.test +91 98765 43210",
            }]}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *_args):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    endpoint = f"http://127.0.0.1:{server.server_port}"
    monkeypatch.setattr(web_search, "_POOL_URL", endpoint)
    monkeypatch.setattr(web_search, "_SEARXNG_URL", endpoint)
    monkeypatch.setattr(web_search, "_DISABLED", False)
    try:
        yield queries
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def test_social_finder_uses_configured_search_pool(search_pool):
    lead = Lead(company="Acme")
    leads = [lead]
    assert social_finder.find_social_profiles(leads, delay=0) is leads
    assert lead.linkedin_url == "https://www.linkedin.com/company/acme"
    assert lead.twitter_url == "https://x.com/acme"
    assert len(search_pool) == 2


def test_search_enricher_uses_configured_search_pool(search_pool):
    lead = Lead(company="Acme", city="Pune")
    leads = [lead]
    assert search_enricher.enrich_via_search(leads, delay=0) is leads
    assert lead.website == "https://acme.test"
    assert lead.email == "sales@acme.test"
    assert lead.phone == "+91 98765 43210"
    assert len(search_pool) == 1
