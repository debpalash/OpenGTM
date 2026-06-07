"""
SSRF url-guard (#7): blocks private/loopback/metadata + encoded-IP smuggling.
"""
import pytest
from apps.api.core.url_guard import check_url, is_safe_url, BlockedUrlError


def test_allows_public_https():
    assert check_url("https://api.hunter.io/v2/domain-search") is not None
    assert is_safe_url("https://example.com/path?q=1")

def test_blocks_non_http_scheme():
    for u in ("file:///etc/passwd", "ftp://x.com", "gopher://x", "data:text/plain,hi"):
        assert not is_safe_url(u)

def test_blocks_loopback_and_private():
    for u in ("http://127.0.0.1/", "http://localhost/", "https://10.0.0.5/x",
              "http://192.168.1.1/", "http://172.16.0.1/", "http://[::1]/"):
        assert not is_safe_url(u), u

def test_blocks_cloud_metadata():
    assert not is_safe_url("http://169.254.169.254/latest/meta-data/")
    assert not is_safe_url("http://metadata.google.internal/")

def test_blocks_encoded_ip_smuggling():
    # all of these resolve to 127.0.0.1 / private space
    for u in (
        "http://2130706433/",        # decimal 127.0.0.1
        "http://0x7f000001/",        # hex
        "http://0177.0.0.1/",        # octal first octet
        "http://0x7f.0.0.1/",        # hex octet
        "http://[::ffff:10.0.0.1]/", # IPv4-mapped IPv6
    ):
        assert not is_safe_url(u), u

def test_http_can_be_disallowed():
    assert is_safe_url("http://example.com", allow_http=True)
    assert not is_safe_url("http://example.com", allow_http=False)

def test_raises_with_reason():
    with pytest.raises(BlockedUrlError):
        check_url("http://127.0.0.1/")
