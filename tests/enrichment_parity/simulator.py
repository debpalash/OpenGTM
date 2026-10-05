"""Deterministic HTTPS provider simulator for the enrichment parity harness.

One simulator serves both executors in turn, so the request log it keeps is
direct evidence that Python and Go sent the same requests. Behaviour is chosen
by the ``domain`` the row supplies (query string or JSON body):

    ok.example         200 with every field
    nomatch.example    200 with an empty ``data`` object
    empty.example      200 with an empty-string email
    err500.example     500
    ratelimit.example  429
    badjson.example    200, text/plain
    envelope.example   200 with the vendor error envelope ({"error": true})
    slow.example       200 after 3.5 s (exceeds the 2 s provider timeout)
    flaky.example      500 on the first request to a path, 200 afterwards
    anything else      200 with email info@<domain> and a derived phone

It serves TLS because manifest v1 connectors must be https; the self-signed
certificate is trusted through SSL_CERT_FILE by both runtimes.
"""
from __future__ import annotations

import datetime
import json
import ssl
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

LOGGED_HEADERS = ("x-api-key", "authorization", "content-type")


def make_certificate(directory: Path) -> tuple[Path, Path]:
    from cryptography import x509
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import rsa
    from cryptography.x509.oid import NameOID
    import ipaddress

    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "opengtm-parity-simulator")])
    now = datetime.datetime.now(datetime.timezone.utc)
    cert = (
        x509.CertificateBuilder()
        .subject_name(name).issuer_name(name).public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(minutes=5))
        .not_valid_after(now + datetime.timedelta(days=1))
        .add_extension(x509.SubjectAlternativeName([x509.IPAddress(ipaddress.ip_address("127.0.0.1"))]), critical=False)
        .add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True)
        .sign(key, hashes.SHA256())
    )
    cert_path, key_path = directory / "sim.pem", directory / "sim.key"
    cert_path.write_bytes(cert.public_bytes(serialization.Encoding.PEM))
    key_path.write_bytes(key.private_bytes(
        serialization.Encoding.PEM, serialization.PrivateFormat.TraditionalOpenSSL, serialization.NoEncryption()))
    return cert_path, key_path


def data_for(kind: str, domain: str) -> dict:
    base = {
        "email": f"info@{domain}",
        "phone": "+1 (555) 010-0100",
        "city": "Springfield",
        "people_json": json.dumps([{"name": "Jane Doe", "email": f"jane@{domain}"}, {"name": "Bob"}]),
    }
    if domain == "ok.example":
        base["email"] = "hello@ok.example"
    if kind == "email":
        return {"email": base["email"]}
    if kind == "phone":
        return {"phone": base["phone"]}
    if kind == "blob":
        return {"people_json": base["people_json"]}
    return base  # both / paid / keyed


class Simulator:
    def __init__(self, directory: Path, expected_key: str = "sim-secret-key"):
        self.expected_key = expected_key
        self.cert, key = make_certificate(directory)
        self._lock = threading.Lock()
        self._log: list[dict] = []
        self._seen: dict[str, int] = {}
        sim = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):  # silence
                pass

            def _serve(self, method: str):
                parts = urlsplit(self.path)
                query = {k: v[0] for k, v in parse_qs(parts.query, keep_blank_values=True).items()}
                length = int(self.headers.get("content-length") or 0)
                raw = self.rfile.read(length).decode() if length else ""
                body = {}
                if raw:
                    try:
                        body = json.loads(raw)
                    except ValueError:
                        body = {}
                headers = {h: self.headers.get(h) for h in LOGGED_HEADERS if self.headers.get(h) is not None}
                with sim._lock:
                    sim._log.append({"method": method, "path": parts.path, "query": query,
                                     "body": raw, "headers": headers})
                status, ctype, payload, delay = sim.respond(method, parts.path, query, body, headers)
                if delay:
                    time.sleep(delay)
                data = payload.encode()
                self.send_response(status)
                self.send_header("content-type", ctype)
                self.send_header("content-length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_GET(self):
                self._serve("GET")

            def do_POST(self):
                self._serve("POST")

        class QuietServer(ThreadingHTTPServer):
            daemon_threads = True

            def handle_error(self, request, client_address):  # clients that time out hang up: expected
                pass

        self.server = QuietServer(("127.0.0.1", 0), Handler)
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(self.cert, key)
        self.server.socket = ctx.wrap_socket(self.server.socket, server_side=True)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @property
    def base(self) -> str:
        return f"https://127.0.0.1:{self.server.server_address[1]}"

    def reset(self) -> None:
        with self._lock:
            self._log.clear()
            self._seen.clear()

    def requests(self) -> list[dict]:
        with self._lock:
            return list(self._log)

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()

    def respond(self, method, path, query, body, headers):
        kind = path.strip("/").split("/")
        domain = query.get("domain") or (body.get("domain") if isinstance(body, dict) else "") or ""
        if (kind[0] == "keyed" and headers.get("x-api-key") != self.expected_key) or (
                kind[0] == "qk" and query.get("key") != self.expected_key):
            return 401, "application/json", json.dumps({"error": "bad key"}), 0
        key = f"{path}|{domain}"
        with self._lock:
            self._seen[key] = self._seen.get(key, 0) + 1
            nth = self._seen[key]
        if domain == "err500.example" or (domain == "flaky.example" and nth == 1):
            return 500, "application/json", json.dumps({"error": "boom"}), 0
        if domain == "ratelimit.example":
            return 429, "application/json", json.dumps({"error": "slow down"}), 0
        if domain == "badjson.example":
            return 200, "text/plain", "this is not json", 0
        if domain == "envelope.example":
            return 200, "application/json", json.dumps({"error": True, "message": "quota exceeded"}), 0
        if domain == "nomatch.example":
            return 200, "application/json", json.dumps({"data": {}}), 0
        if domain == "empty.example":
            return 200, "application/json", json.dumps({"data": {"email": "", "phone": ""}}), 0
        delay = 3.5 if domain == "slow.example" else 0
        data = data_for(kind[-1] if kind[-1] in {"email", "phone", "blob", "both"} else "both", domain)
        return 200, "application/json", json.dumps({"data": data}), delay
