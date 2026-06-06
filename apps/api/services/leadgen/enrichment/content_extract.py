"""
Clean-content extraction — zero-LLM-cost Tier-4 quality pass (trafilatura).

trafilatura (research/ai-extraction/trafilatura) extracts the main article/body
text and metadata (title, author, date, sitename, description) from messy HTML far
more reliably than regex — and without any LLM spend. Use it before paying for an
LLM extraction pass: feed `extract_main_content(html)["text"]` to the LLM instead of
raw HTML to cut tokens and noise.

ACTIVATION: `pip install trafilatura`. Optional dependency — when absent,
`is_available()` is False and `extract_main_content` returns None so callers fall
back to their existing extraction.
"""

import logging
from typing import Dict, Optional

logger = logging.getLogger("leadgen.content_extract")

_AVAILABLE: Optional[bool] = None


def is_available() -> bool:
    global _AVAILABLE
    if _AVAILABLE is None:
        try:
            import trafilatura  # noqa: F401
            _AVAILABLE = True
        except Exception:
            _AVAILABLE = False
    return _AVAILABLE


def extract_main_content(html: str, url: str = "") -> Optional[Dict[str, str]]:
    """Return {text, title, author, date, sitename, description} or None.

    None means trafilatura is unavailable or found no main content — callers should
    fall back to their current (regex/LLM) extraction.
    """
    if not html or not is_available():
        return None
    try:
        import trafilatura
        from trafilatura import extract, extract_metadata

        text = extract(
            html, url=url or None,
            include_comments=False, include_tables=True,
            favor_recall=True, output_format="txt",
        )
        meta = None
        try:
            meta = extract_metadata(html, default_url=url or None)
        except Exception:
            meta = None

        if not text and not meta:
            return None

        return {
            "text": (text or "").strip(),
            "title": getattr(meta, "title", "") or "",
            "author": getattr(meta, "author", "") or "",
            "date": getattr(meta, "date", "") or "",
            "sitename": getattr(meta, "sitename", "") or "",
            "description": getattr(meta, "description", "") or "",
        }
    except Exception as e:
        logger.debug("trafilatura extraction failed for %s: %s", url, e)
        return None
