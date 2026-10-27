"""Runs INSIDE the browser-service container (docker exec -i <c> python - < this).

Serves a static page on 127.0.0.1 (allow-listed via APP_ENV=test +
BROWSER_EGRESS_ALLOW_CIDRS=127.0.0.1/32), renders it through the service and
checks that a redirect to 127.0.0.2 and the metadata IP are refused."""

import http.server
import json
import os
import sys
import threading
import urllib.error
import urllib.request

PAGE = (
    b"<html><head><title>smoke</title></head>"
    b"<body><script>document.body.innerHTML='<p>js-ok</p>'</script></body></html>"
)


class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_GET(self):
        if self.path == "/redir":
            self.send_response(302)
            self.send_header("Location", "http://127.0.0.2:8080/")
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/html")
        self.send_header("Content-Length", str(len(PAGE)))
        self.end_headers()
        self.wfile.write(PAGE)


srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
threading.Thread(target=srv.serve_forever, daemon=True).start()
base = f"http://127.0.0.1:{srv.server_address[1]}"
token = os.environ["BROWSER_SERVICE_TOKEN"]


def call(url):
    req = urllib.request.Request(
        "http://127.0.0.1:8090/v1/render",
        data=json.dumps({"url": url, "include_text": True, "timeout_s": 30}).encode(),
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as r:  # noqa: S310 - fixed local URL
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read())


failures = []
st, body = call(base + "/page")
if st != 200 or "js-ok" not in (body.get("text") or "") or body.get("title") != "smoke":
    failures.append(f"render: {st} {body}")
for url in (base + "/redir", "http://169.254.169.254/latest/meta-data/"):
    st, body = call(url)
    if st != 403 or body["error"]["code"] != "egress_blocked":
        failures.append(f"{url}: {st} {body}")
print("SMOKE OK" if not failures else "SMOKE FAIL\n" + "\n".join(failures))
sys.exit(1 if failures else 0)
