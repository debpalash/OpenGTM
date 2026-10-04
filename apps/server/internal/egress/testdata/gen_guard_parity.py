"""Record apps/api/core/url_guard.py decisions for the Go egress guard.

Run from the repository root:

    uv run python apps/server/internal/egress/testdata/gen_guard_parity.py

Each URL is checked with check_url(url, allow_http=True) (no DNS). The Go
test requires the same verdict, except for a documented list where Go is
deliberately stricter (CGNAT, site-local, inet_aton short forms, NAT64/6to4
embedded private IPv4, *.localhost).
"""

import json
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[4]
sys.path.insert(0, str(ROOT))

from apps.api.core.url_guard import BlockedUrlError, check_url  # noqa: E402

URLS = [
    # allowed
    "https://example.com/", "http://example.com/path?q=1", "https://api.leadmagic.io/email-finder",
    "https://93.184.216.34/", "https://[2606:4700:4700::1111]/", "https://8.8.8.8:8443/x",
    "https://xn--caf-dma.example/", "https://sub.domain.example.org/a/b",
    # schemes
    "ftp://example.com/", "file:///etc/passwd", "gopher://example.com/", "javascript:alert(1)",
    "data:text/plain,hi", "//example.com/", "example.com", "", "   ",
    # hostnames
    "http://localhost/", "http://LOCALHOST:8000/", "http://ip6-localhost/", "http://ip6-loopback/",
    "http://metadata.google.internal/computeMetadata/v1/", "http://metadata/", "https:///nohost",
    # IPv4 private / special
    "http://127.0.0.1/", "http://127.255.255.254/", "http://10.0.0.1/", "http://172.16.5.4/",
    "http://172.31.255.255/", "http://172.32.0.1/", "http://192.168.1.1/", "http://169.254.169.254/latest/meta-data/",
    "http://169.254.1.1/", "http://0.0.0.0/", "http://224.0.0.1/", "http://239.255.255.250/",
    "http://240.0.0.1/", "http://255.255.255.255/", "http://192.0.2.1/", "http://198.18.0.1/",
    "http://198.51.100.7/", "http://203.0.113.9/", "http://100.64.0.1/", "http://100.127.255.254/",
    # encodings
    "http://2130706433/", "http://0x7f000001/", "http://017700000001/", "http://0177.0.0.1/",
    "http://0x7f.0.0.1/", "http://0x7f.0x0.0x0.0x1/", "http://127.000.000.001/", "http://3232235777/",
    "http://4294967295/", "http://08.0.0.1/", "http://127.1/", "http://0/",
    # IPv6
    "http://[::1]/", "http://[::]/", "http://[fe80::1]/", "http://[fc00::1]/", "http://[fd12:3456::1]/",
    "http://[ff02::1]/", "http://[::ffff:127.0.0.1]/", "http://[::ffff:10.0.0.1]/", "http://[::ffff:8.8.8.8]/",
    "http://[fd00:ec2::254]/", "http://[2001:db8::1]/", "http://[64:ff9b::a00:1]/", "http://[2002:a00:1::]/",
    "http://[fec0::1]/", "http://[100::1]/",
]


def main():
    out = []
    for url in URLS:
        try:
            check_url(url, allow_http=True)
            out.append({"url": url, "blocked": False})
        except BlockedUrlError as exc:
            out.append({"url": url, "blocked": True, "reason": str(exc)})
    (HERE / "guard_parity.json").write_text(json.dumps(out, indent=1) + "\n", encoding="utf-8")
    print(f"wrote {len(out)} guard decisions")


if __name__ == "__main__":
    main()
