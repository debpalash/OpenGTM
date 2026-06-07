"""
Vendor / cost catalog — single source of truth for provider economics.

Ported from sayanta-ghosh/gtm-engine (vendor_catalog.py + execution/service.py
calculate_cost) and nurturev/gtm-engine (billing/cost_config_service). See
docs/clay-alternatives-ingestion-catalog.md (top-10 #5).

Replaces the bare planner.PROVIDER_COST dict with a catalog that knows, per
provider: base cost/lookup, BYOK-vs-platform, and how cost SCALES with the
operation (search = per-page, bulk = per-record). Lets the workbook show a
"N rows × providers = $X" spend preview before a run, and feeds the planner's
yield÷cost ordering.

Cost sources, merged at lookup time (later wins):
  1. built-in VENDORS table (paid BYOK APIs)
  2. each provider's own `cost_per_lookup` (incl. declarative YAML manifests)
"""

import logging
import math
from dataclasses import dataclass, field
from typing import Dict, List, Optional

logger = logging.getLogger("workbook.vendor_catalog")


@dataclass
class Vendor:
    name: str
    base_cost: float = 0.0          # USD per single successful lookup
    byok: bool = True               # BYOK (user key) vs platform-billed
    capabilities: List[str] = field(default_factory=list)
    # cost scaling for bulk/search operations
    per_page_size: int = 25         # search: 1 unit per this many requested rows
    notes: str = ""


# Built-in paid providers (free OSS providers omit → base_cost 0.0).
# Costs are approximate USD/lookup; refine from each vendor's pricing page.
VENDORS: Dict[str, Vendor] = {
    "hunter_io":        Vendor("hunter_io", 0.04, capabilities=["email"]),
    "apollo_io":        Vendor("apollo_io", 0.03, capabilities=["email", "phone", "contact_person", "company_size"]),
    "snovio":           Vendor("snovio", 0.03, capabilities=["email"]),
    "prospeo":          Vendor("prospeo", 0.02, capabilities=["email", "phone"]),
    "people_data_labs": Vendor("people_data_labs", 0.03, capabilities=["email", "phone", "company_size"]),
    "abstract_api":     Vendor("abstract_api", 0.01, capabilities=["email_verify"]),
    "debounce":         Vendor("debounce", 0.008, capabilities=["email_verify"]),
    "numverify":        Vendor("numverify", 0.005, capabilities=["phone"]),
    "google_maps":      Vendor("google_maps", 0.005, capabilities=["phone", "address"]),
    # declarative-manifest providers (cost comes from the manifest; listed here
    # so the catalog knows they're paid even before the registry loads)
    "leadmagic_email":  Vendor("leadmagic_email", 0.05, capabilities=["email"]),
}


def _provider_declared_cost(name: str) -> Optional[float]:
    """Read cost_per_lookup off a registered provider instance (e.g. a
    declarative manifest), if available. Returns None if not registered."""
    try:
        from apps.api.services.workbook.providers import get_provider
        p = get_provider(name)
        if p is not None:
            c = getattr(p, "cost_per_lookup", 0.0) or 0.0
            return float(c)
    except Exception:
        pass
    return None


def base_cost(name: str) -> float:
    """Per-lookup cost for a provider — manifest/provider value wins over the
    built-in table; unknown/free providers are 0.0."""
    declared = _provider_declared_cost(name)
    if declared is not None and declared > 0:
        return declared
    v = VENDORS.get(name)
    return v.base_cost if v else 0.0


def is_paid(name: str) -> bool:
    return base_cost(name) > 0.0


def is_byok(name: str) -> bool:
    v = VENDORS.get(name)
    return v.byok if v else True


def calculate_cost(name: str, operation: str = "enrich", params: Optional[dict] = None) -> float:
    """Cost of one call, scaled by operation.

    - enrich/verify/find (default): base_cost × 1
    - search: base_cost × ceil(requested_rows / per_page_size)
    - bulk:   base_cost × record_count
    """
    params = params or {}
    cost = base_cost(name)
    if cost <= 0:
        return 0.0
    if operation == "search":
        rows = int(params.get("limit") or params.get("per_page") or params.get("rows") or 25)
        per_page = (VENDORS.get(name).per_page_size if name in VENDORS else 25) or 25
        return cost * max(1, math.ceil(rows / per_page))
    if operation == "bulk":
        return cost * max(1, int(params.get("count") or params.get("records") or 1))
    return cost


def estimate_run_cost(num_rows: int, provider_names_by_column: Dict[str, List[str]]) -> dict:
    """Estimate a workbook run's spend BEFORE running it.

    provider_names_by_column: {column_id: [provider ids in its waterfall]}.
    Worst case assumes every paid provider in a column's chain is tried for
    every row (no cache/early-exit); we also report a best case (cheapest paid
    provider per column hits first). Returns {worst, best, breakdown[]}.
    """
    worst = 0.0
    best = 0.0
    breakdown = []
    for col_id, providers in provider_names_by_column.items():
        paid = [(p, base_cost(p)) for p in providers if is_paid(p)]
        col_worst = sum(c for _, c in paid) * num_rows
        col_best = (min((c for _, c in paid), default=0.0)) * num_rows
        worst += col_worst
        best += col_best
        breakdown.append({
            "column": col_id,
            "paid_providers": [p for p, _ in paid],
            "worst_usd": round(col_worst, 4),
            "best_usd": round(col_best, 4),
        })
    return {
        "rows": num_rows,
        "worst_usd": round(worst, 4),
        "best_usd": round(best, 4),
        "breakdown": breakdown,
        "note": "worst = every paid provider tried per row; best = cheapest paid hit first. "
                "Cross-provider cache + confidence early-exit reduce actual spend.",
    }


def list_vendors() -> List[dict]:
    return [
        {"name": v.name, "base_cost": v.base_cost, "byok": v.byok,
         "capabilities": v.capabilities, "notes": v.notes}
        for v in VENDORS.values()
    ]
