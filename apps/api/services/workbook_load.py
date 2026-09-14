"""Controlled, self-cleaning scale gate for workbook paging and selection."""

import math
import time
import uuid

from sqlalchemy import text

from apps.api.database import SessionLocal, engine
from apps.api.services.workbook.models import Workbook, WorkbookRow


CONFIRMATION = "RUN WORKBOOK LOAD TEST"


def _timed(db, statement, params=None):
    started = time.perf_counter()
    rows = db.execute(statement, params or {}).all()
    return rows, (time.perf_counter() - started) * 1000


def run_workbook_load_test(
    *,
    rows: int = 1_000_000,
    page_size: int = 100,
    confirmation: str = "",
    max_page_ms: float = 2_000,
    max_search_ms: float = 5_000,
    max_selection_ms: float = 2_000,
    allow_sqlite: bool = False,
) -> dict:
    """Validate indexed paging, search, and distant exact-row selection."""
    if confirmation != CONFIRMATION:
        raise ValueError(f"confirmation must equal {CONFIRMATION}")
    dialect = engine.dialect.name
    if dialect != "postgresql" and not allow_sqlite:
        raise RuntimeError("controlled workbook load validation requires PostgreSQL")
    maximum = 2_000_000 if dialect == "postgresql" else 20_000
    if not 100 <= rows <= maximum or not 1 <= page_size <= 5_000:
        raise ValueError("load parameters are outside safe bounds")

    run_id = uuid.uuid4().hex
    workspace_id = f"workbook-load-{run_id}"
    workbook_id = f"workbook-load-{run_id}"
    created = False
    try:
        with SessionLocal() as db:
            db.add(Workbook(
                id=workbook_id,
                workspace_id=workspace_id,
                name="Controlled workbook load gate",
                source_type="empty",
                columns_config=[
                    {"id": "company", "type": "lead_field", "lead_field": "company"},
                    {"id": "score", "type": "lead_field", "lead_field": "score"},
                ],
            ))
            db.commit()
            created = True
            if dialect == "postgresql":
                db.execute(text("""
                    INSERT INTO workbook_rows
                        (workspace_id, workbook_id, position, data, enrichments,
                         corroboration_count)
                    SELECT :workspace_id, :workbook_id, n - 1,
                           jsonb_build_object(
                               'company', 'Scale Company ' || lpad(n::text, 9, '0'),
                               'score', lpad((n % 1000)::text, 4, '0'),
                               'notes', CASE WHEN n = :needle THEN :needle_text ELSE '' END
                           ), '{}'::jsonb, 1
                    FROM generate_series(1, :rows) AS n
                """), {
                    "workspace_id": workspace_id, "workbook_id": workbook_id,
                    "rows": rows, "needle": math.ceil(rows / 2),
                    "needle_text": f"scale-needle-{run_id}",
                })
            else:
                db.bulk_insert_mappings(WorkbookRow, [
                    {
                        "workspace_id": workspace_id,
                        "workbook_id": workbook_id,
                        "position": n - 1,
                        "data": {
                            "company": f"Scale Company {n:09d}",
                            "score": f"{n % 1000:04d}",
                            "notes": f"scale-needle-{run_id}" if n == math.ceil(rows / 2) else "",
                        },
                        "enrichments": {},
                        "corroboration_count": 1,
                    }
                    for n in range(1, rows + 1)
                ])
            db.commit()
            db.execute(text("ANALYZE workbook_rows"))

            base = {"workbook_id": workbook_id, "limit": page_size}
            first, first_ms = _timed(db, text("""
                SELECT id, position FROM workbook_rows
                WHERE workbook_id = :workbook_id
                ORDER BY position, id LIMIT :limit
            """), base)
            last, last_ms = _timed(db, text("""
                SELECT id, position FROM workbook_rows
                WHERE workbook_id = :workbook_id
                ORDER BY position DESC, id DESC LIMIT :limit
            """), base)
            custom, custom_ms = _timed(db, text("""
                SELECT id, COALESCE(data ->> 'company', '') AS company
                FROM workbook_rows WHERE workbook_id = :workbook_id
                ORDER BY COALESCE(data ->> 'company', ''), position, id LIMIT :limit
            """), base) if dialect == "postgresql" else (first, first_ms)

            selected_ids = [first[0].id, last[0].id, custom[-1].id]
            selected, selection_ms = _timed(db, text("""
                SELECT id, position FROM workbook_rows
                WHERE workbook_id = :workbook_id AND id IN (:first, :last, :custom)
                ORDER BY id
            """), {**base, "first": selected_ids[0], "last": selected_ids[1], "custom": selected_ids[2]})
            needle = f"scale-needle-{run_id}"
            if dialect == "postgresql":
                search, search_ms = _timed(db, text("""
                    SELECT id FROM workbook_rows
                    WHERE workbook_id = :workbook_id
                      AND lower(CAST(data AS text)) LIKE :needle
                """), {"workbook_id": workbook_id, "needle": f"%{needle}%"})
            else:
                search, search_ms = _timed(db, text("""
                    SELECT id FROM workbook_rows
                    WHERE workbook_id = :workbook_id AND lower(CAST(data AS text)) LIKE :needle
                """), {"workbook_id": workbook_id, "needle": f"%{needle}%"})

            page_latencies = [first_ms, last_ms, custom_ms]
            report = {
                "ok": (
                    len(first) == page_size
                    and len(last) == page_size
                    and len(selected) == len(set(selected_ids))
                    and {item.id for item in selected} == set(selected_ids)
                    and len(search) == 1
                    and max(page_latencies) <= max_page_ms
                    and search_ms <= max_search_ms
                    and selection_ms <= max_selection_ms
                ),
                "run_id": run_id,
                "dialect": dialect,
                "rows": rows,
                "page_size": page_size,
                "selected_positions": sorted(item.position for item in selected),
                "selection_exact": {item.id for item in selected} == set(selected_ids),
                "search_matches": len(search),
                "latency_ms": {
                    "first_page": round(first_ms, 3),
                    "last_page": round(last_ms, 3),
                    "custom_sort_page": round(custom_ms, 3),
                    "exact_selection": round(selection_ms, 3),
                    "needle_search": round(search_ms, 3),
                },
                "threshold_ms": {
                    "page": max_page_ms,
                    "search": max_search_ms,
                    "selection": max_selection_ms,
                },
            }
            if not report["ok"]:
                raise RuntimeError(f"workbook load validation failed: {report}")
            return report
    finally:
        if created:
            with SessionLocal() as db:
                db.query(Workbook).filter(Workbook.id == workbook_id).delete()
                db.commit()
