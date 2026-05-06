"""
Provider Registry — Maps provider names to EnrichmentProvider instances.

This is the glue between the Workbook column config (which stores provider names
as strings) and the actual provider implementations. When a workbook cell needs
enrichment, the registry resolves the provider name to a callable instance.
"""

import logging
from typing import Dict, List, Optional
from apps.api.services.leadgen.enrichment.provider import EnrichmentProvider, EnrichmentResult

logger = logging.getLogger("workbook.providers")


# ── Global Registry ──────────────────────────────────────────────────────

_registry: Dict[str, EnrichmentProvider] = {}


def register_provider(provider: EnrichmentProvider):
    """Register a provider instance in the global registry."""
    _registry[provider.name] = provider
    logger.info(f"Registered provider: {provider.name} (capabilities: {provider.capabilities})")


def get_provider(name: str) -> Optional[EnrichmentProvider]:
    """Get a provider by name. Returns None if not found."""
    return _registry.get(name)


def list_providers() -> List[dict]:
    """List all registered providers with their metadata."""
    return [
        {
            "name": p.name,
            "capabilities": p.capabilities,
            "confidence": p.default_confidence,
        }
        for p in _registry.values()
    ]


def get_providers_for_capability(capability: str) -> List[EnrichmentProvider]:
    """Get all providers that can provide a specific field."""
    return [p for p in _registry.values() if p.can_provide(capability)]


# ── Auto-register existing providers ──────────────────────────────────────

def _init_providers():
    """Initialize and register all built-in providers."""
    try:
        from apps.api.services.leadgen.enrichment.providers.mailscout_verify import MailScoutVerifyProvider
        register_provider(MailScoutVerifyProvider())
    except Exception as e:
        logger.warning(f"Failed to register mailscout: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.crosslinked import CrossLinkedProvider
        register_provider(CrossLinkedProvider())
    except Exception as e:
        logger.warning(f"Failed to register crosslinked: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.facebook_pages import FacebookPageProvider
        register_provider(FacebookPageProvider())
    except Exception as e:
        logger.warning(f"Failed to register facebook_pages: {e}")

    try:
        from apps.api.services.leadgen.enrichment.providers.jobspy_signals import JobSpySignalProvider
        register_provider(JobSpySignalProvider())
    except Exception as e:
        logger.warning(f"Failed to register jobspy_signals: {e}")

    # Note: search_enricher.py and website_scraper.py exist but don't expose
    # EnrichmentProvider subclasses yet. They can be wrapped later.

    logger.info(f"Provider registry initialized: {len(_registry)} providers loaded")


# Auto-initialize on import
_init_providers()
