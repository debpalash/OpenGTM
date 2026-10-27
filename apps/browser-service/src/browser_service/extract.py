"""POST /v1/extract: main text and metadata (trafilatura, legacy
leadgen/enrichment/content_extract.py) plus markdown-ish text (markdownify,
legacy services/scraper.py _parse_html), from given HTML or a fetched URL."""

from __future__ import annotations

import asyncio
import logging

from .engines import Deadline, Deps, _check, render
from .errors import ServiceError
from .models import ExtractRequest, ExtractResult, RenderRequest
from .textutil import html_to_text, truncate_utf8

logger = logging.getLogger("browser_service.extract")

# trafilatura is CPU-bound and slow on huge inputs; parse a bounded prefix.
MAX_PARSE_CHARS = 8 << 20


def main_content(html: str, url: str) -> dict | None:
    """Legacy extract_main_content: {text,title,author,date,sitename,description}
    or None when trafilatura finds neither main text nor metadata."""
    from trafilatura import extract, extract_metadata

    try:
        text = extract(
            html,
            url=url or None,
            include_comments=False,
            include_tables=True,
            favor_recall=True,
            output_format="txt",
        )
    except Exception:
        logger.debug("trafilatura extract failed", exc_info=True)
        text = None
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


async def extract(deps: Deps, req: ExtractRequest, deadline: Deadline) -> ExtractResult:
    status = 0
    final_url = req.url or ""
    if req.html is not None:
        html = req.html
    elif req.url:
        _check(deps, req.url)
        page = await render(
            deps,
            RenderRequest(
                url=req.url,
                engine=req.engine,
                include_html=True,
                max_bytes=min(deps.settings.max_response_bytes, 16 << 20),
                timeout_s=max(1.0, deadline.left()),
                upstream_proxy=req.upstream_proxy,
            ),
            deadline,
        )
        html, status, final_url = page.html or "", page.status, page.final_url
    else:
        raise ServiceError("bad_request")

    src = html[:MAX_PARSE_CHARS]
    content = await asyncio.to_thread(main_content, src, final_url)
    md = md_trunc = None
    if req.include_markdown:
        md, md_trunc = truncate_utf8(await asyncio.to_thread(html_to_text, src), req.max_bytes)
    content = content or {}
    # legacy returned a dict whenever trafilatura built a metadata object, even an
    # empty one; "found" means real text or at least one metadata field
    found = any(str(v).strip() for v in content.values())
    text, text_trunc = truncate_utf8(content.get("text", ""), req.max_bytes)
    return ExtractResult(
        found=found,
        text=text,
        markdown=md,
        word_count=len(text.split()),
        title=str(content.get("title", ""))[:1000],
        author=str(content.get("author", ""))[:500],
        date=str(content.get("date", ""))[:64],
        sitename=str(content.get("sitename", ""))[:500],
        description=str(content.get("description", ""))[:2000],
        final_url=final_url,
        status=status,
        text_truncated=text_trunc,
        markdown_truncated=bool(md_trunc),
        elapsed_ms=deadline.elapsed_ms(),
    )
