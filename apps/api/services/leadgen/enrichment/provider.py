"""
Waterfall Enrichment Engine — Provider abstraction + waterfall execution.

Inspired by YALC's provider manifest system and Clay's waterfall enrichment.
Each provider is a self-contained module that can enrich a lead with specific
fields. The WaterfallEnricher chains multiple providers and stops at the first
successful result for each field.
"""

import asyncio
import json
import logging
from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional, Tuple

from apps.api.services.leadgen.models import Lead
from apps.api.services.leadgen.enrichment.validate import validate_field

logger = logging.getLogger("leadgen.waterfall")


# ── Result Types ─────────────────────────────────────────────────

@dataclass
class EnrichmentResult:
    """Result from a single provider enrichment attempt."""
    provider: str = ""
    success: bool = False
    fields: Dict[str, Any] = field(default_factory=dict)
    confidence: float = 0.0
    error: str = ""
    duration_ms: float = 0.0

    def has_value(self, field_name: str) -> bool:
        val = self.fields.get(field_name)
        return val is not None and val != "" and val != []

    def get(self, field_name: str, default: Any = None) -> Any:
        return self.fields.get(field_name, default)


@dataclass
class WaterfallLog:
    """Log of all provider attempts for a single field."""
    field_name: str
    attempts: List[Dict[str, Any]] = field(default_factory=list)
    winner: str = ""
    final_value: Any = None
    final_confidence: float = 0.0

    def to_dict(self) -> dict:
        return {
            "field": self.field_name,
            "attempts": self.attempts,
            "winner": self.winner,
            "value": str(self.final_value)[:50] if self.final_value else None,
            "confidence": self.final_confidence,
        }


# ── Abstract Provider ────────────────────────────────────────────

class EnrichmentProvider(ABC):
    """Base class for all enrichment providers.

    Each provider declares:
    - name: unique identifier (e.g. "crosslinked", "mailscout")
    - capabilities: list of fields it can provide (e.g. ["email", "phone"])
    - default_confidence: baseline confidence for this provider's results

    DATA CONTRACT — Cell Value Rules:
      ┌──────────────────────────────────────────────────────────────────┐
      │ Every value in EnrichmentResult.fields MUST be a flat scalar    │
      │ (string, number, bool). JSON arrays/objects MUST be stored as   │
      │ JSON strings in their designated Lead fields (decision_makers,  │
      │ hiring_signals, etc.) but NEVER surfaced directly in a workbook │
      │ cell.                                                           │
      │                                                                 │
      │ The enrichment engine enforces this: structured fields are      │
      │ written back to the Lead record, and workbook cells receive     │
      │ only the targeted scalar field (e.g. email, phone, name).       │
      │                                                                 │
      │ If a provider returns {"decision_makers": "[{...}]",            │
      │   "contact_person": "John"}, only "contact_person" will appear  │
      │   in the cell. "decision_makers" writes to the Lead record.     │
      └──────────────────────────────────────────────────────────────────┘
    """

    name: str = "base"
    capabilities: List[str] = []
    default_confidence: float = 0.5

    # ── Pillar 2: cost metadata (defaults = free OSS provider) ──
    requires_api_key: bool = False
    free_tier_limit: int = 0       # 0 = unlimited
    cost_per_lookup: float = 0.0   # USD per successful lookup (override or see planner.PROVIDER_COST)

    @abstractmethod
    async def enrich(self, lead: Lead) -> EnrichmentResult:
        """Run enrichment for a lead. Return an EnrichmentResult with discovered fields."""
        ...

    def can_provide(self, field_name: str) -> bool:
        return field_name in self.capabilities


# ── Waterfall Executor ───────────────────────────────────────────

