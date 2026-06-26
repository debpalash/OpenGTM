"""
Per-email verification cache — hashing, get/set, definitive vs unknown TTL, and
lazy expiry. Uses a throwaway SQLite file (never the shared cache db).
"""
import time

from apps.api.services.leadgen.enrichment.email_verify_cache import (
    EmailVerifyCache, email_key, DEFINITIVE_TTL_DAYS, UNKNOWN_TTL_DAYS,
)


def _cache(tmp_path):
    return EmailVerifyCache(db_path=str(tmp_path / "verify_cache.db"))


def test_key_hashes_email_case_insensitively():
    assert email_key("A@B.com", "reacher") == email_key("a@b.com  ", "reacher")
    # plaintext never appears in the key
    assert "a@b.com" not in email_key("a@b.com", "reacher")
    # verifier name is part of the key space
    assert email_key("a@b.com", "reacher") != email_key("a@b.com", "smtp")


def test_set_get_roundtrip(tmp_path):
    c = _cache(tmp_path)
    c.set("a@b.com", "reacher", "valid", source="reacher", detail="ok", confidence=0.95)
    hit = c.get("a@b.com", "reacher")
    assert hit["status"] == "valid" and hit["source"] == "reacher"
    assert hit["detail"] == "ok" and hit["confidence"] == 0.95


def test_miss_returns_none(tmp_path):
    assert _cache(tmp_path).get("nobody@nowhere.com", "reacher") is None


def test_definitive_gets_long_ttl(tmp_path):
    c = _cache(tmp_path)
    before = time.time()
    c.set("a@b.com", "reacher", "valid")
    row = c._conn.execute(
        "SELECT expires_at FROM email_verify_cache WHERE cache_key=?",
        (email_key("a@b.com", "reacher"),),
    ).fetchone()
    ttl = row[0] - before
    assert abs(ttl - DEFINITIVE_TTL_DAYS * 86400) < 5


def test_unknown_gets_short_ttl(tmp_path):
    c = _cache(tmp_path)
    before = time.time()
    c.set("a@b.com", "reacher", "unknown")
    row = c._conn.execute(
        "SELECT expires_at FROM email_verify_cache WHERE cache_key=?",
        (email_key("a@b.com", "reacher"),),
    ).fetchone()
    ttl = row[0] - before
    assert abs(ttl - UNKNOWN_TTL_DAYS * 86400) < 5


def test_lazy_expiry_returns_none(tmp_path):
    c = _cache(tmp_path)
    c.set("a@b.com", "reacher", "valid", ttl_days=-1)  # already expired
    assert c.get("a@b.com", "reacher") is None


def test_purge_expired(tmp_path):
    c = _cache(tmp_path)
    c.set("a@b.com", "reacher", "valid", ttl_days=-1)
    c.set("c@d.com", "reacher", "valid")
    assert c.purge_expired() == 1
    assert c.get("c@d.com", "reacher") is not None
