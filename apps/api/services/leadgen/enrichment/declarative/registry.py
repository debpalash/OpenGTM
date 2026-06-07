"""
Capability registry — resolve a capability (e.g. "email") to a concrete,
available provider via an ordered priority list.

Ported from YALC's capabilities.ts. Skills/columns declare WHAT they need
(a capability); the registry picks the first provider in the priority list whose
`is_available()` is true. Priority can be overridden per workspace.

This composes with the existing workbook provider registry: declarative
(YAML-manifest) providers are auto-registered there at import time, so the
waterfall and this capability registry share one provider pool.
"""

import logging
from typing import Dict, List, Optional

from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider

logger = logging.getLogger("leadgen.declarative.registry")


class CapabilityUnsatisfied(Exception):
    def __init__(self, capability: str, tried: List[str]):
        self.capability = capability
        self.tried = tried
        super().__init__(
            f"No available provider for capability '{capability}'. "
            f"Tried (in priority order): {tried or '[]'}. "
            f"Configure an API key for one of them."
        )


# capability -> ordered provider ids (default priority). Overridable per workspace.
_DEFAULT_PRIORITY: Dict[str, List[str]] = {}


def set_default_priority(capability: str, provider_ids: List[str]):
    _DEFAULT_PRIORITY[capability] = list(provider_ids)


def get_default_priority(capability: str) -> List[str]:
    return list(_DEFAULT_PRIORITY.get(capability, []))


def resolve(
    capability: str,
    *,
    priority: Optional[List[str]] = None,
    workspace_overrides: Optional[Dict[str, List[str]]] = None,
) -> EnrichmentProvider:
    """Return the first available provider for `capability`.

    Order: explicit `priority` arg → per-workspace override → default priority →
    any registered provider that can_provide(capability). Raises
    CapabilityUnsatisfied with the ordered list it tried if none is available.
    """
    from apps.api.services.workbook.providers import get_provider, get_providers_for_capability

    order = (
        priority
        or (workspace_overrides or {}).get(capability)
        or get_default_priority(capability)
    )

    tried: List[str] = []
    # 1) explicit priority order
    for pid in order:
        tried.append(pid)
        p = get_provider(pid)
        if p and _available(p) and p.can_provide(capability):
            return p
    # 2) fall back to any capable + available provider
    for p in get_providers_for_capability(capability):
        if p.name in tried:
            continue
        tried.append(p.name)
        if _available(p):
            return p

    raise CapabilityUnsatisfied(capability, tried)


def resolve_chain(
    capability: str,
    *,
    priority: Optional[List[str]] = None,
    workspace_overrides: Optional[Dict[str, List[str]]] = None,
) -> List[EnrichmentProvider]:
    """Return ALL available providers for a capability, in priority order.

    This is what the waterfall wants: try each in turn (best-of-N / early-exit),
    not just the single first-available one.
    """
    from apps.api.services.workbook.providers import get_provider, get_providers_for_capability

    order = (
        priority
        or (workspace_overrides or {}).get(capability)
        or get_default_priority(capability)
    )
    chain: List[EnrichmentProvider] = []
    seen = set()
    for pid in order:
        p = get_provider(pid)
        if p and p.name not in seen and _available(p) and p.can_provide(capability):
            chain.append(p)
            seen.add(p.name)
    for p in get_providers_for_capability(capability):
        if p.name not in seen and _available(p):
            chain.append(p)
            seen.add(p.name)
    return chain


def _available(provider: EnrichmentProvider) -> bool:
    """A provider is available unless it declares otherwise (declarative
    providers check their API key)."""
    fn = getattr(provider, "is_available", None)
    if callable(fn):
        try:
            return bool(fn())
        except Exception:
            return False
    return True


def register_declarative_manifests() -> int:
    """Load all YAML manifests, compile them, and register into the workbook
    provider registry. Returns the count registered."""
    from apps.api.services.leadgen.enrichment.declarative.manifest import load_all_manifests
    from apps.api.services.leadgen.enrichment.declarative.compiler import compile_manifest
    from apps.api.services.workbook.providers import register_provider

    count = 0
    for manifest in load_all_manifests():
        try:
            register_provider(compile_manifest(manifest))
            count += 1
        except Exception as e:
            logger.warning(f"failed to register manifest {manifest.name}: {e}")
    if count:
        logger.info(f"registered {count} declarative provider(s) from manifests")
    return count