class WaterfallEnricher:
    """Chain multiple providers for each field, stop at first success.

    Usage:
        waterfall = WaterfallEnricher()
        waterfall.register_chain("email", [
            WebsiteCrawlProvider(),
            PatternEmailProvider(),
            MailScoutVerifyProvider(),
            DDGSearchProvider(),
        ])
        result = await waterfall.enrich(lead)
    """

    # Stop the chain as soon as a result clears this confidence (we trust it
    # enough to not spend more providers). Ported from masteranime/enrichment-kit
    # (earlyExitOnConfidence). Below this, we keep going and remember best-of-N.
    EARLY_EXIT_CONFIDENCE: float = 0.85

    def __init__(self):
        self._chains: Dict[str, List[EnrichmentProvider]] = {}
        self._timeout: float = 15.0  # per-provider timeout in seconds

    def register_chain(self, field_name: str, providers: List[EnrichmentProvider]):
        """Register a chain of providers for a specific field."""
        self._chains[field_name] = providers

    async def enrich(self, lead: Lead) -> Tuple[Dict[str, Any], List[WaterfallLog]]:
        """Run all registered waterfall chains for a lead.

        Returns:
            - dict of {field_name: best_value}
            - list of WaterfallLog for provenance tracking
        """
        results: Dict[str, Any] = {}
        logs: List[WaterfallLog] = []

        # company domain (for the email accept-gate) — best-effort from the lead
        company_domain = None
        for attr in ("website", "domain", "company_domain"):
            v = getattr(lead, attr, None)
            if v:
                company_domain = v
                break

        for field_name, providers in self._chains.items():
            log = WaterfallLog(field_name=field_name)
            # Best-of-N: keep the highest-confidence VALID result seen so far,
            # short-circuit when one clears EARLY_EXIT_CONFIDENCE. (Was: accept
            # and break on the first non-empty value, ignoring confidence/quality.)
            best_value = None
            best_conf = -1.0

            for provider in providers:
                attempt = {"provider": provider.name, "success": False}
                try:
                    result = await asyncio.wait_for(
                        provider.enrich(lead),
                        timeout=self._timeout,
                    )
                    attempt["duration_ms"] = result.duration_ms

                    if result.has_value(field_name):
                        value = result.get(field_name)
                        # Accept-gate: reject role/placeholder/sentinel/mismatch
                        # values so we don't store or stop on garbage.
                        if not validate_field(field_name, value, company_domain=company_domain):
                            attempt["error"] = "rejected_invalid"
                            log.attempts.append(attempt)
                            continue

                        conf = result.confidence or provider.default_confidence
                        attempt["success"] = True
                        attempt["confidence"] = conf
                        log.attempts.append(attempt)

                        if conf > best_conf:
                            best_value, best_conf = value, conf
                            log.winner = provider.name
                            log.final_value = value
                            log.final_confidence = conf

                        if conf >= self.EARLY_EXIT_CONFIDENCE:
                            break  # confident enough — stop spending providers
                        continue
                    else:
                        attempt["error"] = result.error or "no_data"

                except asyncio.TimeoutError:
                    attempt["error"] = "timeout"
                except Exception as e:
                    attempt["error"] = str(e)[:100]

                log.attempts.append(attempt)

            if best_value is not None:
                results[field_name] = best_value

            logs.append(log)

        return results, logs

    def apply_results(self, lead: Lead, results: Dict[str, Any], logs: List[WaterfallLog]):
        """Apply waterfall results to a Lead object."""
        field_map = {
            "email": "email",
            "phone": "phone",
            "website": "website",
            "linkedin_url": "linkedin_url",
            "contact_person": "contact_person",
            "contact_title": "contact_title",
            "decision_makers": "decision_makers",
            "hiring_signals": "hiring_signals",
        }

        for field_name, value in results.items():
            lead_attr = field_map.get(field_name, field_name)
            if hasattr(lead, lead_attr):
                current = getattr(lead, lead_attr)
                # Only overwrite if current is empty or new value has higher confidence
                if not current or current in ("", "N/A", "nan"):
                    setattr(lead, lead_attr, value)

        # Track which providers found what
        for log in logs:
            if log.winner:
                provider_attr = f"{log.field_name}_provider"
                if hasattr(lead, provider_attr):
                    setattr(lead, provider_attr, log.winner)

        # Store full waterfall log as JSON
        if hasattr(lead, "enrichment_waterfall"):
            lead.enrichment_waterfall = json.dumps(
                [l.to_dict() for l in logs], default=str
            )
        if hasattr(lead, "enrichment_attempts"):
            lead.enrichment_attempts = sum(len(l.attempts) for l in logs)
